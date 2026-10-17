package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"mhvtl-control/internal/conffile"
)

const (
	rollbackDir  = "rollback"
	keepRollback = 50
	// rollbackStamp names a rollback directory; it sorts by time.
	rollbackStamp = "20060102T150405.000000000Z"
)

// change is one file of a change set.
type change struct {
	name string
	base string // sha before, "" when absent
	data []byte // new content; unused when del
	del  bool
}

// changesOf returns the request's changes in write order (plan 3.2,
// write.go): mhvtl.conf, then the library_contents files written, then
// device.conf, then the library_contents files deleted. device.conf is so
// written after the files of a library it adds and before the removal of
// those of a library it drops, and never names a library whose file is not
// yet in place.
func changesOf(req *Request) []change {
	var conf, libs, dev, dels []change
	for _, f := range req.Files {
		c := change{name: f.Name, base: f.BaseSHA256, data: f.Content}
		switch f.Name {
		case mhvtlConf:
			conf = append(conf, c)
		case deviceConf:
			dev = append(dev, c)
		default:
			libs = append(libs, c)
		}
	}
	for _, d := range req.Delete {
		dels = append(dels, change{name: d.Name, base: d.BaseSHA256, del: true})
	}
	sort.Slice(libs, func(i, j int) bool { return libs[i].name < libs[j].name })
	sort.Slice(dels, func(i, j int) bool { return dels[i].name < dels[j].name })
	return append(append(append(conf, libs...), dev...), dels...)
}

// errKilled is what a kill point returns in tests: the helper stops there
// as if it had died, with no cleanup.
var errKilled = errors.New("killed at a test kill point")

// writeFiles makes the changes durable, one file at a time, behind the
// intent (plan 3.2, write.go; AC 4):
//
//  1. the apply's rollback directory StateDir/rollback/<time>/ (made here
//     unless the caller made it; pruned to the newest 50) gets a copy of
//     every file replaced;
//  2. intent.json records each file's original bytes and both hashes;
//  3. each file is written with conffile.ReplaceIfHash, which re-hashes it
//     immediately before the rename, or deleted with removeIfHash, which
//     moves it into the rollback directory; the intent's step follows.
//
// A conflict or an error at step 3, or ctx ending, restores the files done
// so far by the restore rule and removes the intent, unless the exec
// sequence owns it: that rolls the units back and ends it then. The
// intent is left in place on success: the caller restarts the units and
// ends the apply.
func (h *Helper) writeFiles(ctx context.Context, it *Intent, changes []change, files *fileSet) error {
	rb, err := h.rollbackDir(it)
	if err != nil {
		return err
	}
	for _, c := range changes {
		cur, err := files.current(c.name)
		if err != nil {
			return err
		}
		f := IntentFile{Name: c.name, Existed: cur.exists, Original: cur.data, OriginalSHA: cur.sha, UID: -1, GID: -1}
		if !c.del {
			f.NewSHA = conffile.Hash(c.data)
		}
		if cur.exists {
			if st, err := os.Lstat(filepath.Join(h.cfg.ConfigDir, c.name)); err == nil {
				f.Mode = uint32(st.Mode().Perm())
				if sys, ok := st.Sys().(*syscall.Stat_t); ok {
					f.UID, f.GID = int(sys.Uid), int(sys.Gid)
				}
			}
			if !c.del {
				if err := conffile.WriteAtomic(rb, c.name, cur.data, 0o600, -1, -1); err != nil {
					return err
				}
			}
		}
		it.Files = append(it.Files, f)
	}
	it.Step = "intent"
	if err := h.saveIntent(it); err != nil {
		return err
	}
	if h.killPoint(it.Step) {
		return errKilled
	}
	for _, c := range changes {
		err := ctx.Err()
		if err == nil {
			h.beforeWrite(c.name)
			if c.del {
				err = removeIfHash(h.cfg.ConfigDir, c.name, c.base, rb)
			} else {
				err = conffile.ReplaceIfHash(h.cfg.ConfigDir, c.name, c.base, c.data)
			}
		}
		if err != nil {
			// The intent stays when a file could not be restored, so
			// that recovery retries it, and in the exec sequence, whose
			// rollback ends it.
			conflicts, rerr := h.restore(it)
			if rerr != nil || it.Exec != nil {
				return errors.Join(err, rerr, conflictsError(conflicts))
			}
			return errors.Join(err, conflictsError(conflicts), h.removeIntent())
		}
		it.Step = "wrote " + c.name
		if err := h.saveIntent(it); err != nil {
			return err
		}
		if h.killPoint(it.Step) {
			return errKilled
		}
	}
	return nil
}

// rollbackDir returns the apply's directory under StateDir/rollback,
// creating it (and pruning the old ones) the first time.
func (h *Helper) rollbackDir(it *Intent) (string, error) {
	if it.RollbackDir != "" {
		return filepath.Join(h.cfg.StateDir, rollbackDir, it.RollbackDir), nil
	}
	rb := filepath.Join(h.cfg.StateDir, rollbackDir, h.cfg.Now().UTC().Format(rollbackStamp))
	if err := os.MkdirAll(rb, 0o700); err != nil {
		return "", err
	}
	h.pruneRollback()
	it.RollbackDir = filepath.Base(rb)
	return rb, nil
}

// pruneRollback keeps the newest keepRollback rollback directories.
// Entries that are not directories named by rollbackStamp are left alone:
// recovery never reads them back (it restores from intent.json).
func (h *Helper) pruneRollback() {
	dir := filepath.Join(h.cfg.StateDir, rollbackDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && len(e.Name()) == len(rollbackStamp) && strings.HasSuffix(e.Name(), "Z") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepRollback {
		os.RemoveAll(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// restore puts back every file of the intent the apply changed, by the
// restore rule (plan 3.2): a file is restored only when its hash is the
// journaled post-write hash; one with the original hash is left as it is;
// any other hash means someone changed it after the apply (a hand fix, a
// mhvtl-move-home switch), and it is left untouched and reported. Files go
// back in the reverse of the write order.
func (h *Helper) restore(it *Intent) ([]Conflict, error) {
	var conflicts []Conflict
	var errs []error
	dir := h.cfg.ConfigDir
	for i := len(it.Files) - 1; i >= 0; i-- {
		f := it.Files[i]
		cur, err := readTarget(dir, f.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch {
		case cur.sha == f.OriginalSHA:
			continue
		case cur.sha != f.NewSHA:
			conflicts = append(conflicts, Conflict{File: f.Name, CurrentSHA256: cur.sha})
			continue
		case !f.Existed:
			err = removeIfHash(dir, f.Name, f.NewSHA, "")
		default:
			err = conffile.ReplaceIfHash(dir, f.Name, f.NewSHA, f.Original)
			if err == nil && f.NewSHA == "" && f.Mode != 0 {
				// Re-created by link(2) as 0644 and owned by root: give
				// it back the original's mode and owner.
				path := filepath.Join(dir, f.Name)
				err = errors.Join(os.Chmod(path, os.FileMode(f.Mode)), os.Lchown(path, f.UID, f.GID))
			}
		}
		if c, ok := conflictOf(err); ok {
			conflicts = append(conflicts, Conflict{File: f.Name, CurrentSHA256: c.Current})
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %w", f.Name, err))
		}
	}
	return conflicts, errors.Join(errs...)
}

// restoreConflicts reports the files a restore left alone.
type restoreConflicts []Conflict

func (c restoreConflicts) Error() string {
	var names []string
	for _, f := range c {
		names = append(names, f.File)
	}
	return "changed since the apply, not restored: " + strings.Join(names, ", ")
}

// conflictsError is nil without conflicts.
func conflictsError(c []Conflict) error {
	if len(c) == 0 {
		return nil
	}
	return restoreConflicts(c)
}
