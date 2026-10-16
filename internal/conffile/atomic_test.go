package conffile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noTemps fails if a temp file was left in dir.
func noTemps(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.conf")
	os.WriteFile(path, []byte("old\n"), 0o600)
	if err := WriteAtomic(dir, "device.conf", []byte("new\n"), 0o640, -1, -1); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "new\n" {
		t.Errorf("content = %q", got)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	noTemps(t, dir)

	if err := WriteAtomic(filepath.Join(dir, "missing"), "x", nil, 0o644, -1, -1); err == nil {
		t.Error("write into a missing directory succeeded")
	}
}

func TestReplaceIfHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library_contents.10")
	base := []byte("Slot 1: A\n")
	os.WriteFile(path, base, 0o640)

	if err := ReplaceIfHash(dir, "library_contents.10", Hash(base), []byte("Slot 1: B\n")); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "Slot 1: B\n" {
		t.Errorf("content = %q", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", st.Mode().Perm())
	}

	// A stale base hash is refused, and the file is left alone.
	err := ReplaceIfHash(dir, "library_contents.10", Hash(base), []byte("Slot 1: C\n"))
	var ce *ConflictError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &ce) || ce.Current != Hash([]byte("Slot 1: B\n")) {
		t.Fatalf("stale hash: err = %v", err)
	}
	if got := readString(t, path); got != "Slot 1: B\n" {
		t.Errorf("content after refusal = %q", got)
	}

	// An empty base hash creates, and refuses when the file exists.
	if err := ReplaceIfHash(dir, "library_contents.40", "", []byte("Slot 1:\n")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ReplaceIfHash(dir, "library_contents.40", "", []byte("x\n")); !errors.Is(err, ErrConflict) {
		t.Errorf("create over an existing file: err = %v", err)
	}
	// A base hash for a file that has gone is a conflict too.
	if err := ReplaceIfHash(dir, "library_contents.99", Hash(base), []byte("x\n")); !errors.As(err, &ce) || ce.Current != "" {
		t.Errorf("missing file: err = %v", err)
	}
	noTemps(t, dir)
}

// TestReplaceIfHashLateConflict edits the file by hand after the new content
// is on disk but before the rename, the window the request-time check cannot
// see. The edit must win and the write be refused.
func TestReplaceIfHashLateConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.conf")
	base := []byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n")
	os.WriteFile(path, base, 0o644)
	hand := []byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n PERSIST: True\n")
	beforeRehash = func() {
		if err := os.WriteFile(path, hand, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeRehash = func() {} })

	err := ReplaceIfHash(dir, "device.conf", Hash(base), []byte("ours\n"))
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != Hash(hand) {
		t.Fatalf("err = %v, want a conflict with the hand edit's hash", err)
	}
	if got := readString(t, path); got != string(hand) {
		t.Errorf("hand edit lost: %q", got)
	}
	noTemps(t, dir)
}

// TestMoveAcrossSameFS moves within one filesystem, by link and unlink.
func TestMoveAcrossSameFS(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "library_contents.10.persist"), []byte("Slot 1: A\n"), 0o600)
	if err := MoveAcross(filepath.Join(src, "library_contents.10.persist"), dst); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(dst, "library_contents.10.persist")); got != "Slot 1: A\n" {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(src, "library_contents.10.persist")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still there: %v", err)
	}
}

// TestMoveAcrossCopy forces EXDEV from the link, as two mounts give, and
// checks the copy path keeps content, mode and mtime, and removes the source.
func TestMoveAcrossCopy(t *testing.T) {
	var renamed int
	link = func(string, string) error { renamed++; return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
	t.Cleanup(func() { link = os.Link })

	src, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "library_contents.10.persist")
	data := bytes.Repeat([]byte("Slot 1: A\n"), 10000)
	os.WriteFile(path, data, 0o640)
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(path, mtime, mtime)

	if err := MoveAcross(path, dst); err != nil {
		t.Fatal(err)
	}
	if renamed != 1 {
		t.Errorf("link tried %d times", renamed)
	}
	out := filepath.Join(dst, "library_contents.10.persist")
	if got := readString(t, out); got != string(data) {
		t.Errorf("content differs (%d bytes, want %d)", len(got), len(data))
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 || !st.ModTime().Equal(mtime) {
		t.Errorf("mode %v mtime %v; want 0640 %v", st.Mode().Perm(), st.ModTime(), mtime)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still there: %v", err)
	}
	noTemps(t, dst)
}

func TestMoveAcrossRefuses(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "f")
	os.WriteFile(path, []byte("new"), 0o644)
	os.WriteFile(filepath.Join(dst, "f"), []byte("old"), 0o644)
	if err := MoveAcross(path, dst); !errors.Is(err, fs.ErrExist) {
		t.Errorf("overwrite: err = %v", err)
	}
	if got := readString(t, filepath.Join(dst, "f")); got != "old" {
		t.Errorf("destination overwritten: %q", got)
	}
	if err := MoveAcross(src, dst); err == nil {
		t.Error("moved a directory")
	}
}

// TestMoveAcrossBindMount moves into a bind mount of a directory on the same
// filesystem, as ProtectSystem=strict sets up, where rename gives EXDEV.
// Mounting needs root, so this runs on the test VM and skips elsewhere.
func TestMoveAcrossBindMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to bind mount; run on the test VM")
	}
	src, backing, dst := t.TempDir(), t.TempDir(), t.TempDir()
	if err := syscall.Mount(backing, dst, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dst, 0) })
	probe := filepath.Join(src, "probe")
	os.WriteFile(probe, nil, 0o600)
	if err := os.Rename(probe, filepath.Join(dst, "probe")); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("expected EXDEV across the bind mount, got %v", err)
	}
	path := filepath.Join(src, "library_contents.10.persist")
	os.WriteFile(path, []byte("Slot 1: A\n"), 0o600)
	if err := MoveAcross(path, dst); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(dst, "library_contents.10.persist")); got != "Slot 1: A\n" {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still there: %v", err)
	}
}

func TestRefuseNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.conf")
	os.WriteFile(real, []byte("x"), 0o600)
	os.Symlink("real.conf", filepath.Join(dir, "device.conf"))
	os.Mkdir(filepath.Join(dir, "library_contents.10"), 0o755)

	for _, name := range []string{"device.conf", "library_contents.10"} {
		if err := WriteAtomic(dir, name, []byte("y"), 0o644, -1, -1); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("WriteAtomic %s: err = %v", name, err)
		}
		if err := ReplaceIfHash(dir, name, Hash([]byte("x")), []byte("y")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("ReplaceIfHash %s: err = %v", name, err)
		}
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "device.conf")); fi.Mode()&fs.ModeSymlink == 0 {
		t.Error("symlink replaced")
	}
	if got := readString(t, real); got != "x" {
		t.Errorf("link target changed: %q", got)
	}
	// A symlink swapped in after the temp file is written is caught too.
	path := filepath.Join(dir, "swap.conf")
	os.WriteFile(path, []byte("x"), 0o644)
	beforeRehash = func() { os.Remove(path); os.Symlink("real.conf", path) }
	t.Cleanup(func() { beforeRehash = func() {} })
	if err := ReplaceIfHash(dir, "swap.conf", Hash([]byte("x")), []byte("y")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("late symlink: err = %v", err)
	}
	noTemps(t, dir)
}

// TestReplaceIfHashCreateRace creates a file that appears after the temp
// file is written: link(2) must refuse rather than overwrite it.
func TestReplaceIfHashCreateRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library_contents.40")
	beforeRehash = func() { os.WriteFile(path, []byte("theirs\n"), 0o644) }
	t.Cleanup(func() { beforeRehash = func() {} })
	err := ReplaceIfHash(dir, "library_contents.40", "", []byte("ours\n"))
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Current != Hash([]byte("theirs\n")) {
		t.Fatalf("err = %v", err)
	}
	if got := readString(t, path); got != "theirs\n" {
		t.Errorf("overwritten: %q", got)
	}
	noTemps(t, dir)
}

func TestNotDurable(t *testing.T) {
	cause := errors.New("EIO")
	syncDir = func(string) error { return cause }
	t.Cleanup(func() {
		syncDir = func(dir string) error {
			d, err := os.Open(dir)
			if err != nil {
				return err
			}
			defer d.Close()
			return d.Sync()
		}
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "device.conf")
	err := WriteAtomic(dir, "device.conf", []byte("new\n"), 0o644, -1, -1)
	if !errors.Is(err, ErrNotDurable) || !errors.Is(err, cause) {
		t.Errorf("WriteAtomic: err = %v", err)
	}
	// The rename took effect all the same.
	if got := readString(t, path); got != "new\n" {
		t.Errorf("content = %q", got)
	}
	if err := ReplaceIfHash(dir, "device.conf", Hash([]byte("new\n")), []byte("newer\n")); !errors.Is(err, ErrNotDurable) {
		t.Errorf("ReplaceIfHash: err = %v", err)
	}
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, nil, 0o644)
	if err := MoveAcross(src, dir); !errors.Is(err, ErrNotDurable) {
		t.Errorf("MoveAcross: err = %v", err)
	}

	// On the copy path the move is still finished: the source goes, so the
	// file is not left in both places.
	link = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
	t.Cleanup(func() { link = os.Link })
	src = filepath.Join(t.TempDir(), "g")
	os.WriteFile(src, []byte("g"), 0o644)
	if err := MoveAcross(src, dir); !errors.Is(err, ErrNotDurable) || !errors.Is(err, cause) {
		t.Errorf("MoveAcross copy: err = %v", err)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source left behind: %v", err)
	}
	if got := readString(t, filepath.Join(dir, "g")); got != "g" {
		t.Errorf("copy = %q", got)
	}
}

// TestRemoveStale checks a writer removes temp files of the same name that a
// dead writer left more than staleTemp ago, and leaves recent ones (another
// writer may be using them) and other names alone.
func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleTemp)
	stale := filepath.Join(dir, ".device.conf.tmp-111")
	fresh := filepath.Join(dir, ".device.conf.tmp-222")
	other := filepath.Join(dir, ".library_contents.10.tmp-333")
	for _, p := range []string{stale, fresh, other} {
		os.WriteFile(p, nil, 0o600)
	}
	os.Chtimes(stale, old, old)
	os.Chtimes(other, old, old)
	if err := WriteAtomic(dir, "device.conf", []byte("x"), 0o644, -1, -1); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{stale: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}
