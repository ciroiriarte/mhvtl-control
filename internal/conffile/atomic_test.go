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

func TestMoveAcrossRename(t *testing.T) {
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

// TestMoveAcrossCopy forces EXDEV from the rename, as two mounts give, and
// checks the copy path keeps content, mode and mtime, and removes the source.
func TestMoveAcrossCopy(t *testing.T) {
	var renamed int
	rename = func(string, string) error { renamed++; return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	t.Cleanup(func() { rename = os.Rename })

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
		t.Errorf("rename tried %d times", renamed)
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
