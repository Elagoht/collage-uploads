package uploads

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// defaultCacheControl is the Cache-Control a Handler sends unless told
// otherwise: a browser may keep the file but must revalidate it, and a shared
// cache must not keep it at all.
const defaultCacheControl = "private, max-age=0"

// HandlerOptions configure a Handler.
type HandlerOptions struct {
	// CacheControl is the Cache-Control header sent with every file. Empty
	// means "private, max-age=0".
	CacheControl string
}

// Handler returns an http.Handler that serves stored files by key, for GET
// and HEAD (any other method is 405). Mount it under a prefix ending in "/":
//
//	app.Handle("/uploads/", u.Handler())
//
// The key is the last segment of the request's path, after decoding, so no
// StripPrefix is needed. A key the store would refuse (anything outside
// [a-z0-9_.-], uppercase included, a leading or trailing ".", "..") and a key
// that is not stored are both 404. Any other store error is 500, logged
// through the host's logger.
//
// Every file is answered with:
//   - Content-Type: the type sniffed from the stored file's first 512 bytes,
//     never the key's extension or anything the uploader claimed;
//   - X-Content-Type-Options: nosniff;
//   - Content-Security-Policy: sandbox;
//   - Content-Disposition: attachment, with the key as the file name, for
//     everything except image/png, image/jpeg, image/gif and image/webp, which
//     are shown inline (SVG and HTML are always attachments);
//   - Cache-Control: "private, max-age=0", or opts' CacheControl.
//
// When the store's reader can seek (Dir's can), a single Range and
// conditional requests are answered by http.ServeContent, with those headers
// kept. A request for several ranges is served whole, since a
// multipart/byteranges answer would replace the sniffed Content-Type. A
// reader that cannot seek is copied whole, and a Range is ignored.
func (p *Plugin) Handler(opts ...HandlerOptions) http.Handler {
	cacheControl := defaultCacheControl
	for _, o := range opts {
		if o.CacheControl != "" {
			cacheControl = o.CacheControl
		}
	}
	return &handler{p: p, cacheControl: cacheControl}
}

type handler struct {
	p            *Plugin
	cacheControl string
}

// inline reports whether a sniffed type is a raster image a browser may
// show in place.
func inline(contentType string) bool {
	switch mediaType(contentType) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// statter is what an *os.File offers for a modification time.
type statter interface {
	Stat() (fs.FileInfo, error)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	key := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	if !validKey(key) {
		http.NotFound(w, r)
		return
	}
	store := h.p.store
	if store == nil {
		h.fail(w, r, key, errors.New("elagoht/uploads: no store: the plugin has not been initialised"))
		return
	}
	body, err := store.Open(r.Context(), key)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, key, err)
		return
	}
	defer body.Close()

	head, contentType, err := sniff(body)
	if err != nil {
		h.fail(w, r, key, err)
		return
	}

	seeker, seekable := body.(io.ReadSeeker)
	if seekable {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			h.fail(w, r, key, err)
			return
		}
	}

	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "sandbox")
	hdr.Set("Cache-Control", h.cacheControl)
	if !inline(contentType) {
		// validKey allows no quote, backslash or control character, so the
		// key needs no escaping inside the quotes.
		hdr.Set("Content-Disposition", `attachment; filename="`+key+`"`)
	}

	if seekable {
		var modtime time.Time
		if s, ok := body.(statter); ok {
			if info, err := s.Stat(); err == nil {
				modtime = info.ModTime()
			}
		}
		// The Content-Type set above is kept: ServeContent sniffs only when
		// there is none. It would replace it with multipart/byteranges for
		// more than one range, so such a request is served whole.
		if strings.Contains(r.Header.Get("Range"), ",") {
			r = r.Clone(r.Context())
			r.Header.Del("Range")
		}
		http.ServeContent(w, r, "", modtime, &quietSeeker{ReadSeeker: seeker, h: h, r: r, key: key})
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	// A failed write is a client gone; there is nobody left to answer.
	_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(head), body))
}

// fail answers 500 without the store's error, and logs it.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, key string, err error) {
	h.logError(r, key, err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// logError logs a failure serving key through the host's logger, unless the
// client has already gone.
func (h *handler) logError(r *http.Request, key string, err error) {
	if r.Context().Err() != nil {
		return
	}
	log := h.p.log
	if log == nil {
		log = slog.Default()
	}
	log.ErrorContext(r.Context(), "elagoht/uploads: serving a file failed", "key", key, "error", err)
}

// errSeek is what ServeContent sees when a seek fails. ServeContent writes a
// failed range seek's error into the response body, and a store's error can
// name a path on the server, so the real one is only logged.
var errSeek = errors.New("elagoht/uploads: seek failed")

// quietSeeker logs a failed seek and hands ServeContent errSeek instead.
type quietSeeker struct {
	io.ReadSeeker
	h   *handler
	r   *http.Request
	key string
}

func (q *quietSeeker) Seek(offset int64, whence int) (int64, error) {
	n, err := q.ReadSeeker.Seek(offset, whence)
	if err != nil {
		q.h.logError(q.r, q.key, err)
		return n, errSeek
	}
	return n, nil
}
