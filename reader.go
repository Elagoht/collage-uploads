package uploads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// sniffLen is how many bytes http.DetectContentType looks at.
const sniffLen = 512

// errFileTooLarge is what fileReader returns once a file passes its limit.
// Receive reports it as an *Error wrapping ErrTooLarge.
var errFileTooLarge = errors.New("elagoht/uploads: file over its size limit")

// sniff reads up to sniffLen bytes of src and returns them with the type they
// sniff as. A file shorter than the window is not an error: only io.EOF ends it
// short. Any other error, io.ErrUnexpectedEOF included, is a failed read: a
// multipart part returns that itself when the body is cut off, and io.ReadFull
// would make it look like a short file.
func sniff(src io.Reader) ([]byte, string, error) {
	buf := make([]byte, sniffLen)
	n := 0
	for n < len(buf) {
		m, err := src.Read(buf[n:])
		n += m
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
	}
	buf = buf[:n]
	if n == 0 {
		return buf, "application/octet-stream", nil
	}
	return buf, http.DetectContentType(buf), nil
}

// mediaType is a content type without its parameters, lower-cased, for
// comparing against a Types rule.
func mediaType(contentType string) string {
	media, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(media))
}

// fileReader streams one file to the store: the sniffed head, then the rest of
// the source. It counts and hashes what it hands on, and fails with
// errFileTooLarge once more than max bytes have passed (max <= 0 is no limit).
// It never asks the source for more than max+1 bytes in all, so a file over its
// limit is not read further than that.
//
// It also records how the read ended, so verify can check a store that
// reported success: a store may ignore the reader's error or stop early.
type fileReader struct {
	src      io.Reader
	max      int64
	n        int64
	hash     hash.Hash
	tooLarge bool
	eof      bool  // the source reached its end
	err      error // the first read error other than io.EOF
}

func newFileReader(head []byte, rest io.Reader, max int64) *fileReader {
	return &fileReader{src: io.MultiReader(bytes.NewReader(head), rest), max: max, hash: sha256.New()}
}

func (f *fileReader) Read(p []byte) (int, error) {
	if f.max > 0 {
		left := f.max + 1 - f.n
		if left <= 0 {
			f.tooLarge = true
			return 0, errFileTooLarge
		}
		if int64(len(p)) > left {
			p = p[:left]
		}
	}
	n, err := f.src.Read(p)
	f.n += int64(n)
	f.hash.Write(p[:n])
	if f.max > 0 && f.n > f.max {
		f.tooLarge = true
		return n, errFileTooLarge
	}
	switch {
	case err == io.EOF:
		f.eof = true
	case err != nil && f.err == nil:
		f.err = err
	}
	return n, err
}

// verify checks a Put that reported n bytes stored against what was read: the
// file within its limit, read without error to its end, and n all of it.
func (f *fileReader) verify(n int64) error {
	switch {
	case f.tooLarge:
		return errFileTooLarge
	case f.err != nil:
		return f.err
	case !f.eof:
		return errors.New("elagoht/uploads: the store stopped reading before the end of the file")
	case n != f.n:
		return fmt.Errorf("elagoht/uploads: the store reported %d bytes stored of %d read", n, f.n)
	}
	return nil
}

// sum is the hex SHA-256 of what has been read.
func (f *fileReader) sum() string { return hex.EncodeToString(f.hash.Sum(nil)) }
