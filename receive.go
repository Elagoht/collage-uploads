package uploads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// maxValueBytes caps the non-file values of one submission, in total.
const maxValueBytes = 64 << 10

// cleanupTimeout bounds deleting what a failed Receive had stored.
const cleanupTimeout = 5 * time.Second

// Rules are what one field's files must satisfy.
type Rules struct {
	// MaxSize is the most bytes one file may have. 0 means no per-file limit;
	// the action's body limit still applies.
	MaxSize int64
	// MaxFiles is how many files the field may carry. 0 means 1.
	MaxFiles int
	// Types are the sniffed media types allowed, without parameters
	// ("image/png", "text/plain"). Empty allows any type.
	Types []string
}

// FieldSpec declares a file field Receive accepts. Build one with Field.
type FieldSpec struct {
	name  string
	rules Rules
}

// Field declares the file field name and the rules its files must satisfy.
func Field(name string, rules Rules) FieldSpec { return FieldSpec{name: name, rules: rules} }

// File is one stored file.
type File struct {
	// Key is where the store keeps it: random, never derived from the client.
	Key string
	// Name is the client's file name, cleaned. It is for display only and is
	// never a path.
	Name string
	// Type is the sniffed content type, parameters included
	// ("text/plain; charset=utf-8").
	Type string
	// SHA256 is the hex SHA-256 of the stored bytes.
	SHA256 string
	// Size is the number of bytes stored.
	Size int64
}

// Received is what one submission carried.
type Received struct {
	// Files are the stored files, by field.
	Files map[string][]File
	// Values are the non-file fields.
	Values url.Values
}

// consumed holds the requests a Receive has started on. A request's body can
// be read once, so the marker belongs to the request, not to the render or to
// one Plugin. Checking and setting it under one mutex makes two concurrent
// Receives on one request race-free: exactly one wins. The marker is set
// before anything is read and stays when Receive returns, failed or not, so a
// later Receive sees it. It is removed when the request's context ends, which
// for a served request is when its handler returns, but can be sooner: a reset
// stream or a deadline ends it mid-handler. So Receive checks the context right
// after marking: the marker is deleted only once the context is done, so a call
// that finds no marker after that sees a non-nil Err and stops there, rather
// than reading the body again or mistaking the sentinel MultipartReader leaves
// in MultipartForm for a parsed form.
//
// A plain map under a mutex rather than a sync.Map, whose API is any-typed;
// and not a collage Key on the RenderContext, whose Get then Set is not
// atomic.
var consumed = struct {
	mu sync.Mutex
	m  map[*http.Request]struct{}
}{m: map[*http.Request]struct{}{}}

// markRead records that r's body is being read, and reports whether it was
// the first to.
func markRead(r *http.Request) bool {
	consumed.mu.Lock()
	defer consumed.mu.Unlock()
	if _, ok := consumed.m[r]; ok {
		return false
	}
	consumed.m[r] = struct{}{}
	context.AfterFunc(r.Context(), func() {
		consumed.mu.Lock()
		delete(consumed.m, r)
		consumed.mu.Unlock()
	})
	return true
}

// Receive reads the submission's files and values, checks each file against
// its field's rules, and stores the files. It reads the form the application
// already parsed when there is one (rc.Request.MultipartForm), and otherwise
// streams the body part by part, so an action built WithStreamingBody never
// holds a whole file in memory or on disk outside the store.
//
// A file's type is sniffed from its first 512 bytes; the client's name and
// Content-Type are never consulted. Non-file values go to Values, at most
// 64 KiB in total.
//
// It is all or nothing: on any error, whether a broken rule, a read that
// failed (a client gone, a body cut off) or a body over the action's limit,
// every file this call stored is deleted before the error is returned. A
// broken rule is an *Error; a body over its limit wraps *http.MaxBytesError,
// which collage answers with 413. A non-multipart request has no files, and
// its Values are its parsed PostForm, if any.
//
// A request's body is read once: a second Receive on it returns
// ErrAlreadyRead, even when the first failed.
func (p *Plugin) Receive(rc *collage.RenderContext, fields ...FieldSpec) (*Received, error) {
	if p.store == nil {
		return nil, errors.New("elagoht/uploads: no store: register the plugin, or pass a store to New")
	}
	if rc == nil || rc.Request == nil {
		return nil, errors.New("elagoht/uploads: Receive needs the request's RenderContext")
	}
	r := rc.Request
	if !markRead(r) {
		return nil, ErrAlreadyRead
	}
	if err := r.Context().Err(); err != nil {
		return nil, fmt.Errorf("elagoht/uploads: %w", err)
	}

	rv := &receiver{
		ctx:    rc.Context(),
		store:  p.store,
		rules:  make(map[string]Rules, len(fields)),
		counts: make(map[string]int, len(fields)),
		out:    &Received{Files: map[string][]File{}, Values: url.Values{}},
	}
	for _, f := range fields {
		rv.rules[f.name] = f.rules
	}

	var err error
	if r.MultipartForm != nil {
		err = rv.fromForm(r.MultipartForm)
	} else {
		err = rv.fromStream(r)
	}
	if err != nil {
		if derr := rv.discard(); derr != nil {
			err = errors.Join(err, derr)
		}
		return nil, err
	}
	return rv.out, nil
}

// receiver is the state of one Receive.
type receiver struct {
	ctx        context.Context
	store      Store
	rules      map[string]Rules
	counts     map[string]int // files seen per field
	valueBytes int            // non-file value bytes seen
	keys       []string       // stored so far, deleted on failure
	out        *Received
}

// fromForm reads a form the application parsed: its values, then its files,
// field by field in name order.
func (v *receiver) fromForm(form *multipart.Form) error {
	for _, name := range sortedKeys(form.Value) {
		for _, val := range form.Value[name] {
			if err := v.addValue(name, len(val)); err != nil {
				return err
			}
			v.out.Values.Add(name, val)
		}
	}
	// Every undeclared field first, so nothing is stored for a submission
	// that is refused anyway.
	names := sortedKeys(form.File)
	for _, name := range names {
		if _, ok := v.rules[name]; !ok && len(form.File[name]) > 0 {
			return &Error{Field: name, Err: ErrUnexpectedField}
		}
	}
	for _, name := range names {
		for _, fh := range form.File[name] {
			if err := v.openFile(name, fh); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *receiver) openFile(name string, fh *multipart.FileHeader) error {
	// The count before the open, as the stream path counts before it reads.
	if err := v.count(name); err != nil {
		return err
	}
	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("elagoht/uploads: open field %q: %w", name, err)
	}
	defer f.Close()
	return v.storeFile(name, fh.Filename, f)
}

// fromStream walks the body part by part, in the order the client sent them.
func (v *receiver) fromStream(r *http.Request) error {
	// MultipartReader marks the request as read by the multipart reader
	// before it looks at the Content-Type, which would leave a form that is
	// not multipart unparseable for the application. So the type is checked
	// here first, the way MultipartReader checks it.
	var mr *multipart.Reader
	var err error = http.ErrNotMultipart
	if media, _, perr := mime.ParseMediaType(r.Header.Get("Content-Type")); perr == nil &&
		(media == "multipart/form-data" || media == "multipart/mixed") {
		mr, err = r.MultipartReader()
	}
	if errors.Is(err, http.ErrNotMultipart) {
		for _, name := range sortedKeys(r.PostForm) {
			for _, val := range r.PostForm[name] {
				if err := v.addValue(name, len(val)); err != nil {
					return err
				}
				v.out.Values.Add(name, val)
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("elagoht/uploads: %w", err)
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("elagoht/uploads: read the body: %w", err)
		}
		err = v.streamPart(part)
		part.Close()
		if err != nil {
			return err
		}
	}
}

// streamPart handles one part the way multipart.Reader.ReadForm would sort
// it: no form name is skipped, no file name is a value, the rest are files.
func (v *receiver) streamPart(part *multipart.Part) error {
	name := part.FormName()
	if name == "" {
		return nil
	}
	filename := part.FileName()
	if filename == "" {
		return v.readValue(name, part)
	}
	if _, ok := v.rules[name]; !ok {
		return &Error{Field: name, Err: ErrUnexpectedField}
	}
	if err := v.count(name); err != nil {
		return err
	}
	return v.storeFile(name, filename, part)
}

// readValue reads a value part, never more than the values' remaining budget
// plus one byte.
func (v *receiver) readValue(name string, part io.Reader) error {
	left := int64(maxValueBytes - v.valueBytes)
	b, err := io.ReadAll(io.LimitReader(part, left+1))
	if err != nil {
		return readError(name, err)
	}
	if err := v.addValue(name, len(b)); err != nil {
		return err
	}
	v.out.Values.Add(name, string(b))
	return nil
}

// addValue counts n value bytes against the 64 KiB budget.
func (v *receiver) addValue(name string, n int) error {
	v.valueBytes += n
	if v.valueBytes > maxValueBytes {
		return &Error{Field: name, Err: ErrTooLarge}
	}
	return nil
}

// count counts one more file in field name, refusing the one past MaxFiles
// before any of it is read.
func (v *receiver) count(name string) error {
	max := v.rules[name].MaxFiles
	if max <= 0 {
		max = 1
	}
	v.counts[name]++
	if v.counts[name] > max {
		return &Error{Field: name, Err: ErrTooMany}
	}
	return nil
}

// storeFile sniffs src, checks its type, and streams it to the store under a
// fresh key, counting and hashing it on the way.
func (v *receiver) storeFile(name, filename string, src io.Reader) error {
	rules := v.rules[name]
	head, typ, err := sniff(src)
	if err != nil {
		return readError(name, err)
	}
	if len(rules.Types) > 0 && !allowed(rules.Types, typ) {
		return &Error{Field: name, Err: ErrType}
	}
	key, err := newKey(typ)
	if err != nil {
		return fmt.Errorf("elagoht/uploads: field %q: %w", name, err)
	}
	fr := newFileReader(head, src, rules.MaxSize)
	n, err := v.store.Put(v.ctx, key, fr)
	if err != nil {
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) && (fr.tooLarge || errors.Is(err, errFileTooLarge)) {
			return &Error{Field: name, Err: ErrTooLarge}
		}
		return readError(name, err)
	}
	v.keys = append(v.keys, key)
	v.out.Files[name] = append(v.out.Files[name], File{
		Key:    key,
		Name:   cleanName(filename),
		Type:   typ,
		SHA256: fr.sum(),
		Size:   n,
	})
	return nil
}

// discard deletes every file this Receive stored. It runs on its own context:
// the request's may be what ended, and the files must go regardless.
func (v *receiver) discard() error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	var errs []error
	for _, key := range v.keys {
		if err := v.store.Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("elagoht/uploads: delete %s after a failed Receive: %w", key, err))
		}
	}
	v.keys = nil
	return errors.Join(errs...)
}

// readError wraps an error reading or storing field name's data. A
// *http.MaxBytesError stays reachable through it, so collage answers 413.
func readError(name string, err error) error {
	return fmt.Errorf("elagoht/uploads: field %q: %w", name, err)
}

// allowed reports whether the sniffed type is one of types, compared without
// parameters and case.
func allowed(types []string, sniffed string) bool {
	media := mediaType(sniffed)
	for _, t := range types {
		if mediaType(t) == media {
			return true
		}
	}
	return false
}

func sortedKeys[V []string | []*multipart.FileHeader](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
