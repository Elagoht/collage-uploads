package uploads

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// e2eServer is an application listening on a real port, in production mode,
// with CSRF on and one streaming action at /upload.
type e2eServer struct {
	base  string // http://127.0.0.1:port
	addr  string // 127.0.0.1:port
	store string // the Dir store's directory
	// received gets Receive's error each time the action's handler runs it.
	received chan error
	// cookie is the CSRF cookie the form page set; its value is the token.
	cookie *http.Cookie
	client *http.Client
	// logs is what the application logged.
	logs *lockedBuffer
}

// lockedBuffer is a log destination the server's goroutines write to while a
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startE2E starts the application with the action's body bounded at limit,
// and its file field allowed maxFiles files of up to 150 MB each. It runs in
// production mode unless devMode is set. What the application logs is shown
// when the test fails.
func startE2E(t *testing.T, limit int64, maxFiles int, devMode bool) *e2eServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	s := &e2eServer{
		base:     "http://" + addr,
		addr:     addr,
		store:    filepath.Join(t.TempDir(), "store"),
		received: make(chan error, 4),
		logs:     &lockedBuffer{},
	}
	u := New(Options{Store: Dir(s.store)})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the application logged:\n%s", s.logs)
		}
	})
	app, err := collage.New(&collage.Config{
		DevMode: devMode,
		// The server's default timeouts, as a deployment has them: the
		// upload action raises its own with WithBodyTimeout.
		Server:   collage.ServerConfig{Host: host, Port: port},
		Security: collage.SecurityConfig{CSRFKey: bytes.Repeat([]byte("k"), 32)},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>x</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{u},
		Logger:   slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	form := collage.NewInlineFragment("form", `<form method="post">{{csrfToken}}</form>`).Build()
	if err := app.RegisterPage(collage.NewPage("form").WithContent(form).WithPath("en", "/form").Dynamic().Build()); err != nil {
		t.Fatal(err)
	}
	action := collage.NewAction("upload").WithPath("en", "/upload").WithMethods(http.MethodPost).
		WithStreamingBody().WithMaxBodyBytes(limit).
		// A 100 MB body under the race detector takes longer than the
		// server's 15s ReadTimeout and 30s WriteTimeout allow.
		WithBodyTimeout(5 * time.Minute).
		WithHandler(func(_ context.Context, rc *collage.RenderContext) (*collage.ActionResult, error) {
			got, err := u.Receive(rc, Field("file", Rules{MaxSize: 150 << 20, MaxFiles: maxFiles}))
			s.received <- err
			var rule *Error
			switch {
			case errors.As(err, &rule):
				return &collage.ActionResult{Status: http.StatusUnprocessableEntity, Body: []byte(err.Error()), ContentType: "text/plain"}, nil
			case err != nil:
				return nil, err
			}
			var keys bytes.Buffer
			for _, f := range got.Files["file"] {
				fmt.Fprintln(&keys, f.Key)
			}
			return &collage.ActionResult{Status: http.StatusOK, Body: keys.Bytes(), ContentType: "text/plain"}, nil
		}).Build()
	if err := app.RegisterAction(action); err != nil {
		t.Fatal(err)
	}

	served := make(chan error, 1)
	go func() { served <- app.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("ListenAndServe: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ListenAndServe did not return after Shutdown")
		}
	})
	ready := false
	for range 200 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("the server never listened")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.client = &http.Client{Jar: jar, Timeout: 5 * time.Minute}
	resp, err := s.client.Get(s.base + "/form")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /form: %d", resp.StatusCode)
	}
	baseURL, _ := url.Parse(s.base)
	cookies := jar.Cookies(baseURL)
	if len(cookies) != 1 || cookies[0].Value == "" {
		t.Fatalf("the form page set cookies %v, want the one CSRF cookie", cookies)
	}
	s.cookie = cookies[0]
	return s
}

// post sends body to /upload with the token in the header (the jar sends the
// cookie). With no Content-Length the body goes out chunked, so nothing ahead
// of the handler can refuse it by its declared size.
func (s *e2eServer) post(t *testing.T, body io.Reader, contentType string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.base+"/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", s.cookie.Value)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitReceived waits for the handler to report Receive's result.
func (s *e2eServer) waitReceived(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-s.received:
		return err
	case <-time.After(within):
		t.Fatalf("the handler did not return from Receive within %v", within)
		return nil
	}
}

// storeEntries lists everything in the store's directory, temp files
// included; a directory never created holds nothing.
func storeEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// payload is n deterministic pseudo-random bytes, generated as they are read
// and never held whole.
func payload(n int64) io.Reader {
	return io.LimitReader(rand.NewChaCha8([32]byte{'u', 'p', 'l', 'o', 'a', 'd', 's'}), n)
}

// streamMultipart writes a multipart body to a pipe as it is read: first each
// small file in full, then one file of big's bytes. It returns the body, its
// Content-Type, and a function that waits for the writer and returns the
// SHA-256 of the big file as sent.
func streamMultipart(small [][]byte, big io.Reader) (io.Reader, string, func() (string, error)) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan struct{})
	var sum string
	var werr error
	go func() {
		defer close(done)
		werr = func() error {
			for i, data := range small {
				part, err := mw.CreateFormFile("file", fmt.Sprintf("small-%d.txt", i))
				if err != nil {
					return err
				}
				if _, err := part.Write(data); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("file", "big.bin")
			if err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(part, h), big); err != nil {
				return err
			}
			sum = hex.EncodeToString(h.Sum(nil))
			return mw.Close()
		}()
		pw.CloseWithError(werr)
	}()
	return pr, mw.FormDataContentType(), func() (string, error) {
		<-done
		return sum, werr
	}
}

// A 100 MB file streamed through the action is stored intact, and the heap
// does not grow with it: Receive holds a window of it, never the whole.
func TestE2E_StreamingMemoryFlat(t *testing.T) {
	const size = 100 << 20
	const maxGrowth = 32 << 20
	s := startE2E(t, 200<<20, 1, false)

	body, contentType, sent := streamMultipart(nil, payload(size))

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	baseline := ms.HeapInuse
	var peak atomic.Uint64
	peak.Store(baseline)
	stop := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Go(func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	})

	status, respBody := s.post(t, body, contentType)
	close(stop)
	sampling.Wait()
	sum, werr := sent()
	if werr != nil {
		t.Fatalf("writing the body: %v", werr)
	}
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", status, respBody)
	}
	if err := s.waitReceived(t, 2*time.Second); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	growth := int64(peak.Load()) - int64(baseline)
	t.Logf("HeapInuse baseline %.1f MB, peak growth %.2f MB over a %d MB upload",
		float64(baseline)/(1<<20), float64(growth)/(1<<20), size>>20)
	if growth >= maxGrowth {
		t.Errorf("the heap grew %d bytes during the upload, want under %d", growth, maxGrowth)
	}

	key := respBody[:len(respBody)-1]
	if !validKey(key) {
		t.Fatalf("the action answered %q, not a key", respBody)
	}
	f, err := os.Open(filepath.Join(s.store, key))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	if n != size {
		t.Errorf("stored %d bytes, want %d", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		t.Errorf("stored SHA-256 %s, sent %s", got, sum)
	}
	if got := storeEntries(t, s.store); len(got) != 1 || got[0] != key {
		t.Errorf("store holds %v, want only %s", got, key)
	}
}

// A body that runs into the action's limit is a 413, and leaves nothing: the
// file stored before the limit was reached is deleted with the one cut off.
func TestE2E_BodyLimitLeavesNothing(t *testing.T) {
	s := startE2E(t, 1<<20, 2, false)

	body, contentType, sent := streamMultipart([][]byte{bytes.Repeat([]byte("first file\n"), 4<<10)}, payload(4<<20))
	status, respBody := s.post(t, body, contentType)
	// The server stops reading at the limit; the writer then fails, which is
	// the point.
	_, _ = sent()
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", status, respBody)
	}
	err := s.waitReceived(t, 2*time.Second)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Receive returned %v, want a *http.MaxBytesError: the 413 must come from Receive reading past the limit", err)
	}
	if got := storeEntries(t, s.store); len(got) != 0 {
		t.Errorf("the store holds %v after a refused upload, want nothing", got)
	}
}

// A client that goes away mid-file leaves nothing: the file it finished
// before is deleted, and the one cut off never lands.
func TestE2E_DisconnectLeavesNothing(t *testing.T) {
	s := startE2E(t, 200<<20, 2, false)

	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	part, err := mw.CreateFormFile("file", "first.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(bytes.Repeat([]byte("first file\n"), 4<<10))
	if _, err := mw.CreateFormFile("file", "second.bin"); err != nil {
		t.Fatal(err)
	}

	w := bufio.NewWriter(conn)
	fmt.Fprintf(w, "POST /upload HTTP/1.1\r\n")
	fmt.Fprintf(w, "Host: %s\r\n", s.addr)
	fmt.Fprintf(w, "Content-Type: %s\r\n", mw.FormDataContentType())
	// A length well past what is sent, and under the limit.
	fmt.Fprintf(w, "Content-Length: %d\r\n", 50<<20)
	fmt.Fprintf(w, "Cookie: %s=%s\r\n", s.cookie.Name, s.cookie.Value)
	fmt.Fprintf(w, "X-CSRF-Token: %s\r\n\r\n", s.cookie.Value)
	if _, err := w.Write(head.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, payload(2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	// The first file is stored before the connection goes, so its deletion
	// is part of what is checked.
	stored := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, name := range storeEntries(t, s.store) {
			if validKey(name) {
				stored = true
			}
		}
		if stored {
			break
		}
	}
	if !stored {
		t.Fatal("the first file was never stored")
	}
	conn.Close()

	if err := s.waitReceived(t, 2*time.Second); err == nil {
		t.Fatal("Receive succeeded on a body cut off mid-file")
	}
	if got := storeEntries(t, s.store); len(got) != 0 {
		t.Errorf("the store holds %v after a disconnect, want nothing", got)
	}
	// A cancelled upload is the client's doing: logged at debug, never as
	// the application's error.
	logged := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if strings.Contains(s.logs.String(), `level=DEBUG msg="collage: request failed"`) {
			logged = true
			break
		}
	}
	if !logged {
		t.Error("the disconnect was not logged at debug")
	}
	if strings.Contains(s.logs.String(), "level=ERROR") {
		t.Error("the disconnect was logged at ERROR")
	}
}

// A plain form carrying the token as a field is refused with 403, and
// nothing is read or stored. Production answers a bare "Forbidden" and does
// not log a 4xx, so the refusal's reason, which names the header the token
// belongs in, is checked where it is shown: in development mode.
func TestE2E_FormTokenRefused(t *testing.T) {
	for _, mode := range []struct {
		name    string
		devMode bool
	}{{"production", false}, {"development", true}} {
		devMode := mode.devMode
		t.Run(mode.name, func(t *testing.T) {
			s := startE2E(t, 200<<20, 1, devMode)

			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if err := mw.WriteField("_csrf", s.cookie.Value); err != nil {
				t.Fatal(err)
			}
			part, err := mw.CreateFormFile("file", "a.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write([]byte("file contents"))
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}

			req, err := http.NewRequest(http.MethodPost, s.base+"/upload", &body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp, err := s.client.Do(req) // the jar sends the cookie
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", resp.StatusCode, got)
			}
			if devMode && !bytes.Contains(got, []byte("must be sent in the X-CSRF-Token header")) {
				t.Errorf("the refusal does not name the header: %s", got)
			}
			select {
			case err := <-s.received:
				t.Errorf("the handler ran Receive (%v) for a refused submission", err)
			default:
			}
			if got := storeEntries(t, s.store); len(got) != 0 {
				t.Errorf("the store holds %v, want nothing", got)
			}
		})
	}
}
