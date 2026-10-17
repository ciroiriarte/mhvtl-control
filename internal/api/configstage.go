package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// stageTTL is how long a stage lives after its last use; maxStages bounds
// the stages held at once. Stages are in memory only: a daemon restart
// drops them, and the client stages its change again.
const (
	stageTTL  = 30 * time.Minute
	maxStages = 32
)

var (
	errStageNotFound = errors.New("no such stage, or it expired: stage the change again")
	errTooManyStages = fmt.Errorf("%d stages are open; apply one or let it expire (%v)", maxStages, stageTTL)
)

// stage is a change set the write routes build and POST /config/apply sends
// to the helper (plan 4.1). It starts from the configuration on disk when
// it is made. The first edit of a library writes that library's live
// placement into its library_contents (liveContents), so the change does
// not revert run-time moves (plan, amendment "Phase 4 client contract for
// next"); a library no edit touches is sent as it is, if at all.
type stage struct {
	// mu is held by the request using the stage, an apply included.
	mu sync.Mutex
	id string
	// expires is guarded by stages.mu, so a sweep never waits for an
	// apply.
	expires time.Time
	// cur is the configuration on disk when the stage was made; disk and
	// base its files and their hashes ("" for none).
	cur  config.Inventory
	disk map[string][]byte
	base map[string]string
	// next holds the files the stage has edited; a nil value deletes the
	// file.
	next         map[string][]byte
	dispositions map[string]validate.Disposition // by library and barcode
	// rev counts edits; plan is the last dry run, valid while its rev is
	// rev.
	rev  int
	plan *stagePlan
}

// stagePlan is what a dry run returned that the apply binds: the units the
// helper derived, which the apply expects (with the vtltape@ of the drives
// a forced apply unloads, mhvtl-control#58).
type stagePlan struct {
	rev   int
	units []string
}

// stages holds the open stages.
type stages struct {
	mu sync.Mutex
	m  map[string]*stage
}

// newStage makes a stage from the configuration on disk, and says when it
// expires.
func (s *Server) newStage() (*stage, time.Time, error) {
	snap, err := s.readConfig()
	if err != nil {
		return nil, time.Time{}, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, time.Time{}, err
	}
	st := &stage{
		id:           hex.EncodeToString(b),
		cur:          snap.inv,
		disk:         snap.files,
		base:         map[string]string{},
		next:         map[string][]byte{},
		dispositions: map[string]validate.Disposition{},
	}
	for name, data := range snap.files {
		st.base[name] = conffile.Hash(data)
	}
	c := &s.stages
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	for id, o := range c.m {
		if !now.Before(o.expires) {
			delete(c.m, id)
		}
	}
	if len(c.m) >= maxStages {
		return nil, time.Time{}, errTooManyStages
	}
	if c.m == nil {
		c.m = map[string]*stage{}
	}
	st.expires = now.Add(stageTTL)
	c.m[st.id] = st
	return st, st.expires, nil
}

// openStage returns stage id, or a new one when id is empty, locked (the
// caller unlocks it), and when it now expires.
func (s *Server) openStage(id string) (*stage, time.Time, error) {
	if id == "" {
		st, expires, err := s.newStage()
		if err != nil {
			return nil, time.Time{}, err
		}
		st.mu.Lock()
		return st, expires, nil
	}
	c := &s.stages
	c.mu.Lock()
	st := c.m[id]
	now := s.now()
	if st != nil && !now.Before(st.expires) {
		delete(c.m, id)
		st = nil
	}
	if st == nil {
		c.mu.Unlock()
		return nil, time.Time{}, errStageNotFound
	}
	st.expires = now.Add(stageTTL)
	expires := st.expires
	c.mu.Unlock()
	st.mu.Lock()
	return st, expires, nil
}

func (s *Server) dropStage(id string) {
	s.stages.mu.Lock()
	delete(s.stages.m, id)
	s.stages.mu.Unlock()
}

// file returns the stage's content of name: as edited, else as on disk; ok
// is false when the file does not exist in the stage.
func (st *stage) file(name string) (data []byte, ok bool) {
	if b, edited := st.next[name]; edited {
		return b, b != nil
	}
	b, ok := st.disk[name]
	return b, ok
}

// inventory parses the stage's configuration.
func (st *stage) inventory(dir string) (config.Inventory, error) {
	inv, err := config.ReadInventory(dir, func(name string) ([]byte, error) {
		if b, ok := st.file(name); ok {
			return b, nil
		}
		return nil, fs.ErrNotExist
	})
	var dce *config.DeviceConfError
	if errors.As(err, &dce) && errors.Is(err, fs.ErrNotExist) {
		return config.Inventory{}, nil
	}
	return inv, err
}

// deviceConf parses the stage's device.conf; one that does not exist yet is
// a new one.
func (st *stage) deviceConf() (*conffile.DeviceConf, error) {
	data, ok := st.file("device.conf")
	if !ok {
		data = []byte("VERSION: 5\n")
	}
	c := conffile.ParseDeviceConf(data)
	return c, c.Writable()
}

// contents parses library id's library_contents in the stage. The first
// time, the library's live placement is written in when it is known.
func (s *Server) contents(ctx context.Context, st *stage, id int) (*conffile.LibContents, error) {
	name := contentsFile(id)
	if _, edited := st.next[name]; !edited {
		if err := s.adopt(st, name); err != nil {
			return nil, err
		}
		data, ok := st.disk[name]
		if lib, inCur := findConfigLibrary(st.cur, id); ok && inCur {
			live, _ := s.readLive(ctx, []config.Library{lib})
			if p, known := live[id]; known {
				lc, err := liveContents(data, lib, p)
				if err != nil {
					return nil, fmt.Errorf("live placement not written into %s: %w", name, err)
				}
				data = lc
			}
		}
		if data == nil {
			data = []byte{} // nil would delete it
		}
		st.next[name] = data
	}
	c := conffile.ParseLibContents(st.next[name])
	return c, c.Writable()
}

// adopt reads the base of a library_contents the stage has not read: that
// of a library device.conf does not have, which mhVTL would read whatever
// file of that name is there.
func (s *Server) adopt(st *stage, name string) error {
	if _, known := st.base[name]; known {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.ConfigDir, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st.base[name] = ""
	case err != nil:
		return err
	default:
		st.base[name], st.disk[name] = conffile.Hash(b), b
	}
	return nil
}

func contentsFile(id int) string { return fmt.Sprintf("library_contents.%d", id) }

// writable checks the files the stage edited, as every conffile writer
// must (conffile.DeviceConf.Writable).
func (st *stage) writable() error {
	for name, data := range st.next {
		var err error
		switch {
		case data == nil:
		case name == "device.conf":
			err = conffile.ParseDeviceConf(data).Writable()
		default:
			err = conffile.ParseLibContents(data).Writable()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// edited records an edit: the plan, if any, no longer stands.
func (st *stage) edited() { st.rev++ }

// changes are the files the stage writes and deletes: those that differ
// from the disk.
func (st *stage) changes() (files []apply.File, del []apply.Delete) {
	names := make([]string, 0, len(st.next))
	for name := range st.next {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := st.next[name]
		disk, onDisk := st.disk[name]
		switch {
		case data == nil && onDisk:
			del = append(del, apply.Delete{Name: name, BaseSHA256: st.base[name]})
		case data != nil && (!onDisk || !bytes.Equal(data, disk)):
			files = append(files, apply.File{Name: name, BaseSHA256: st.base[name], Content: data})
		}
	}
	return files, del
}

func (st *stage) dispositionList() []validate.Disposition {
	out := make([]validate.Disposition, 0, len(st.dispositions))
	for _, d := range st.dispositions {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b validate.Disposition) int {
		if a.Library != b.Library {
			return a.Library - b.Library
		}
		if a.Barcode < b.Barcode {
			return -1
		}
		if a.Barcode > b.Barcode {
			return 1
		}
		return 0
	})
	return out
}

func (st *stage) dispose(d validate.Disposition) {
	st.dispositions[fmt.Sprintf("%d/%s", d.Library, d.Barcode)] = d
}

// stageDTO is a stage as the write routes return it: the files it changes
// (null deletes one) and the hash of each as it was read ("" when it did
// not exist).
type stageDTO struct {
	Stage        string                 `json:"stage"`
	Expires      time.Time              `json:"expires"`
	Files        map[string]*string     `json:"files"`
	Base         map[string]string      `json:"base_sha256"`
	Dispositions []validate.Disposition `json:"dispositions"`
}

func (st *stage) dto(expires time.Time) stageDTO {
	d := stageDTO{Stage: st.id, Expires: expires, Files: map[string]*string{}, Base: map[string]string{}, Dispositions: st.dispositionList()}
	files, del := st.changes()
	for _, f := range files {
		c := string(f.Content)
		d.Files[f.Name], d.Base[f.Name] = &c, f.BaseSHA256
	}
	for _, f := range del {
		d.Files[f.Name], d.Base[f.Name] = nil, f.BaseSHA256
	}
	return d
}
