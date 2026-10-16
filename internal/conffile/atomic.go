package conffile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrConflict means the file changed since its base hash was taken.
var ErrConflict = errors.New("file changed since it was read")

// ErrNotDurable means the change took effect (the new file is in place, or
// the move is done) but the directory fsync after it failed, so it may not
// survive a crash. The returned error wraps both it and the cause.
var ErrNotDurable = errors.New("change made, but not made durable")

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

// staleTemp is the age after which a writer removes a temp file left by
// another one that died between creating it and renaming it.
const staleTemp = time.Hour

// Test seams: beforeRehash runs in ReplaceIfHash between writing the temp
// file and re-hashing the target; link is MoveAcross's first attempt;
// syncDir is the directory fsync.
var (
	beforeRehash = func() {}
	link         = os.Link
	syncDir      = func(dir string) error {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
)

func durable(dirs ...string) error {
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("%w: fsync %s: %w", ErrNotDurable, d, err)
		}
	}
	return nil
}

// regularTarget returns path's lstat, nil if it does not exist, and refuses
// anything but a regular file: renaming over a symlink would replace the
// link, not the file it points to.
func regularTarget(path string) (fs.FileInfo, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file (%v), refusing to replace it", path, st.Mode().Type())
	}
	return st, nil
}

// removeStale removes temp files of name in dir older than staleTemp. A
// writer that dies between creating its temp file and renaming it leaves
// one behind; the next writer of the same file cleans it up. Errors are
// ignored: a leftover temp file is harmless.
func removeStale(dir, name string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := "." + name + ".tmp-"
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleTemp {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// writeTemp creates a temp file ".<name>.tmp-*" in dir, fills it with
// write, then fsyncs, fchowns (uid or gid -1 leaves that id unchanged) and
// fchmods it. On error the temp file is removed.
func writeTemp(dir, name string, mode fs.FileMode, uid, gid int, write func(*os.File) error) (path string, err error) {
	removeStale(dir, name)
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

func writeData(data []byte) func(*os.File) error {
	return func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}
}

// WriteAtomic replaces dir/name with data: a temp file in the same
// directory, fsync, fchown and fchmod, rename over the target, then an fsync
// of the directory so the rename itself is durable. A reader sees the old
// file or the new one, never a mix. A target that is not a regular file is
// refused.
func WriteAtomic(dir, name string, data []byte, mode fs.FileMode, uid, gid int) error {
	path := filepath.Join(dir, name)
	if _, err := regularTarget(path); err != nil {
		return err
	}
	tmp, err := writeTemp(dir, name, mode, uid, gid, writeData(data))
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return durable(dir)
}

// ReplaceIfHash is WriteAtomic guarded by a base hash: dir/name is re-read
// and re-hashed immediately before the rename, after the new content is
// already on disk, so a hand edit made at any point up to then is detected
// and refused with a *ConflictError. What is left is the window between that
// re-hash and the rename. An existing file keeps its mode and owner; one
// that is not a regular file is refused.
//
// An empty expectSHA means the file must not exist yet. It is then created
// 0644 with the caller's ids, by link(2), which fails rather than overwrite
// a file that appeared in the meantime, so that case has no window at all.
func ReplaceIfHash(dir, name, expectSHA string, data []byte) error {
	path := filepath.Join(dir, name)
	mode, uid, gid := fs.FileMode(0o644), -1, -1
	st, err := regularTarget(path)
	if err != nil {
		return err
	}
	if st != nil {
		mode = st.Mode().Perm()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	}
	tmp, err := writeTemp(dir, name, mode, uid, gid, writeData(data))
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // a no-op once renamed
	beforeRehash()
	current := func() (string, error) {
		if _, err := regularTarget(path); err != nil {
			return "", err
		}
		_, sha, err := ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return sha, err
	}
	if expectSHA == "" {
		if err := os.Link(tmp, path); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			sha, err := current()
			if err != nil {
				return err
			}
			return &ConflictError{Path: path, Current: sha}
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
		return durable(dir)
	}
	sha, err := current()
	if err != nil {
		return err
	}
	if sha != expectSHA {
		return &ConflictError{Path: path, Current: sha}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return durable(dir)
}

// MoveAcross moves the regular file src into dstDir under the same name,
// refusing to overwrite. On one filesystem it links src into dstDir and
// unlinks it; link(2), unlike rename(2), fails if the target exists. When
// src and dstDir are on different mounts (EXDEV, which bind mounts under
// ProtectSystem=strict also give) it copies to a temp file, fsyncs it, links
// it into place, fsyncs dstDir, and only then unlinks src and fsyncs its
// directory. Times are kept, since mhVTL compares library_contents mtimes
// (mhvtl usr/cmd/vtllibrary.c:980), and so is the owner when running as
// root.
//
// The copy is for small configuration files only: it keeps Mode().Perm(),
// so setuid, setgid and sticky bits are dropped, and io.Copy writes holes
// out and loses xattrs and ACLs. Phase 5's media moves must not reuse it.
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
	err = link(src, dst)
	if err == nil {
		if err := os.Remove(src); err != nil {
			return err
		}
		return durable(dstDir, filepath.Dir(src))
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
	if err := os.Link(tmp, dst); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := durable(dstDir); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	return durable(filepath.Dir(src))
}
