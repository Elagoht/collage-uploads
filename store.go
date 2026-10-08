package uploads

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Store keeps uploaded files under keys. Put never overwrites: a key that is
// already stored is ErrExists. Open of a missing key is fs.ErrNotExist.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

var (
	// ErrExists is returned by Put when the key is already stored.
	ErrExists = errors.New("elagoht/uploads: key exists")
	// ErrBadKey is returned for a key that is empty, too long, or contains
	// "/", "\", "..", a control character, or a leading ".".
	ErrBadKey = errors.New("elagoht/uploads: invalid key")
)

// Dir returns a Store that keeps each file as path/key. The directory is
// created with mode 0700 on the first Put, and files are 0600.
//
// Put writes to a temp file in the same directory and then hard-links it to
// the key, so a reader never sees a half-written file and two Puts of the
// same key cannot both win. On a filesystem without hard links it falls back
// to an exclusive create and a copy, which still never overwrites but can
// expose a partly written file to a concurrent Open while it runs.
func Dir(path string) Store {
	return &dirStore{path: path, link: os.Link}
}

type dirStore struct {
	path string
	// link is os.Link; tests swap it to reach the fallback and race paths.
	link func(oldname, newname string) error
}

func (d *dirStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if !validKey(key) {
		return 0, ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return 0, err
	}
	target := filepath.Join(d.path, key)
	// A fast path only: the link below is what decides a race.
	if _, err := os.Lstat(target); err == nil {
		return 0, ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}

	tmp, err := os.CreateTemp(d.path, ".upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // after a successful link the key keeps the data
	closed := false
	defer func() {
		if !closed {
			tmp.Close()
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return 0, err
	}
	n, err := copyCtx(ctx, tmp, r)
	if err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	closed = true
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// A cancel that arrived with the last read still wins over the store.
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	err = d.link(tmpName, target)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, fs.ErrExist):
		return 0, ErrExists
	default:
		// No hard links here (FAT, some network or FUSE mounts).
		if err := copyExclusive(ctx, tmpName, target); err != nil {
			return 0, err
		}
		return n, nil
	}
}

// copyExclusive creates target with O_EXCL, so it never replaces a file, and
// copies src into it. If the copy fails, it removes target, which is safe
// because O_EXCL proved this call created it.
func copyExclusive(ctx context.Context, src, target string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(target)
		}
	}()

	if _, err = copyCtx(ctx, out, in); err != nil {
		return err
	}
	return out.Sync()
}

// copyCtx copies r to w and checks ctx before every read, so a cancelled
// request stops a copy whose reader keeps producing data.
func copyCtx(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			total += int64(wn)
			if werr != nil {
				return total, werr
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

func (d *dirStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(d.path, key))
}

func (d *dirStore) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(d.path, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
