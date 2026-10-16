package conffile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrConflict means the file changed since its base hash was taken.
var ErrConflict = errors.New("file changed since it was read")

// ConflictError reports a ReplaceIfHash refusal with the file's hash now
// ("" when it no longer exists). errors.Is(err, ErrConflict) holds.
type ConflictError struct {
	Path    string
	Current string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: %v (now sha256 %q)", e.Path, ErrConflict, e.Current)
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Test seams: beforeRehash runs in ReplaceIfHash between writing the temp
// file and re-hashing the target; rename is MoveAcross's first attempt.
var (
	beforeRehash = func() {}
	rename       = os.Rename
)

// writeTemp creates a temp file in dir, fills it with write, then fsyncs,
// fchowns (uid or gid -1 leaves that id unchanged) and fchmods it. On error
// the temp file is removed.
func writeTemp(dir, name string, mode fs.FileMode, uid, gid int, write func(*os.File) error) (path string, err error) {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = write(f); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if uid >= 0 || gid >= 0 {
		if err = f.Chown(uid, gid); err != nil {
			return "", err
		}
	}
	if err = f.Chmod(mode); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// WriteAtomic replaces dir/name with data: a temp file in the same
// directory, fsync, fchown and fchmod, rename over the target, then an fsync
// of the directory so the rename itself is durable. A reader sees the old
// file or the new one, never a mix.
func WriteAtomic(dir, name string, data []byte, mode fs.FileMode, uid, gid int) error {
	tmp, err := writeTemp(dir, name, mode, uid, gid, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// ReplaceIfHash is WriteAtomic guarded by a base hash: dir/name is re-read
// and re-hashed immediately before the rename, after the new content is
// already on disk, so a hand edit made at any point up to then is detected
// and refused with a *ConflictError. What is left is the window between that
// re-hash and the rename. An empty expectSHA means the file must not exist
// yet; it is then created 0644 with the caller's ids. An existing file keeps
// its mode and owner.
func ReplaceIfHash(dir, name, expectSHA string, data []byte) error {
	path := filepath.Join(dir, name)
	mode, uid, gid := fs.FileMode(0o644), -1, -1
	st, err := os.Stat(path)
	switch {
	case err == nil:
		mode = st.Mode().Perm()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tmp, err := writeTemp(dir, name, mode, uid, gid, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	beforeRehash()
	current := ""
	if _, sha, err := ReadFile(path); err == nil {
		current = sha
	} else if !errors.Is(err, fs.ErrNotExist) {
		os.Remove(tmp)
		return err
	}
	if current != expectSHA {
		os.Remove(tmp)
		return &ConflictError{Path: path, Current: current}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// MoveAcross moves the regular file src into dstDir under the same name,
// refusing to overwrite. It renames when it can; when src and dstDir are on
// different mounts (EXDEV, which bind mounts under ProtectSystem=strict also
// give) it copies to a temp file, fsyncs it, links it into place, fsyncs
// dstDir, and only then unlinks src and fsyncs its directory. Mode, owner
// (when running as root) and times are kept, since mhVTL compares
// library_contents mtimes (mhvtl usr/cmd/vtllibrary.c:980).
func MoveAcross(src, dstDir string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", src)
	}
	name := filepath.Base(src)
	dst := filepath.Join(dstDir, name)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s: %w", dst, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = rename(src, dst)
	if err == nil {
		if err := syncDir(dstDir); err != nil {
			return err
		}
		return syncDir(filepath.Dir(src))
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	uid, gid := -1, -1
	atime, mtime := st.ModTime(), st.ModTime()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		atime = time.Unix(sys.Atim.Unix())
		if os.Geteuid() == 0 {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := writeTemp(dstDir, name, st.Mode().Perm(), uid, gid, func(f *os.File) error {
		_, err := io.Copy(f, in)
		return err
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Chtimes(tmp, atime, mtime); err != nil {
		return err
	}
	// link, unlike rename, fails if dst appeared in the meantime.
	if err := os.Link(tmp, dst); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := syncDir(dstDir); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	return syncDir(filepath.Dir(src))
}
