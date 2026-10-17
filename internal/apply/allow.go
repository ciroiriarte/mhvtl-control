package apply

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"

	"mhvtl-control/internal/conffile"
)

// The files the helper may write (plan 3.2, allow.go). Nothing else in the
// configuration directory is ever opened for writing; library_contents.<N>
// .persist is vtllibrary's, and the helper alone moves it (PR 3b).
const (
	deviceConf = "device.conf"
	mhvtlConf  = "mhvtl.conf"
)

var (
	libContentsRE = regexp.MustCompile(`^library_contents\.([0-9]{1,5})$`)
	unitRE        = regexp.MustCompile(`^(vtllibrary|vtltape)@[0-9]{1,5}\.service$`)
	shaRE         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// libraryOf returns N for library_contents.<N>, written as vtllibrary
// names the file it reads ("library_contents.%d"), so with no leading zero;
// ok is false for any other name.
func libraryOf(name string) (n int, ok bool) {
	m := libContentsRE.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || strconv.Itoa(n) != m[1] {
		return 0, false
	}
	return n, true
}

// checkNames refuses a request naming anything but device.conf, mhvtl.conf
// and library_contents.<N>, naming a file twice, carrying a malformed base
// hash, or deleting anything but a library_contents.<N>. Whether N is a
// library is checked once the new device.conf is known (checkLibraries).
func checkNames(req *Request) error {
	seen := map[string]bool{}
	check := func(name, sha string) error {
		if _, ok := libraryOf(name); !ok && name != deviceConf && name != mhvtlConf {
			return refuse(ErrName, "%q is not device.conf, mhvtl.conf or library_contents.<N>", name)
		}
		if seen[name] {
			return refuse(ErrName, "%q named twice", name)
		}
		seen[name] = true
		if sha != "" && !shaRE.MatchString(sha) {
			return refuse(ErrBadRequest, "%q: base_sha256 %q is not a lower-case hex SHA-256", name, sha)
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
		if _, ok := libraryOf(d.Name); !ok {
			return refuse(ErrName, "%q cannot be deleted; only library_contents.<N> can", d.Name)
		}
		if d.BaseSHA256 == "" {
			return refuse(ErrBadRequest, "%q: deleting needs the base_sha256 of the file", d.Name)
		}
	}
	return nil
}

// checkLibraries refuses a library_contents.<N> written for a library that
// neither the current nor the new device.conf has, and one deleted for a
// library the new device.conf still has.
func checkLibraries(req *Request, cur, next map[int]bool) error {
	for _, f := range req.Files {
		if n, ok := libraryOf(f.Name); ok && !cur[n] && !next[n] {
			return refuse(ErrName, "%q: no library %d in device.conf", f.Name, n)
		}
	}
	for _, d := range req.Delete {
		n, _ := libraryOf(d.Name)
		if next[n] {
			return refuse(ErrName, "%q: library %d stays in device.conf", d.Name, n)
		}
	}
	return nil
}

// checkUnits refuses an expect_units entry that is not a vtllibrary@N or
// vtltape@N instance, or one listed twice. Whether the set matches the
// helper's own derivation is checkExpectUnits'.
func checkUnits(units []string) error {
	seen := map[string]bool{}
	for _, u := range units {
		if !unitRE.MatchString(u) {
			return refuse(ErrUnit, "%q is not a vtllibrary@N or vtltape@N service", u)
		}
		if seen[u] {
			return refuse(ErrUnit, "%q listed twice", u)
		}
		seen[u] = true
	}
	return nil
}

// fileState is a configuration file as read: absent, or its bytes and
// hash.
type fileState struct {
	exists bool
	data   []byte
	sha    string // "" when absent
}

// readTarget reads dir/name without following a symlink (O_NOFOLLOW) and
// refuses anything but a regular file of at most MaxFileSize bytes. A FIFO
// is opened non-blocking so it cannot stall the helper, then refused.
func readTarget(dir, name string) (fileState, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return fileState{}, refuse(ErrName, "%s is a symbolic link", path)
	}
	if err != nil {
		return fileState{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fileState{}, err
	}
	if !st.Mode().IsRegular() {
		return fileState{}, refuse(ErrName, "%s is not a regular file (%v)", path, st.Mode().Type())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return fileState{}, err
	}
	if len(data) > MaxFileSize {
		return fileState{}, refuse(ErrOversize, "%s is larger than %d bytes", path, MaxFileSize)
	}
	return fileState{exists: true, data: data, sha: conffile.Hash(data)}, nil
}

// fileSet is the configuration directory as the helper sees it: the files
// on disk, read once each, with the request's changes laid over them when
// changes is set.
type fileSet struct {
	dir     string
	disk    map[string]fileState
	changes map[string]*fileState // nil value: deleted
}

func newFileSet(dir string) *fileSet {
	return &fileSet{dir: dir, disk: map[string]fileState{}}
}

// current returns name as on disk, reading it the first time.
func (s *fileSet) current(name string) (fileState, error) {
	if st, ok := s.disk[name]; ok {
		return st, nil
	}
	st, err := readTarget(s.dir, name)
	if err != nil {
		return fileState{}, err
	}
	s.disk[name] = st
	return st, nil
}

// next returns name as the request would leave it.
func (s *fileSet) next(name string) (fileState, error) {
	if c, ok := s.changes[name]; ok {
		if c == nil {
			return fileState{}, nil
		}
		return *c, nil
	}
	return s.current(name)
}

// withChanges returns the request's view of s.
func (s *fileSet) withChanges(req *Request) *fileSet {
	n := &fileSet{dir: s.dir, disk: s.disk, changes: map[string]*fileState{}}
	for _, f := range req.Files {
		n.changes[f.Name] = &fileState{exists: true, data: f.Content, sha: conffile.Hash(f.Content)}
	}
	for _, d := range req.Delete {
		n.changes[d.Name] = nil
	}
	return n
}

// reader adapts a view for config.ReadInventory: a missing file is
// fs.ErrNotExist.
func reader(get func(string) (fileState, error)) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		st, err := get(name)
		if err != nil {
			return nil, err
		}
		if !st.exists {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		return st.data, nil
	}
}
