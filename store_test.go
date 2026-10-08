package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func readAll(t *testing.T, s Store, key string) string {
	t.Helper()
	rc, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("Open(%q): %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(b)
}

// leftovers lists the temp files Put left in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestDir_PutOpenDelete(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "uploads")
	s := Dir(dir)

	n, err := s.Put(ctx, "a.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != 5 {
		t.Errorf("Put wrote %d bytes, want 5", n)
	}
	if got := readAll(t, s, "a.txt"); got != "hello" {
		t.Errorf("content = %q, want hello", got)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}

	if err := s.Delete(ctx, "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Open(ctx, "a.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open after Delete: %v, want fs.ErrNotExist", err)
	}
	if err := s.Delete(ctx, "a.txt"); err != nil {
		t.Errorf("Delete of a missing key: %v, want nil", err)
	}
}

func TestDir_NoOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := Dir(dir)

	if _, err := s.Put(ctx, "k.bin", strings.NewReader("original")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := s.Put(ctx, "k.bin", strings.NewReader("replaced")); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put: %v, want ErrExists", err)
	}
	if got := readAll(t, s, "k.bin"); got != "original" {
		t.Errorf("content = %q, want original", got)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

// The existence check before the copy is only a fast path: two Puts racing
// past it must still end with exactly one winner, decided by the link.
func TestDir_ConcurrentPutOneWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := Dir(dir)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
			_, errs[i] = s.Put(ctx, "race.bin", bytes.NewReader(body))
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrExists):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d Puts succeeded, want 1", wins)
	}
	got := readAll(t, s, "race.bin")
	if len(got) != 64<<10 || strings.Count(got, got[:1]) != len(got) {
		t.Errorf("stored content is mixed or short (%d bytes)", len(got))
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

func TestDir_BadKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := Dir(dir)
	keys := []string{"", "../x", "a/b", `a\b`, "..", "x\n", "x\x00", ".", ".upload-x"}
	for _, key := range keys {
		if _, err := s.Put(ctx, key, strings.NewReader("x")); !errors.Is(err, ErrBadKey) {
			t.Errorf("Put(%q): %v, want ErrBadKey", key, err)
		}
		if _, err := s.Open(ctx, key); !errors.Is(err, ErrBadKey) {
			t.Errorf("Open(%q): %v, want ErrBadKey", key, err)
		}
		if err := s.Delete(ctx, key); !errors.Is(err, ErrBadKey) {
			t.Errorf("Delete(%q): %v, want ErrBadKey", key, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("bad keys left %d entries in the directory", len(entries))
	}
}

func TestDir_Permissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "nested", "uploads")
	s := Dir(dir)
	if _, err := s.Put(context.Background(), "p.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
	fi, err := os.Stat(filepath.Join(dir, "p.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
}

// stallReader gives 1 KiB, then blocks until the context is cancelled, then
// keeps giving data forever. Only a ctx check inside Put can stop the copy.
type stallReader struct {
	ctx     context.Context
	sent    int
	stalled chan struct{}
	once    sync.Once
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.sent >= 1024 {
		r.once.Do(func() { close(r.stalled) })
		<-r.ctx.Done()
	}
	n := min(len(p), 1024)
	for i := range n {
		p[i] = 'x'
	}
	r.sent += n
	return n, nil
}

func TestDir_CancelledCtxLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	s := Dir(dir)
	ctx, cancel := context.WithCancel(context.Background())
	r := &stallReader{ctx: ctx, stalled: make(chan struct{})}

	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, "c.bin", r)
		done <- err
	}()
	<-r.stalled
	cancel()
	err := <-done

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "c.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("target exists after cancel: %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

func TestDir_AlreadyCancelledCtx(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dir(dir).Put(ctx, "c.bin", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "c.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("target exists after cancel: %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestDir_ReaderErrorLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Dir(dir).Put(context.Background(), "f.bin", io.MultiReader(strings.NewReader("part"), failReader{})); err == nil {
		t.Fatal("Put succeeded with a failing reader")
	}
	if _, err := os.Lstat(filepath.Join(dir, "f.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("target exists after a read error: %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

// A filesystem without hard links (FAT, some network mounts) fails Link with
// something other than EEXIST; Put falls back to an exclusive create + copy.
func TestDir_LinkUnsupportedFallsBack(t *testing.T) {
	dir := t.TempDir()
	s := Dir(dir).(*dirStore)
	s.link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	ctx := context.Background()

	n, err := s.Put(ctx, "fb.txt", strings.NewReader("fallback"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != 8 {
		t.Errorf("Put wrote %d bytes, want 8", n)
	}
	if got := readAll(t, s, "fb.txt"); got != "fallback" {
		t.Errorf("content = %q, want fallback", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "fb.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("file mode = %o, want 600", got)
		}
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}

	// The fallback never overwrites either.
	if _, err := s.Put(ctx, "fb.txt", strings.NewReader("replaced")); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put: %v, want ErrExists", err)
	}
	if got := readAll(t, s, "fb.txt"); got != "fallback" {
		t.Errorf("content = %q, want fallback", got)
	}
}

// When a racing Put creates the target between the Lstat and the exclusive
// create, the fallback reports ErrExists and leaves the winner's file alone.
func TestDir_FallbackLosesRace(t *testing.T) {
	dir := t.TempDir()
	s := Dir(dir).(*dirStore)
	target := filepath.Join(dir, "r.txt")
	s.link = func(oldname, newname string) error {
		if err := os.WriteFile(target, []byte("winner"), 0o600); err != nil {
			t.Fatal(err)
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOTSUP}
	}
	if _, err := s.Put(context.Background(), "r.txt", strings.NewReader("loser")); !errors.Is(err, ErrExists) {
		t.Fatalf("Put: %v, want ErrExists", err)
	}
	if got := readAll(t, s, "r.txt"); got != "winner" {
		t.Errorf("content = %q, want winner", got)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}

// EEXIST from Link is the race being lost: ErrExists, and no fallback.
func TestDir_LinkExistsIsErrExists(t *testing.T) {
	dir := t.TempDir()
	s := Dir(dir).(*dirStore)
	s.link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	}
	if _, err := s.Put(context.Background(), "e.txt", strings.NewReader("x")); !errors.Is(err, ErrExists) {
		t.Fatalf("Put: %v, want ErrExists", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "e.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fallback ran after EEXIST: %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
}
