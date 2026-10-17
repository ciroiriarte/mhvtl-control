package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/conffile"
)

// The helper's own state (plan 3.2, intent.go). Both directories are the
// helper's alone: its unit creates them 0700, and mhvtl-control.service
// masks them (InaccessiblePaths=), so the daemon can neither read nor plant
// anything the helper reads back.
const (
	intentFile = "intent.json"
	lockFile   = "exclusive.lock"
	// maxIntent bounds intent.json: MaxFiles originals in base64, and room
	// for what PR 3b journals.
	maxIntent = 32 << 20
)

// Intent is the write-ahead record of an apply, in StateDir/intent.json,
// written atomically (temp, fsync, rename, directory fsync) before the
// first change and after every step. While it exists and exclusive.lock is
// free, the apply that wrote it is dead, and the next plan, apply or recover
// recovers it before doing anything else.
type Intent struct {
	V int `json:"v"`
	// BootID is /proc/sys/kernel/random/boot_id when the apply began:
	// boot recovery acts only on an apply of an earlier boot.
	BootID  string    `json:"boot_id"`
	Started time.Time `json:"started"`
	Actor   string    `json:"actor"`
	Peer    Peer      `json:"peer"`
	Op      string    `json:"op"`
	// Step is the last step done.
	Step string `json:"step"`
	// RollbackDir is the apply's directory under StateDir/rollback.
	RollbackDir string       `json:"rollback_dir"`
	Files       []IntentFile `json:"files"`
	// Libraries are the libraries whose units the apply took offline,
	// stopped or started. Recovery hands them to Lifecycle before it
	// restores files. Set by PR 3b's exec sequence; 3a's writes leave it
	// empty.
	Libraries []int     `json:"libraries,omitempty"`
	Recovery  *Recovery `json:"recovery,omitempty"`
}

// IntentFile is one file the apply changes, with what it was and what it
// becomes.
type IntentFile struct {
	Name string `json:"name"`
	// Existed is false for a file the apply creates.
	Existed bool `json:"existed"`
	// Original is the file's bytes before the apply (base64).
	Original    []byte `json:"original,omitempty"`
	OriginalSHA string `json:"original_sha256"`
	// Mode, UID and GID are the original's, restored on a deleted file.
	Mode uint32 `json:"mode,omitempty"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// NewSHA is the file's hash after the apply, "" when it deletes it.
	NewSHA string `json:"new_sha256"`
}

// Recovery is the progress of a recovery, kept apart from the apply's
// steps so that a recovery that dies is itself resumed.
type Recovery struct {
	Step string `json:"step"`
}

func (h *Helper) intentPath() string { return filepath.Join(h.cfg.StateDir, intentFile) }

// saveIntent writes it atomically and durably.
func (h *Helper) saveIntent(it *Intent) error {
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(h.cfg.StateDir, intentFile, b, 0o600, -1, -1)
}

// loadIntent returns the intent, nil when there is none.
func (h *Helper) loadIntent() (*Intent, error) {
	it, err := h.readIntent()
	if err != nil {
		return nil, refuse(ErrIntentUnreadable, "%v; recover by hand from the copies under %s, then remove %s", err, filepath.Join(h.cfg.StateDir, rollbackDir), h.intentPath())
	}
	return it, nil
}

func (h *Helper) readIntent() (*Intent, error) {
	f, err := os.OpenFile(h.intentPath(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxIntent+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxIntent {
		return nil, fmt.Errorf("%s: larger than %d bytes", h.intentPath(), maxIntent)
	}
	var it Intent
	if err := json.Unmarshal(b, &it); err != nil {
		return nil, fmt.Errorf("%s: %w", h.intentPath(), err)
	}
	if it.V != Version {
		return nil, fmt.Errorf("%s: version %d, want %d", h.intentPath(), it.V, Version)
	}
	for _, f := range it.Files {
		if _, ok := libraryOf(f.Name); !ok && f.Name != deviceConf && f.Name != mhvtlConf {
			return nil, fmt.Errorf("%s: names %q", h.intentPath(), f.Name)
		}
	}
	return &it, nil
}

// removeIntent ends an apply or a recovery.
func (h *Helper) removeIntent() error {
	if err := os.Remove(h.intentPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(h.cfg.StateDir)
}

// lock is exclusive.lock, held with flock(2) for the whole life of an
// apply, plan or recovery, and by mhvtl-move-home. The kernel releases it
// when the holder dies, which is how a live apply is told from a dead one.
type lock struct{ f *os.File }

// tryLock takes exclusive.lock without waiting; held is true when another
// process has it.
func (h *Helper) tryLock() (l *lock, held bool, err error) {
	f, err := os.OpenFile(filepath.Join(h.cfg.RuntimeDir, lockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return &lock{f}, false, nil
}

// lockWait is how long a plan, apply or recover waits for exclusive.lock
// before it reports apply_in_progress: a hello holds the lock for a moment
// (state), and the daemon's polling must not make an apply fail.
const lockWait = 100 * time.Millisecond

// waitLock is tryLock retried for up to lockWait while another process
// holds the lock.
func (h *Helper) waitLock() (l *lock, held bool, err error) {
	deadline := time.Now().Add(lockWait)
	for {
		l, held, err = h.tryLock()
		if !held || time.Now().After(deadline) {
			return l, held, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (l *lock) release() { l.f.Close() }

// state reports what a hello says. It takes the lock only for a moment to
// see whether someone else holds it, and never recovers.
func (h *Helper) state() (string, error) {
	l, held, err := h.tryLock()
	if err != nil {
		return "", err
	}
	if held {
		return StateInProgress, nil
	}
	defer l.release()
	if it, err := h.readIntent(); err != nil {
		return StateIntentUnreadable, nil
	} else if it != nil {
		return StateIncomplete, nil
	}
	if err := h.checkLegacyEnv(); err != nil {
		var r *refusal
		if errors.As(err, &r) {
			return StateLegacyEnv, nil
		}
		return "", err
	}
	return StateReady, nil
}

// bootID returns the current boot's ID.
func (h *Helper) bootID() (string, error) {
	b, err := os.ReadFile(h.cfg.BootIDPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
