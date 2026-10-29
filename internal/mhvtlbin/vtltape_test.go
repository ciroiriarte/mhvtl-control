package mhvtlbin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string, b []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestGCM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vtltape")
	v := &Vtltape{Path: path}
	if got := v.GCM(); got != GCMUnknown {
		t.Errorf("missing binary: %v, want unknown", got)
	}

	t0 := time.Unix(1700000000, 0)
	old := bytes.Repeat([]byte{0x7f}, 200<<10)
	write(t, path, old, t0)
	if got := v.GCM(); got != GCMAbsent {
		t.Errorf("binary without the marker: %v, want absent", got)
	}

	// The marker straddles the 64 KiB read boundary.
	gcm := append([]byte{}, old...)
	copy(gcm[64<<10-7:], gcmMarker)
	write(t, path, gcm, t0.Add(time.Second))
	if got := v.GCM(); got != GCMPresent {
		t.Errorf("binary with the marker: %v, want present", got)
	}

	// Same size and mtime: the cached result stands.
	write(t, path, old, t0.Add(time.Second))
	if got := v.GCM(); got != GCMPresent {
		t.Errorf("unchanged size and mtime: %v, want the cached present", got)
	}
	// A new mtime is read again.
	write(t, path, old, t0.Add(2*time.Second))
	if got := v.GCM(); got != GCMAbsent {
		t.Errorf("replaced binary: %v, want absent", got)
	}

	write(t, path, old, t0.Add(3*time.Second))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if got := v.GCM(); got != GCMUnknown {
			t.Errorf("unreadable binary: %v, want unknown", got)
		}
	}
}

// A name without a slash is looked up in PATH.
func TestGCMPath(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "vtltape"), []byte("x"+gcmMarker), time.Now())
	t.Setenv("PATH", dir)
	if got := (&Vtltape{Path: "vtltape"}).GCM(); got != GCMPresent {
		t.Errorf("GCM() = %v, want present", got)
	}
}

func TestLocateLibexec(t *testing.T) {
	dir := t.TempDir()
	saved := LibexecDirs
	t.Cleanup(func() { LibexecDirs = saved })
	LibexecDirs = []string{filepath.Join(dir, "none"), dir}
	t.Setenv("PATH", t.TempDir())

	name := "vtltape-test-libexec"
	v := &Vtltape{Path: name}
	if got := v.GCM(); got != GCMUnknown {
		t.Errorf("in neither PATH nor libexec: %v, want unknown", got)
	}
	write(t, filepath.Join(dir, name), []byte("x"+gcmMarker+"x"), time.Unix(1700000000, 0))
	if got := v.GCM(); got != GCMPresent {
		t.Errorf("in libexec only: %v, want present", got)
	}
	// A path with a slash is taken as given, not searched for.
	if got := (&Vtltape{Path: "./" + name}).GCM(); got != GCMUnknown {
		t.Errorf("relative path outside libexec: %v, want unknown", got)
	}
}
