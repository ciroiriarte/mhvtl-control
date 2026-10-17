package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ApplyLockDir holds apply.lock, the cooperative lock that orders the
// daemon's own moves and media writes against applies (plan 3.2, step 1).
// The real exclusion is exclusive.lock; this one only keeps an honest
// daemon out of the way.
const (
	ApplyLockDir  = "/run/mhvtl-control"
	applyLockFile = "apply.lock"
)

// openApplyLock opens dir/apply.lock for flock(2) and nothing else. The
// directory is the daemon's to write, so whatever is there may have been
// planted: every open of apply.lock goes through here, and
//
//   - O_NOFOLLOW refuses a symlink, which could point the open anywhere;
//   - O_CREAT without O_TRUNC never truncates an existing file, and the
//     file is opened read-only and never written: flock needs neither;
//   - O_NONBLOCK keeps a planted FIFO from blocking the open, and fstat
//     refuses anything but a regular file.
func openApplyLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, applyLockFile)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is a %s, not a regular file", path, fi.Mode().Type())
	}
	return f, nil
}
