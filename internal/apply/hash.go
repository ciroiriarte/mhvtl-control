package apply

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mhvtl-control/internal/conffile"
)

// Base hashes are checked twice (plan 3.2, hash.go): here, when the request
// arrives, and again immediately before each rename (conffile.ReplaceIfHash,
// and removeIfHash below), because a hand edit takes no lock. What remains
// is the window between that second re-hash and rename(2) or unlink(2),
// microseconds long.

// checkBases compares each named file's base hash with the file on disk;
// the first mismatch is a *conffile.ConflictError.
func checkBases(req *Request, files *fileSet) error {
	check := func(name, base string) error {
		st, err := files.current(name)
		if err != nil {
			return err
		}
		if st.sha != base {
			return &conffile.ConflictError{Path: filepath.Join(files.dir, name), Current: st.sha}
		}
		return nil
	}
	for _, f := range req.Files {
		if err := check(f.Name, f.BaseSHA256); err != nil {
			return err
		}
	}
	for _, d := range req.Delete {
		if err := check(d.Name, d.BaseSHA256); err != nil {
			return err
		}
	}
	return nil
}

// beforeRemove is a test seam: it runs in removeIfHash before the re-hash.
var beforeRemove = func() {}

// removeIfHash removes dir/name if its hash is sha, moving it into keep
// when keep is set (a deleted file's rollback copy) or unlinking it
// otherwise, then fsyncs dir. A file whose hash differs, or that is gone,
// is a *conffile.ConflictError.
func removeIfHash(dir, name, sha, keep string) error {
	path := filepath.Join(dir, name)
	beforeRemove()
	st, err := readTarget(dir, name)
	if err != nil {
		return err
	}
	if st.sha != sha {
		return &conffile.ConflictError{Path: path, Current: st.sha}
	}
	if keep != "" {
		return conffile.MoveAcross(path, keep)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("%w: fsync %s: %w", conffile.ErrNotDurable, dir, err)
	}
	return nil
}

// conflictOf returns the conflict err carries, if any.
func conflictOf(err error) (*conffile.ConflictError, bool) {
	var c *conffile.ConflictError
	ok := errors.As(err, &c)
	return c, ok
}
