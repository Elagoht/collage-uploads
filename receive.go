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
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// maxValueBytes caps the non-file values of one submission, in total, names
// and per-part overhead included.
const maxValueBytes = 64 << 10

// valuePartOverhead is what each value part costs beyond its name and value,
// as multipart.Reader.ReadForm charges it: without it, a flood of empty
// values would cost nothing.
const valuePartOverhead = 200

// maxParts is how many parts the stream path reads, every part counted, as
// ParseMultipartForm's default allows.
const maxParts = 1000

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

// readKey marks a render whose request body a Receive has taken. collage.Once
// runs the first caller's function and hands every later caller its result
// without running theirs, atomically, so whichever Receive's function ran is
// the one that reads. The mark lives exactly as long as the render: nothing
// to clean up, and it does not care whether the request's context has ended.
var readKey = collage.NewKey[bool]("elagoht/uploads:read")

// Receive reads the submission's files and values, checks each file against
// its field's rules, and stores the files. It reads the form the application
// already parsed when there is one (rc.Request.MultipartForm), and otherwise
// streams the body part by part, so an action built WithStreamingBody never
// holds a whole file in memory or on disk outside the store.
//
// A file's type is sniffed from its first 512 bytes; the client's name and
// Content-Type are never consulted. Non-file values go to Values, at most
// 64 KiB in total, counting each value's name and 200 bytes per part. The
// stream path reads at most 1000 parts.
//
// It is all or nothing: on any error, whether a broken rule, a read that
// failed (a client gone, a body cut off), a body over the action's limit, a
// store that misreports what it stored, or a panic out of the store, every
// file this call stored is deleted before the error (or panic) goes on. A
// broken rule is an *Error; a body over its limit wraps *http.MaxBytesError,
// which collage answers with 413. A non-multipart request has no files, and
// its Values are its parsed PostForm, if any.
//
// With an *Error, Receive also returns a Received holding no files and the
// values read so far, for re-showing the form. On the stream path that is
// only the values that came before the failing part. Any other error comes
// back with a nil Received.
//
// When several parts break rules, which one is reported can differ between
// the paths: the stream path reports the first in the body, the parsed path
// checks values, then undeclared fields, then files field by field in name
// order.
//
// A request's body is read once: a second Receive in the same render returns
// ErrAlreadyRead, even when the first failed, and so does a Receive after the
// application took the body with MultipartReader itself. The mark is kept on
// the RenderContext collage built; a hand-built one has nowhere to keep it.
func (p *Plugin) Receive(rc *collage.RenderContext, fields ...FieldSpec) (*Received, error) {
	if p.store == nil {
		return nil, errors.New("uploads: no store: register the plugin, or set Options.Store")
	}
	if rc == nil || rc.Request == nil {
		return nil, errors.New("uploads: Receive needs the request's RenderContext")
	}
	first := false
	// The function cannot fail, so neither can Once, except for a later
	// caller whose context ended while waiting; that caller is not first
	// either way.
	_, _ = collage.Once(rc, readKey, func(context.Context) (bool, error) {
		first = true
		return true, nil
	})
	if !first {
		return nil, ErrAlreadyRead
	}

	r := rc.Request
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

	// A panic out of the store (or anything under it) still removes what
	// was stored; it is not recovered, so it goes on as it came.
	finished := false
	defer func() {
		if !finished {
			_ = rv.discard()
		}
	}()

	var err error
	if r.MultipartForm != nil {
		// MultipartReader leaves a non-nil, empty MultipartForm behind it;
		// ParseMultipartForm tells that apart from a parsed form by failing.
		if perr := r.ParseMultipartForm(0); perr != nil {
			finished = true
			return nil, ErrAlreadyRead
		}
		err = rv.fromForm(r.MultipartForm)
	} else {
		err = rv.fromStream(r)
	}
	finished = true
	if err == nil {
		return rv.out, nil
	}
	if derr := rv.discard(); derr != nil {
		err = errors.Join(err, derr)
	}
	var rule *Error
	if errors.As(err, &rule) {
		return &Received{Files: map[string][]File{}, Values: rv.out.Values}, err
	}
	return nil, err
}

// receiver is the state of one Receive.
type receiver struct {
	ctx        context.Context
	store      Store
	rules      map[string]Rules
	counts     map[string]int // files seen per field
	valueBytes int            // value budget spent
	parts      int            // parts read on the stream path
	keys       []string       // stored so far, deleted on failure
	out        *Received
}

// fromForm reads a form the application parsed: its values, then its files,
// field by field in name order.
func (v *receiver) fromForm(form *multipart.Form) error {
	if err := v.addValues(form.Value); err != nil {
		return err
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

// addValues adds values parsed elsewhere, in name order, against the budget.
func (v *receiver) addValues(values map[string][]string) error {
	for _, name := range sortedKeys(values) {
		for _, val := range values[name] {
			if err := v.charge(name, len(name)+valuePartOverhead+len(val)); err != nil {
				return err
			}
			v.out.Values.Add(name, val)
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
		return fmt.Errorf("uploads: open field %q: %w", name, err)
	}
	defer f.Close()
	return v.storeFile(name, fh.Filename, f)
}

// fromStream walks the body part by part, in the order the client sent them.
//
// A part is never closed: Close reads it to its end, which for a refused
// part is exactly the reading a refusal should spare, and NextPart skips
// whatever an accepted part left unread anyway.
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
		return v.addValues(r.PostForm)
	}
	if err != nil {
		return fmt.Errorf("uploads: %w", err)
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("uploads: read the body: %w", err)
		}
		v.parts++
		if v.parts > maxParts {
			return &Error{Field: part.FormName(), Err: ErrTooLarge}
		}
		if err := v.streamPart(part); err != nil {
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

// readValue reads a value part, never more than the budget left plus one
// byte.
func (v *receiver) readValue(name string, part io.Reader) error {
	if err := v.charge(name, len(name)+valuePartOverhead); err != nil {
		return err
	}
	left := int64(maxValueBytes - v.valueBytes)
	b, err := io.ReadAll(io.LimitReader(part, left+1))
	if err != nil {
		return readError(name, err)
	}
	if err := v.charge(name, len(b)); err != nil {
		return err
	}
	v.out.Values.Add(name, string(b))
	return nil
}

// charge spends n bytes of the values budget on field name.
func (v *receiver) charge(name string, n int) error {
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
// fresh key, counting and hashing it on the way. A Put that succeeds is
// checked against what was read: the store is not trusted to have honoured
// the reader's refusal, to have read to the end, or to report the size.
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
		return fmt.Errorf("uploads: field %q: %w", name, err)
	}
	fr := newFileReader(head, src, rules.MaxSize)
	n, err := v.store.Put(v.ctx, key, fr)
	if err == nil {
		// Stored, so deleted with the rest if what follows refuses it.
		v.keys = append(v.keys, key)
		err = fr.verify(n)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) && (fr.tooLarge || errors.Is(err, errFileTooLarge)) {
			return &Error{Field: name, Err: ErrTooLarge}
		}
		return readError(name, err)
	}
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
			errs = append(errs, fmt.Errorf("uploads: delete %s after a failed Receive: %w", key, err))
		}
	}
	v.keys = nil
	return errors.Join(errs...)
}

// readError wraps an error reading or storing field name's data. A
// *http.MaxBytesError stays reachable through it, so collage answers 413.
func readError(name string, err error) error {
	return fmt.Errorf("uploads: field %q: %w", name, err)
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
