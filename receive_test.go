package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"

	"github.com/Elagoht/collage/pkg/collage"
)

// part is one part of a multipart body. An empty filename makes it a plain
// value; otherwise it is a file sent with ctype as its Content-Type.
type part struct {
	field    string
	filename string
	ctype    string
	data     []byte
}

func value(field, v string) part { return part{field: field, data: []byte(v)} }

func file(field, filename string, data []byte) part {
	return part{field: field, filename: filename, ctype: "application/octet-stream", data: data}
}

// multipartBody encodes parts and returns the body and its Content-Type.
func multipartBody(t *testing.T, parts ...part) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		var err error
		var dst io.Writer
		if p.filename == "" {
			dst, err = w.CreateFormField(p.field)
		} else {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename))
			h.Set("Content-Type", p.ctype)
			dst, err = w.CreatePart(h)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dst.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

// pngBytes is a real PNG image.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// app builds a collage application with p registered and one POST action at
// /upload whose handler runs inside. The action streams its body unless
// parsed is set; with parsed, the handler parses the multipart form itself
// before inside runs, as an application on the ordinary path would.
func app(t *testing.T, p *Plugin, parsed bool, pluginConfig map[string]json.RawMessage, inside func(rc *collage.RenderContext)) (http.Handler, error) {
	t.Helper()
	a, err := collage.New(&collage.Config{
		Server:       collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:     collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>x</p>`)}}, Root: "t"},
		Plugins:      []collage.Plugin{p},
		PluginConfig: pluginConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := collage.NewAction("upload").WithPath("en", "/upload").WithMethods(http.MethodPost).WithoutCSRF()
	if !parsed {
		b = b.WithStreamingBody()
	}
	b = b.WithHandler(func(_ context.Context, rc *collage.RenderContext) (*collage.ActionResult, error) {
		if parsed {
			if err := rc.Request.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				t.Errorf("ParseMultipartForm: %v", err)
				return nil, err
			}
			if rc.Request.MultipartForm == nil && rc.Request.PostForm == nil {
				t.Error("the parsed path left the form unparsed")
			}
		} else if rc.Request.MultipartForm != nil || rc.Request.Form != nil {
			t.Error("the stream path reached the handler with a parsed form")
		}
		inside(rc)
		return nil, nil
	})
	if err := a.RegisterAction(b.Build()); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		return nil, err
	}
	return a.Handler(), nil
}

// post sends body to /upload on h.
func post(h http.Handler, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/upload", body)
	r.Host = "localhost:3000"
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// receive runs one Receive with specs over body, on the path parsed picks.
func receive(t *testing.T, p *Plugin, parsed bool, body io.Reader, contentType string, specs ...FieldSpec) (*Received, error) {
	t.Helper()
	var got *Received
	var err error
	ran := false
	h, startErr := app(t, p, parsed, nil, func(rc *collage.RenderContext) {
		ran = true
		got, err = p.Receive(rc, specs...)
	})
	if startErr != nil {
		t.Fatal(startErr)
	}
	post(h, body, contentType)
	if !ran {
		t.Fatal("the action's handler did not run")
	}
	return got, err
}

// paths is every way Receive is reached: a form the application already
// parsed, and a body nobody has read.
var paths = []struct {
	name   string
	parsed bool
}{
	{"parsed", true},
	{"stream", false},
}

// stored lists every entry in dir, temp files included. A directory that was
// never created holds nothing.
func stored(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
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

func newDirPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	return New(Dir(dir)), dir
}

// wantRule checks err is an *Error for field wrapping rule.
func wantRule(t *testing.T, err error, field string, rule error) {
	t.Helper()
	if !errors.Is(err, rule) {
		t.Fatalf("err = %v, want %v", err, rule)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %T %v, want *Error", err, err)
	}
	if ue.Field != field {
		t.Errorf("Error.Field = %q, want %q", ue.Field, field)
	}
}

func TestReceive_StoresAndReports(t *testing.T) {
	img := pngBytes(t)
	sum := sha256.Sum256(img)
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, dir := newDirPlugin(t)
			body, ct := multipartBody(t, file("avatar", `..\..\me.png`, img))
			got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct,
				Field("avatar", Rules{MaxSize: 1 << 20, Types: []string{"image/png"}}))
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			files := got.Files["avatar"]
			if len(files) != 1 {
				t.Fatalf("got %d files, want 1", len(files))
			}
			f := files[0]
			if !validKey(f.Key) || !strings.HasSuffix(f.Key, ".png") || len(f.Key) != 32+len(".png") {
				t.Errorf("Key = %q, want 32 hex chars and .png", f.Key)
			}
			if f.Name != "me.png" {
				t.Errorf("Name = %q, want me.png", f.Name)
			}
			if f.Type != "image/png" {
				t.Errorf("Type = %q, want image/png", f.Type)
			}
			if f.Size != int64(len(img)) {
				t.Errorf("Size = %d, want %d", f.Size, len(img))
			}
			if f.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("SHA256 = %s, want %x", f.SHA256, sum)
			}
			if readAll(t, p.Store(), f.Key) != string(img) {
				t.Error("the stored bytes differ from the bytes sent")
			}
			if names := stored(t, dir); len(names) != 1 {
				t.Errorf("store holds %v, want one file", names)
			}
		})
	}
}

// Review Focus 1: the client's name and Content-Type never decide the type.
func TestReceive_SniffTraps(t *testing.T) {
	html := []byte("<html><body><script>alert(1)</script></body></html>")
	svgs := map[string][]byte{
		"with xml declaration": []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"bare":                 []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`),
	}
	lying := func(data []byte) part {
		return part{field: "pic", filename: "x.png", ctype: "image/png", data: data}
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			t.Run("html refused by an image rule", func(t *testing.T) {
				p, dir := newDirPlugin(t)
				body, ct := multipartBody(t, lying(html))
				_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct,
					Field("pic", Rules{Types: []string{"image/png"}}))
				wantRule(t, err, "pic", ErrType)
				if names := stored(t, dir); len(names) != 0 {
					t.Errorf("store holds %v after a refused type", names)
				}
			})
			t.Run("html stored as html when any type is allowed", func(t *testing.T) {
				p, _ := newDirPlugin(t)
				body, ct := multipartBody(t, lying(html))
				got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("pic", Rules{}))
				if err != nil {
					t.Fatalf("Receive: %v", err)
				}
				f := got.Files["pic"][0]
				if f.Type != "text/html; charset=utf-8" {
					t.Errorf("Type = %q, want text/html; charset=utf-8", f.Type)
				}
				if strings.HasSuffix(f.Key, ".png") || !(strings.HasSuffix(f.Key, ".bin") || strings.HasSuffix(f.Key, ".txt")) {
					t.Errorf("Key = %q, want .bin or .txt, never .png", f.Key)
				}
			})
			for name, svg := range svgs {
				t.Run("svg "+name, func(t *testing.T) {
					p, _ := newDirPlugin(t)
					body, ct := multipartBody(t, lying(svg))
					got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("pic", Rules{}))
					if err != nil {
						t.Fatalf("Receive: %v", err)
					}
					f := got.Files["pic"][0]
					media, _, _ := strings.Cut(f.Type, ";")
					if media != "text/xml" && media != "text/plain" {
						t.Errorf("Type = %q, want text/xml or text/plain", f.Type)
					}
					if strings.HasSuffix(f.Key, ".png") {
						t.Errorf("Key = %q ends in .png", f.Key)
					}

					p2, _ := newDirPlugin(t)
					_, err = receive(t, p2, path.parsed, bytes.NewReader(body), ct,
						Field("pic", Rules{Types: []string{"image/png"}}))
					wantRule(t, err, "pic", ErrType)
				})
			}
		})
	}
}

// Review Focus 3: exactly MaxSize passes, one more byte is refused and leaves
// nothing.
func TestReceive_MaxSizeBoundary(t *testing.T) {
	for _, path := range paths {
		for _, max := range []int64{100, 512, 1000, 70000} {
			t.Run(fmt.Sprintf("%s/%d", path.name, max), func(t *testing.T) {
				rules := Rules{MaxSize: max}

				p, dir := newDirPlugin(t)
				exact := bytes.Repeat([]byte("a"), int(max))
				body, ct := multipartBody(t, file("f", "a.txt", exact))
				got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", rules))
				if err != nil {
					t.Fatalf("exactly MaxSize: %v", err)
				}
				if got.Files["f"][0].Size != max {
					t.Errorf("Size = %d, want %d", got.Files["f"][0].Size, max)
				}
				if names := stored(t, dir); len(names) != 1 {
					t.Errorf("store holds %v, want one file", names)
				}

				p, dir = newDirPlugin(t)
				over := bytes.Repeat([]byte("a"), int(max)+1)
				body, ct = multipartBody(t, file("f", "a.txt", over))
				_, err = receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", rules))
				wantRule(t, err, "f", ErrTooLarge)
				if names := stored(t, dir); len(names) != 0 {
					t.Errorf("store holds %v after MaxSize+1", names)
				}
			})
		}
	}
}

func TestReceive_TooMany(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			t.Run("MaxFiles 2", func(t *testing.T) {
				p, dir := newDirPlugin(t)
				body, ct := multipartBody(t,
					file("f", "1.txt", []byte("one")),
					file("f", "2.txt", []byte("two")),
					file("f", "3.txt", []byte("three")))
				_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", Rules{MaxFiles: 2}))
				wantRule(t, err, "f", ErrTooMany)
				if names := stored(t, dir); len(names) != 0 {
					t.Errorf("store holds %v", names)
				}
			})
			t.Run("default is one", func(t *testing.T) {
				p, dir := newDirPlugin(t)
				body, ct := multipartBody(t,
					file("f", "1.txt", []byte("one")),
					file("f", "2.txt", []byte("two")))
				_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", Rules{}))
				wantRule(t, err, "f", ErrTooMany)
				if names := stored(t, dir); len(names) != 0 {
					t.Errorf("store holds %v", names)
				}
			})
			t.Run("exactly MaxFiles passes", func(t *testing.T) {
				p, _ := newDirPlugin(t)
				body, ct := multipartBody(t,
					file("f", "1.txt", []byte("one")),
					file("f", "2.txt", []byte("two")))
				got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", Rules{MaxFiles: 2}))
				if err != nil {
					t.Fatalf("Receive: %v", err)
				}
				if len(got.Files["f"]) != 2 {
					t.Errorf("got %d files, want 2", len(got.Files["f"]))
				}
			})
		})
	}
}

func TestReceive_UnexpectedField(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, dir := newDirPlugin(t)
			body, ct := multipartBody(t,
				file("a", "a.txt", []byte("fine")),
				file("other", "b.txt", []byte("not declared")))
			_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("a", Rules{}))
			wantRule(t, err, "other", ErrUnexpectedField)
			if names := stored(t, dir); len(names) != 0 {
				t.Errorf("store holds %v", names)
			}
		})
	}
}

func TestReceive_AllOrNothing(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, dir := newDirPlugin(t)
			body, ct := multipartBody(t,
				file("a", "a.png", pngBytes(t)),
				file("b", "b.png", []byte("<html>not an image</html>")))
			_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct,
				Field("a", Rules{Types: []string{"image/png"}}),
				Field("b", Rules{Types: []string{"image/png"}}))
			wantRule(t, err, "b", ErrType)
			if names := stored(t, dir); len(names) != 0 {
				t.Errorf("store holds %v after a failed Receive", names)
			}
		})
	}
}

func TestReceive_Values(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			t.Run("before and after the files", func(t *testing.T) {
				p, _ := newDirPlugin(t)
				body, ct := multipartBody(t,
					value("title", "Hello"),
					value("tag", "one"),
					file("f", "a.txt", []byte("file")),
					value("tag", "two"),
					value("note", ""))
				got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", Rules{}))
				if err != nil {
					t.Fatalf("Receive: %v", err)
				}
				if got.Values.Get("title") != "Hello" {
					t.Errorf("title = %q", got.Values.Get("title"))
				}
				if tags := got.Values["tag"]; len(tags) != 2 || tags[0] != "one" || tags[1] != "two" {
					t.Errorf("tag = %v, want [one two]", tags)
				}
				if v, ok := got.Values["note"]; !ok || len(v) != 1 || v[0] != "" {
					t.Errorf("note = %v, want one empty value", v)
				}
				if len(got.Files["f"]) != 1 {
					t.Errorf("got %d files, want 1", len(got.Files["f"]))
				}
			})
			t.Run("exactly 64 KiB passes", func(t *testing.T) {
				p, _ := newDirPlugin(t)
				body, ct := multipartBody(t,
					value("a", strings.Repeat("x", 32<<10)),
					value("b", strings.Repeat("y", 32<<10)))
				if _, err := receive(t, p, path.parsed, bytes.NewReader(body), ct); err != nil {
					t.Fatalf("Receive: %v", err)
				}
			})
			t.Run("64 KiB + 1 is too large", func(t *testing.T) {
				p, dir := newDirPlugin(t)
				body, ct := multipartBody(t,
					file("f", "a.txt", []byte("stored first")),
					value("a", strings.Repeat("x", 32<<10)),
					value("b", strings.Repeat("y", 32<<10+1)))
				_, err := receive(t, p, path.parsed, bytes.NewReader(body), ct, Field("f", Rules{}))
				wantRule(t, err, "b", ErrTooLarge)
				if names := stored(t, dir); len(names) != 0 {
					t.Errorf("store holds %v", names)
				}
			})
		})
	}
}

func TestReceive_ShortAndEmpty(t *testing.T) {
	short := []byte("hello 1234")
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, _ := newDirPlugin(t)
			body, ct := multipartBody(t,
				file("short", "s.txt", short),
				file("empty", "e.txt", nil))
			got, err := receive(t, p, path.parsed, bytes.NewReader(body), ct,
				Field("short", Rules{}), Field("empty", Rules{}))
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			s := got.Files["short"][0]
			if s.Size != 10 || readAll(t, p.Store(), s.Key) != string(short) {
				t.Errorf("short: Size = %d, content %q", s.Size, readAll(t, p.Store(), s.Key))
			}
			if s.Type != "text/plain; charset=utf-8" {
				t.Errorf("short: Type = %q", s.Type)
			}
			e := got.Files["empty"][0]
			if e.Size != 0 || readAll(t, p.Store(), e.Key) != "" {
				t.Errorf("empty: Size = %d", e.Size)
			}
			if e.Type != "application/octet-stream" || !strings.HasSuffix(e.Key, ".bin") {
				t.Errorf("empty: Type = %q, Key = %q", e.Type, e.Key)
			}
			emptySum := sha256.Sum256(nil)
			if e.SHA256 != hex.EncodeToString(emptySum[:]) {
				t.Errorf("empty: SHA256 = %s", e.SHA256)
			}
		})
	}
}

// failingBody returns data, then err.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// Review Focus 2: a stream that ends mid-file leaves no file and no temp file.
func TestReceive_TruncatedLeavesNothing(t *testing.T) {
	first := bytes.Repeat([]byte("1"), 4000)
	second := bytes.Repeat([]byte("2"), 4000)
	body, ct := multipartBody(t, file("f", "1.txt", first), file("f", "2.txt", second))
	start := bytes.Index(body, second)
	if start < 0 {
		t.Fatal("the second file is not in the body")
	}
	cuts := map[string]int{
		"inside the sniff window": start + 100,
		"past the sniff window":   start + 2000,
	}
	for name, cut := range cuts {
		t.Run(name, func(t *testing.T) {
			p, dir := newDirPlugin(t)
			got, err := receive(t, p, false, &failingBody{data: body[:cut], err: io.ErrUnexpectedEOF}, ct,
				Field("f", Rules{MaxFiles: 2}))
			if err == nil {
				t.Fatalf("Receive succeeded on a truncated body: %+v", got)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("err = %v, want it to wrap io.ErrUnexpectedEOF", err)
			}
			if names := stored(t, dir); len(names) != 0 {
				t.Errorf("store holds %v after a truncated body", names)
			}
		})
	}
}

func TestReceive_AlreadyRead(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, _ := newDirPlugin(t)
			body, ct := multipartBody(t, file("f", "a.txt", []byte("once")))
			var first, second error
			var firstGot *Received
			h, err := app(t, p, path.parsed, nil, func(rc *collage.RenderContext) {
				firstGot, first = p.Receive(rc, Field("f", Rules{}))
				_, second = p.Receive(rc, Field("f", Rules{}))
			})
			if err != nil {
				t.Fatal(err)
			}
			post(h, bytes.NewReader(body), ct)
			if first != nil || len(firstGot.Files["f"]) != 1 {
				t.Fatalf("first Receive: %v", first)
			}
			if !errors.Is(second, ErrAlreadyRead) {
				t.Errorf("second Receive: %v, want ErrAlreadyRead", second)
			}
		})
	}
}

// A first Receive that failed still consumed the body.
func TestReceive_AlreadyReadAfterFailure(t *testing.T) {
	p, _ := newDirPlugin(t)
	body, ct := multipartBody(t, file("other", "a.txt", []byte("x")))
	var second error
	h, err := app(t, p, false, nil, func(rc *collage.RenderContext) {
		if _, err := p.Receive(rc, Field("f", Rules{})); err == nil {
			t.Error("first Receive succeeded")
		}
		_, second = p.Receive(rc, Field("f", Rules{}))
	})
	if err != nil {
		t.Fatal(err)
	}
	post(h, bytes.NewReader(body), ct)
	if !errors.Is(second, ErrAlreadyRead) {
		t.Errorf("second Receive: %v, want ErrAlreadyRead", second)
	}
}

func TestReceive_NotMultipart(t *testing.T) {
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p, _ := newDirPlugin(t)
			got, err := receive(t, p, path.parsed, strings.NewReader("a=1&b=2"), "application/x-www-form-urlencoded",
				Field("f", Rules{}))
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if got.Files == nil || len(got.Files) != 0 || got.Values == nil {
				t.Errorf("got %+v, want empty, non-nil results", got)
			}
			if path.parsed && got.Values.Get("a") != "1" {
				t.Errorf("Values = %v, want the parsed PostForm", got.Values)
			}
		})
	}
}

func TestReceive_MaxBytesErrorWrapped(t *testing.T) {
	p, dir := newDirPlugin(t)
	body, ct := multipartBody(t,
		file("f", "1.txt", []byte("small")),
		file("f", "2.txt", bytes.Repeat([]byte("2"), 8000)))
	var err error
	a, startErr := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>x</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if startErr != nil {
		t.Fatal(startErr)
	}
	act := collage.NewAction("upload").WithPath("en", "/upload").WithMethods(http.MethodPost).
		WithoutCSRF().WithStreamingBody().WithMaxBodyBytes(2000).
		WithHandler(func(_ context.Context, rc *collage.RenderContext) (*collage.ActionResult, error) {
			_, err = p.Receive(rc, Field("f", Rules{MaxFiles: 2}))
			return nil, err
		})
	if err := a.RegisterAction(act.Build()); err != nil {
		t.Fatal(err)
	}
	// No Content-Length, so the bound is hit while Receive reads, not up front.
	w := post(a.Handler(), io.MultiReader(bytes.NewReader(body)), ct)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v, want a wrapped *http.MaxBytesError", err)
	}
	var ue *Error
	if errors.As(err, &ue) {
		t.Errorf("a body over its bound is not a rule error: %v", err)
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	if names := stored(t, dir); len(names) != 0 {
		t.Errorf("store holds %v", names)
	}
}

func TestReceive_NoStore(t *testing.T) {
	p := NewWith(Config{})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	if _, err := p.Receive(&collage.RenderContext{Request: r}); err == nil {
		t.Error("Receive on a plugin with no store succeeded")
	}
}

func TestError(t *testing.T) {
	err := error(&Error{Field: "avatar", Err: ErrTooLarge})
	if !errors.Is(err, ErrTooLarge) {
		t.Error("Error does not unwrap to its rule")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "elagoht/uploads: ") || !strings.Contains(msg, "avatar") {
		t.Errorf("Error() = %q", msg)
	}
	for _, e := range []error{ErrTooLarge, ErrType, ErrTooMany, ErrUnexpectedField, ErrAlreadyRead} {
		if !strings.HasPrefix(e.Error(), "elagoht/uploads: ") {
			t.Errorf("%q lacks the prefix", e.Error())
		}
	}
}

func TestInit_NeedsStore(t *testing.T) {
	cfg := map[string]json.RawMessage{Name: json.RawMessage(`{"dir": ""}`)}
	_, err := app(t, NewWith(Config{}), false, cfg, func(*collage.RenderContext) {})
	if err == nil {
		t.Fatal("Init succeeded with neither a store nor a dir")
	}
}

func TestNew_DirFromConfig(t *testing.T) {
	dirOf := func(t *testing.T, p *Plugin) string {
		t.Helper()
		d, ok := p.Store().(*dirStore)
		if !ok {
			t.Fatalf("Store() = %T, want a Dir store", p.Store())
		}
		return d.path
	}
	t.Run("config section", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "from-config")
		cfg := map[string]json.RawMessage{Name: json.RawMessage(fmt.Sprintf(`{"dir": %q}`, want))}
		p := New(nil)
		if _, err := app(t, p, false, cfg, func(*collage.RenderContext) {}); err != nil {
			t.Fatal(err)
		}
		if got := dirOf(t, p); got != want {
			t.Errorf("dir = %q, want %q", got, want)
		}
	})
	t.Run("NewWith", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "from-code")
		p := NewWith(Config{Dir: want})
		if _, err := app(t, p, false, nil, func(*collage.RenderContext) {}); err != nil {
			t.Fatal(err)
		}
		if got := dirOf(t, p); got != want {
			t.Errorf("dir = %q, want %q", got, want)
		}
	})
	t.Run("default", func(t *testing.T) {
		p := New(nil)
		if _, err := app(t, p, false, nil, func(*collage.RenderContext) {}); err != nil {
			t.Fatal(err)
		}
		if got := dirOf(t, p); got != "var/uploads" {
			t.Errorf("dir = %q, want var/uploads", got)
		}
	})
	t.Run("a given store is kept", func(t *testing.T) {
		s := Dir(t.TempDir())
		cfg := map[string]json.RawMessage{Name: json.RawMessage(`{"dir": "elsewhere"}`)}
		p := New(s)
		if _, err := app(t, p, false, cfg, func(*collage.RenderContext) {}); err != nil {
			t.Fatal(err)
		}
		if p.Store() != s {
			t.Error("Init replaced the store New was given")
		}
	})
}

// sniff fills its window from a reader that hands out a byte at a time, keeps
// a file shorter than the window whole, and treats only io.EOF as its end: a
// part cut off mid-body reports io.ErrUnexpectedEOF itself, which is a failed
// read, not a short file.
func TestSniff(t *testing.T) {
	long := bytes.Repeat([]byte("a"), 600)
	head, typ, err := sniff(iotest.OneByteReader(bytes.NewReader(long)))
	if err != nil || len(head) != sniffLen || typ != "text/plain; charset=utf-8" {
		t.Errorf("long: %d bytes, %q, %v", len(head), typ, err)
	}

	head, _, err = sniff(iotest.DataErrReader(strings.NewReader("ten bytes!")))
	if err != nil || string(head) != "ten bytes!" {
		t.Errorf("short: %q, %v", head, err)
	}

	head, typ, err = sniff(strings.NewReader(""))
	if err != nil || len(head) != 0 || typ != "application/octet-stream" {
		t.Errorf("empty: %q, %q, %v", head, typ, err)
	}

	_, _, err = sniff(&failingBody{data: []byte("ten bytes!"), err: io.ErrUnexpectedEOF})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut off: err = %v, want io.ErrUnexpectedEOF", err)
	}
}
