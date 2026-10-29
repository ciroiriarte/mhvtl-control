// Package mhvtlbin tells what the installed mhVTL binaries support, for
// features device.conf alone cannot show.
package mhvtlbin

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GCM is whether a vtltape binary has AES-256-GCM drive encryption.
type GCM int

const (
	// GCMUnknown: the binary cannot be found or read.
	GCMUnknown GCM = iota
	// GCMAbsent: vtltape predates markh794/mhvtl#328, so all its
	// encryption is the simulated kind, with the key in each block header,
	// and it ignores device.conf Encryption.
	GCMAbsent
	// GCMPresent: vtltape reads device.conf Encryption: aes-gcm|simulated.
	GCMPresent
)

// gcmMarker begins the error vtltape logs for a bad Encryption value
// (mhvtl usr/cmd/vtltape.c:2398 at 03c7d09). It came with the directive in
// markh794/mhvtl#328 (833f3ad); an older vtltape does not contain it.
const gcmMarker = "Encryption: expects"

// LibexecDirs are where a name not found in PATH is looked for next. mhVTL
// installs its daemons in $(LIBEXECDIR)/mhvtl, /usr/libexec/mhvtl with the
// default PREFIX, since markh794/mhvtl 0d1a744 (config.mk:20-24 at 0d98bde);
// before that they were in /usr/bin, which PATH finds.
var LibexecDirs = []string{"/usr/libexec/mhvtl", "/usr/local/libexec/mhvtl"}

// Vtltape inspects one vtltape binary. The result is kept until the file's
// path, size or modification time changes.
type Vtltape struct {
	Path string // a path, or a name looked up in PATH, then in LibexecDirs

	mu  sync.Mutex
	key fileKey
	gcm GCM
}

// locate resolves v.Path to the file to inspect.
func (v *Vtltape) locate() (string, error) {
	path, err := exec.LookPath(v.Path)
	if err == nil || strings.ContainsRune(v.Path, '/') {
		return path, err
	}
	for _, d := range LibexecDirs {
		p := filepath.Join(d, v.Path)
		if fi, serr := os.Stat(p); serr == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", err
}

type fileKey struct {
	path  string
	size  int64
	mtime time.Time
}

// GCM reports whether the binary has AES-GCM encryption.
func (v *Vtltape) GCM() GCM {
	path, err := v.locate()
	if err != nil {
		return GCMUnknown
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return GCMUnknown
	}
	k := fileKey{path, fi.Size(), fi.ModTime()}
	v.mu.Lock()
	defer v.mu.Unlock()
	if k == v.key && v.gcm != GCMUnknown {
		return v.gcm
	}
	found, err := contains(path, []byte(gcmMarker))
	switch {
	case err != nil:
		v.gcm = GCMUnknown
	case found:
		v.gcm = GCMPresent
	default:
		v.gcm = GCMAbsent
	}
	v.key = k
	return v.gcm
}

// contains reports whether the file at path holds s, reading it in chunks.
func contains(path string, s []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	keep := 0 // bytes carried over from the previous chunk
	for {
		n, err := f.Read(buf[keep:])
		if bytes.Contains(buf[:keep+n], s) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		end := keep + n
		keep = min(len(s)-1, end)
		copy(buf, buf[end-keep:end])
	}
}
