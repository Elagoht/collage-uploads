package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Elagoht/collage/pkg/collage"
)

const (
	svgXML  = `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`
	svgBare = `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`
	htmlDoc = `<!DOCTYPE html><html><body><script>alert(document.cookie)</script></body></html>`
)

// putFile stores data under key in a fresh Dir and returns the plugin.
func putFile(t *testing.T, key string, data []byte) *Plugin {
	t.Helper()
	store := Dir(t.TempDir())
	if _, err := store.Put(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return New(store)
}

func get(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// wantSafe checks the headers every served file carries.
func wantSafe(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("Content-Security-Policy = %q, want sandbox", got)
	}
}

func wantAttachment(t *testing.T, w *httptest.ResponseRecorder, key string) {
	t.Helper()
	want := `attachment; filename="` + key + `"`
	if got := w.Header().Get("Content-Disposition"); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
}

func TestHandler_ImageInline(t *testing.T) {
	data := pngBytes(t)
	key := strings.Repeat("a", 32) + ".png"
	h := putFile(t, key, data).Handler()

	w := get(h, http.MethodGet, "/uploads/"+key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	wantSafe(t, w)
	if got := w.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want none", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=0" {
		t.Errorf("Cache-Control = %q, want the default", got)
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Error("the body is not the stored file")
	}
}

func TestHandler_RasterTypesInline(t *testing.T) {
	for name, data := range map[string][]byte{
		"jpeg": append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 20)...),
		"gif":  []byte("GIF89a" + strings.Repeat("\x00", 20)),
		"webp": []byte("RIFF\x00\x00\x00\x00WEBPVP8 " + strings.Repeat("\x00", 20)),
	} {
		t.Run(name, func(t *testing.T) {
			key := name + ".bin"
			w := get(putFile(t, key, data).Handler(), http.MethodGet, "/u/"+key, nil)
			if got := w.Header().Get("Content-Type"); got != "image/"+name {
				t.Errorf("Content-Type = %q, want image/%s", got, name)
			}
			if got := w.Header().Get("Content-Disposition"); got != "" {
				t.Errorf("Content-Disposition = %q, want none", got)
			}
			wantSafe(t, w)
		})
	}
}

// Review Focus 1: SVG is never served inline as image/svg+xml, whatever its
// key claims.
func TestHandler_SVGIsAttachment(t *testing.T) {
	for name, svg := range map[string]string{"xml prolog": svgXML, "bare": svgBare} {
		t.Run(name, func(t *testing.T) {
			key := "evil.png"
			w := get(putFile(t, key, []byte(svg)).Handler(), http.MethodGet, "/uploads/"+key, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			ct := w.Header().Get("Content-Type")
			if strings.Contains(ct, "svg") || strings.HasPrefix(ct, "image/") {
				t.Errorf("Content-Type = %q, want a non-image sniffed type", ct)
			}
			wantAttachment(t, w, key)
			wantSafe(t, w)
		})
	}
}

func TestHandler_HTMLIsAttachment(t *testing.T) {
	key := "page.png"
	w := get(putFile(t, key, []byte(htmlDoc)).Handler(), http.MethodGet, "/uploads/"+key, nil)
	if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the sniffed text/html", got)
	}
	wantAttachment(t, w, key)
	wantSafe(t, w)
	if w.Body.String() != htmlDoc {
		t.Error("the body is not the stored file")
	}
}

// A PNG signature followed by HTML sniffs as a PNG and is inline, but still
// sandboxed and nosniff, so a browser never runs it as a document.
func TestHandler_PolyglotStaysSandboxed(t *testing.T) {
	key := "poly.png"
	data := append([]byte("\x89PNG\r\n\x1a\n"), []byte(htmlDoc)...)
	w := get(putFile(t, key, data).Handler(), http.MethodGet, "/uploads/"+key, nil)
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	wantSafe(t, w)
}

func TestHandler_TextAndEmptyAreAttachments(t *testing.T) {
	for _, tc := range []struct {
		key, data, ct string
	}{
		{"note.txt", "hello", "text/plain; charset=utf-8"},
		{"empty.bin", "", "application/octet-stream"},
		{"doc.pdf", "%PDF-1.7\n", "application/pdf"},
	} {
		w := get(putFile(t, tc.key, []byte(tc.data)).Handler(), http.MethodGet, "/uploads/"+tc.key, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d", tc.key, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.ct {
			t.Errorf("%s: Content-Type = %q, want %q", tc.key, got, tc.ct)
		}
		wantAttachment(t, w, tc.key)
		wantSafe(t, w)
		if w.Body.String() != tc.data {
			t.Errorf("%s: body = %q", tc.key, w.Body.String())
		}
	}
}

// A body longer than the sniff window comes back whole: the sniffed head and
// the rest.
func TestHandler_LongBodyIntact(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 1000))
	w := get(putFile(t, "long.txt", data).Handler(), http.MethodGet, "/uploads/long.txt", nil)
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Errorf("body has %d bytes, want %d", w.Body.Len(), len(data))
	}
}

func TestHandler_NotFoundAndBadKey(t *testing.T) {
	dir := t.TempDir()
	store := Dir(filepath.Join(dir, "store"))
	key := strings.Repeat("b", 32) + ".txt"
	if _, err := store.Put(context.Background(), key, strings.NewReader("in the store")); err != nil {
		t.Fatal(err)
	}
	// A file next to the store, which traversal would reach.
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "store", "Upper.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(store).Handler()

	for _, target := range []string{
		"/uploads/" + strings.Repeat("c", 32) + ".txt", // missing
		"/uploads/",                        // no key
		"/uploads/..%2f",                   // decodes to "../": empty last segment
		"/uploads/..%2fsecret",             // decodes to "../secret"
		"/uploads/..%2f..%2fsecret",        // decodes to "../../secret"
		"/uploads/%2e%2e",                  // ".."
		"/uploads/a%5cb",                   // "a\b"
		"/uploads/..%5csecret",             // "..\secret"
		"/uploads/" + strings.ToUpper(key), // uppercase: refused, even on a case-insensitive disk
		"/uploads/Upper.txt",               // a real file, but not a valid key
		"/uploads/.upload-123",             // a temp file's name
		"/uploads/nul.png",                 // Windows device name
		"/uploads/a%00b",                   // control character
	} {
		w := get(h, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
		if strings.Contains(w.Body.String(), "outside") || strings.Contains(w.Body.String(), "in the store") {
			t.Errorf("%s: served a file: %q", target, w.Body.String())
		}
		if got := w.Header().Get("Content-Disposition"); got != "" {
			t.Errorf("%s: a 404 carries Content-Disposition %q", target, got)
		}
	}

	if w := get(h, http.MethodGet, "/uploads/"+key, nil); w.Code != http.StatusOK || w.Body.String() != "in the store" {
		t.Errorf("the valid key: status %d, body %q", w.Code, w.Body.String())
	}
}

// A directory or a symlink under a key is not found.
func TestHandler_DirAndSymlinkNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	h := New(Dir(dir)).Handler()
	for _, key := range []string{"sub", "link.txt"} {
		if w := get(h, http.MethodGet, "/uploads/"+key, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", key, w.Code)
		}
	}
}

// A store directory that was never created is a 404, not a 500.
func TestHandler_MissingStoreDir(t *testing.T) {
	h := New(Dir(filepath.Join(t.TempDir(), "nope"))).Handler()
	if w := get(h, http.MethodGet, "/uploads/abc.png", nil); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandler_Methods(t *testing.T) {
	key := "a.txt"
	h := putFile(t, key, []byte("hello")).Handler()

	w := get(h, http.MethodHead, "/uploads/"+key, nil)
	if w.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD wrote a body: %q", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("HEAD Content-Type = %q", got)
	}
	wantAttachment(t, w, key)
	wantSafe(t, w)

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		w := get(h, m, "/uploads/"+key, nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", m, w.Code)
		}
		if got := w.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s Allow = %q, want GET, HEAD", m, got)
		}
	}
}

func TestHandler_CacheControlOption(t *testing.T) {
	p := putFile(t, "a.txt", []byte("hello"))
	w := get(p.Handler(HandlerOptions{CacheControl: "public, max-age=31536000, immutable"}), http.MethodGet, "/uploads/a.txt", nil)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	// An empty option keeps the default.
	w = get(p.Handler(HandlerOptions{}), http.MethodGet, "/uploads/a.txt", nil)
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=0" {
		t.Errorf("empty option: Cache-Control = %q, want the default", got)
	}
}

// A range of a stored HTML file is still typed by its head, sandboxed and an
// attachment.
func TestHandler_RangeKeepsHeaders(t *testing.T) {
	key := "page.bin"
	h := putFile(t, key, []byte(htmlDoc)).Handler()
	w := get(h, http.MethodGet, "/uploads/"+key, http.Header{"Range": {"bytes=20-"}})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if w.Body.String() != htmlDoc[20:] {
		t.Errorf("body = %q, want %q", w.Body.String(), htmlDoc[20:])
	}
	if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the head's sniffed type", got)
	}
	wantAttachment(t, w, key)
	wantSafe(t, w)
}

// streamOnly hides everything but Read and Close, as a remote store's body
// would.
type streamOnly struct{ io.ReadCloser }

type streamStore struct{ Store }

func (s streamStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.Store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	return streamOnly{rc}, nil
}

func TestHandler_NonSeekableStore(t *testing.T) {
	store := Dir(t.TempDir())
	key := "page.bin"
	data := htmlDoc + strings.Repeat("x", 2000)
	if _, err := store.Put(context.Background(), key, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	h := New(streamStore{store}).Handler()

	w := get(h, http.MethodGet, "/uploads/"+key, http.Header{"Range": {"bytes=20-"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Range ignored)", w.Code)
	}
	if w.Body.String() != data {
		t.Error("the body is not the whole stored file")
	}
	if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	wantAttachment(t, w, key)
	wantSafe(t, w)

	w = get(h, http.MethodHead, "/uploads/"+key, nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: status %d, body %d bytes", w.Code, w.Body.Len())
	}
	wantSafe(t, w)
}

type brokenStore struct{ Store }

func (brokenStore) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("disk on fire")
}

type failingRead struct{}

func (failingRead) Read([]byte) (int, error) { return 0, errors.New("read on fire") }
func (failingRead) Close() error             { return nil }

type badReadStore struct{ Store }

func (badReadStore) Open(context.Context, string) (io.ReadCloser, error) {
	return failingRead{}, nil
}

// A store error that is not "not found" is a 500, logged through the logger
// the host gave Init, and the response carries no file headers. Mounted the
// way the README shows.
func TestHandler_StoreErrorIs500AndLogged(t *testing.T) {
	for name, store := range map[string]Store{
		"open": brokenStore{Dir(t.TempDir())},
		"read": badReadStore{Dir(t.TempDir())},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			p := New(store)
			a, err := collage.New(&collage.Config{
				Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
				Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>x</p>`)}}, Root: "t"},
				Plugins:  []collage.Plugin{p},
				Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Handle("/uploads/", p.Handler()); err != nil {
				t.Fatal(err)
			}
			if err := a.Start(); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/uploads/abc.png", nil)
			r.Host = "localhost:3000"
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			if got := w.Header().Get("Content-Disposition"); got != "" {
				t.Errorf("a 500 carries Content-Disposition %q", got)
			}
			if !strings.Contains(logs.String(), "on fire") {
				t.Errorf("the store error was not logged: %q", logs.String())
			}
			if strings.Contains(w.Body.String(), "on fire") {
				t.Errorf("the store error reached the client: %q", w.Body.String())
			}
		})
	}
}

// Without Init, the handler still answers, logging through slog.Default.
func TestHandler_WithoutInit(t *testing.T) {
	w := get(New(brokenStore{}).Handler(), http.MethodGet, "/uploads/abc.png", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// Before Init built a store, there is nothing to serve.
func TestHandler_NoStore(t *testing.T) {
	w := get(NewWith(Config{}).Handler(), http.MethodGet, "/uploads/abc.png", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
