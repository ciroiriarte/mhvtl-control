package apply

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenApplyLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, applyLockFile)

	// Created when missing, and lockable read-only.
	f, err := openApplyLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("flock: %v", err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Error("apply.lock opened writable")
	}
	f.Close()

	// An existing file is neither truncated nor written.
	if err := os.WriteFile(path, []byte("daemon"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = openApplyLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if b, _ := os.ReadFile(path); string(b) != "daemon" {
		t.Errorf("apply.lock now %q", b)
	}

	// A symlink, a FIFO and a directory are refused, and the open does not
	// block on the FIFO.
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("keep"), 0o644)
	for name, plant := range map[string]func() error{
		"symlink":   func() error { return os.Symlink(target, path) },
		"fifo":      func() error { return syscall.Mkfifo(path, 0o600) },
		"directory": func() error { return os.Mkdir(path, 0o755) },
	} {
		os.Remove(path)
		if err := plant(); err != nil {
			t.Fatal(err)
		}
		if f, err := openApplyLock(dir); err == nil {
			f.Close()
			t.Errorf("%s: opened", name)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target now %q", b)
	}
}
