package uploads

import (
	"context"
	"errors"
	"fmt"
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
	// ErrBadKey is returned for a key outside the portable allowlist:
	// 1-255 bytes of [a-z0-9_.-], no leading or trailing ".", no "..", and
	// not a Windows reserved device name.
	ErrBadKey = errors.New("elagoht/uploads: invalid key")
)

// Dir returns a Store that keeps each file as path/key. The directory is
// created with mode 0700 on the first Put, and an existing directory with any
// group or other permission bit is tightened to 0700 (Put fails if it cannot
// be). Files are 0600. Open and Delete go through an os.Root of the directory
// and act only on regular files: a subdirectory or a symlink under a key is
// not found by Open and is refused by Delete.
//
// Put writes to a temp file in the same directory and then hard-links it to
// the key, so a reader never sees a half-written file and two Puts of the
// same key cannot both win. On a filesystem without hard links it falls back
// to an exclusive create and a copy, which still never overwrites but can
// expose a partly written file to a concurrent Open while it runs.
//
// A crash (or kill) mid-Put can leave an orphaned ".upload-*" temp file in the
// directory; no key can name it, and it is safe to delete. In fallback mode a
// crash during the copy can also leave a truncated file under the key itself.
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
	if err := d.ensureDir(); err != nil {
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
	defer tmp.Close()        // runs first; the data is already synced

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
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		if err := copyExclusive(ctx, tmp, target); err != nil {
			return 0, err
		}
		return n, nil
	}
}

// copyExclusive creates target with O_EXCL, so it never replaces a file, and
// copies src into it. If the copy fails, it removes target, which is safe
// because O_EXCL proved this call created it.
func copyExclusive(ctx context.Context, src io.Reader, target string) (err error) {
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

	if _, err = copyCtx(ctx, out, src); err != nil {
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

// ensureDir creates the directory as 0700, or tightens an existing one that
// grants any group or other permission. It fails closed if it cannot.
func (d *dirStore) ensureDir() error {
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(d.path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("elagoht/uploads: %s is not a directory", d.path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(d.path, 0o700); err != nil {
			return fmt.Errorf("elagoht/uploads: tighten %s to 0700: %w", d.path, err)
		}
	}
	return nil
}

// errNotRegular is what Open returns for a key naming a directory, a symlink
// or anything else that is not a regular file: to a caller it is not there.
func errNotRegular(op, key string) error {
	return &fs.PathError{Op: op, Path: key, Err: fs.ErrNotExist}
}

func (d *dirStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(d.path)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	// Lstat refuses a symlink; comparing it with the opened file's Stat
	// refuses one swapped in between the two calls.
	linfo, err := root.Lstat(key)
	if err != nil {
		return nil, err
	}
	if !linfo.Mode().IsRegular() {
		return nil, errNotRegular("open", key)
	}
	f, err := root.Open(key)
	if err != nil {
		return nil, err
	}
	finfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		f.Close()
		return nil, errNotRegular("open", key)
	}
	return f, nil
}

func (d *dirStore) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := root.Lstat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("elagoht/uploads: delete %s: not a regular file", key)
	}
	err = root.Remove(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
