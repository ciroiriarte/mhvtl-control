package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
	"mhvtl-control/internal/validate"
)

// Queue is the mhVTL message queue as the helper uses it
// (ipc.IPCController in production).
type Queue interface {
	Offline(lib int64) error
	Online(lib int64) error
	ListMap(lib int64) (string, error)
}

// Devices is the helper's changer and drive access (SCSIAccess in
// production).
type Devices interface {
	Locate(serial string, typ uint8) (hctl string, err error)
	Inventory(lib config.Library, hctl string) (changer.Inventory, error)
	Loaded(hctl string) (bool, error)
}

// StopMode is who calls guardedStop. They share it so the three cannot
// diverge (plan 3.3, guarded.go, item 11).
type StopMode string

const (
	ModeApply    StopMode = "apply"
	ModeRollback StopMode = "rollback"
	ModeRecover  StopMode = "recover"
)

// StopLib is one library for guardedStop: its configuration as the
// running daemon has it, whether PERSIST is on, and the drives of the
// plan to stop with it.
type StopLib struct {
	Lib     config.Library
	Persist bool
	Drives  []int
}

// Snapshot is what guardedStop records of library_contents.N.persist.
type Snapshot struct {
	Exists  bool   `json:"exists"`
	Size    int64  `json:"size,omitempty"`
	MtimeNS int64  `json:"mtime_ns,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// ForcedUnload is a drive found holding a cartridge in rollback or
// recover mode: its cartridge goes back to its source element and its
// vtltape@ is stopped too.
type ForcedUnload struct {
	Library    int    `json:"library"`
	Drive      int    `json:"drive"`
	Barcode    string `json:"barcode,omitempty"`
	SourceSlot int    `json:"source_slot,omitempty"`
	SourceMAP  int    `json:"source_map,omitempty"`
}

// Placement sources.
const (
	SourcePersist = "persist"          // a trusted .persist
	SourceLive    = "live"             // R1′, READ ELEMENT STATUS before the stop
	SourceFile    = "library_contents" // inactive library: the file vtllibrary would load
)

// LibStop is what guardedStop found and did for one library.
type LibStop struct {
	ID int `json:"id"`
	// Active is false when the library's unit was inactive or failed: no
	// R1′, offline or TUR then.
	Active bool `json:"active"`
	// HCTL is the changer's H:C:T:L at R1′; DriveHCTL the drives'.
	HCTL      string         `json:"hctl,omitempty"`
	DriveHCTL map[int]string `json:"drive_hctl,omitempty"`
	// R1 is R1′, and Map the "list map" reply that came with it.
	R1  *validate.Placement `json:"r1,omitempty"`
	Map string              `json:"map,omitempty"`
	// Offline is set once the library was taken offline.
	Offline       bool           `json:"offline"`
	ForcedUnloads []ForcedUnload `json:"forced_unloads,omitempty"`
	// Stopped are the units stopped, in order.
	Stopped []string `json:"stopped,omitempty"`
	// Result is the library unit's systemd Result= after the stop.
	Result string `json:"result,omitempty"`
	// Before and After snapshot .persist around the stop; Fresh is a
	// change between them, Trusted a fresh one with Result=success.
	Before  Snapshot `json:"persist_before"`
	After   Snapshot `json:"persist_after"`
	Fresh   bool     `json:"persist_fresh"`
	Trusted bool     `json:"persist_trusted"`
	// PersistData are the bytes of a trusted .persist, or of the .persist
	// an inactive library would load.
	PersistData []byte `json:"persist_data,omitempty"`
	// Aside is where .persist was moved, "" when there was none.
	Aside string `json:"persist_aside,omitempty"`
	// Source is the placement to build on, and SourceKind where it came
	// from.
	Source     validate.Placement `json:"source"`
	SourceKind string             `json:"source_kind"`
}

// ErrRaced means a library changed between R1′ and the stop: the MAP
// barrier disagreed, or a drive R1′ showed empty holds a cartridge.
var ErrRaced = errors.New("raced")

// stopper is guardedStop's view of the system.
type stopper struct {
	cfg       *Config
	configDir string
	aside     string
	mode      StopMode
	// journal runs once per library after its verdict and before .persist
	// is moved aside, so the verdict and the bytes are durable first.
	journal func(*LibStop) error
}

// guardedStop takes libs down for mode (plan 3.3, guarded.go):
//
//  1. an inactive or failed library skips R1′, offline and TUR; its
//     source is the file vtllibrary would load;
//  2. R1′: READ ELEMENT STATUS and "list map";
//  3. offline, then "list map" again, which must equal R1′'s, else raced;
//  4. TEST UNIT READY on each drive: a cartridge where R1′ showed none is
//     raced in apply mode; in rollback and recover any loaded drive is a
//     forced unload and its vtltape@ is stopped too;
//  5. snapshot .persist;
//  6. stop the library's units, library first (amendment 1), and read its
//     Result=;
//  7. verdict: .persist is fresh if the snapshot changed, trusted if fresh
//     and Result=success; journal, then move any .persist aside, trusted or
//     not, fresh or not, because a .persist left in place wins over the
//     merged library_contents at vtllibrary.c:980;
//  8. return the source: the trusted .persist, else R1′.
//
// Steps 1-5 run for every library before any is stopped, so a race found
// in one stops none. A library taken offline and not stopped is put back
// online on every return, error or panic.
func (s *stopper) guardedStop(ctx context.Context, libs []StopLib) (out []*LibStop, err error) {
	defer func() {
		for _, ls := range out {
			if ls.Offline && len(ls.Stopped) == 0 {
				if oerr := s.cfg.Queue.Online(int64(ls.ID)); oerr != nil {
					s.cfg.Logf("library %d: online after %v: %v", ls.ID, err, oerr)
				}
			}
		}
	}()
	for _, sl := range libs {
		ls := &LibStop{ID: sl.Lib.ID}
		out = append(out, ls)
		if err := s.prepare(ctx, sl, ls); err != nil {
			return out, err
		}
	}
	for i, sl := range libs {
		if err := s.stop(ctx, sl, out[i]); err != nil {
			return out, err
		}
	}
	return out, nil
}

// prepare runs steps 1-5 for one library.
func (s *stopper) prepare(ctx context.Context, sl StopLib, ls *LibStop) error {
	state, err := s.show(ctx, libraryUnit(sl.Lib.ID), "ActiveState")
	if err != nil {
		return err
	}
	ls.Active = state != "inactive" && state != "failed"
	if !ls.Active {
		return s.snapshot(sl, ls, &ls.Before)
	}
	devs := s.cfg.Devices
	if ls.HCTL, err = devs.Locate(sl.Lib.Serial, scsi.PeripheralMediumChanger); err != nil {
		return fmt.Errorf("library %d: %w", sl.Lib.ID, err)
	}
	inv, err := devs.Inventory(sl.Lib, ls.HCTL)
	if err != nil {
		return fmt.Errorf("library %d: READ ELEMENT STATUS: %w", sl.Lib.ID, err)
	}
	r1 := placementOf(inv)
	ls.R1 = &r1
	if ls.Map, err = s.cfg.Queue.ListMap(int64(sl.Lib.ID)); err != nil {
		return fmt.Errorf("library %d: list map: %w", sl.Lib.ID, err)
	}
	if err := s.cfg.Queue.Offline(int64(sl.Lib.ID)); err != nil {
		return fmt.Errorf("library %d: offline: %w", sl.Lib.ID, err)
	}
	ls.Offline = true
	// The barrier: vtllibrary handles messages in order, so a reply to a
	// "list map" sent after "offline" means the offline took effect.
	barrier, err := s.cfg.Queue.ListMap(int64(sl.Lib.ID))
	if err != nil {
		return fmt.Errorf("library %d: list map after offline: %w", sl.Lib.ID, err)
	}
	if barrier != ls.Map {
		return fmt.Errorf("library %d: %w: the MAP changed between READ ELEMENT STATUS (%q) and offline (%q)", sl.Lib.ID, ErrRaced, ls.Map, barrier)
	}
	ls.DriveHCTL = map[int]string{}
	for _, d := range sl.Lib.Drives {
		hctl, err := devs.Locate(d.Serial, scsi.PeripheralSequential)
		if err != nil {
			return fmt.Errorf("library %d drive %d: %w", sl.Lib.ID, d.ID, err)
		}
		ls.DriveHCTL[d.ID] = hctl
		loaded, err := devs.Loaded(hctl)
		if err != nil {
			return fmt.Errorf("library %d drive %d: %w", sl.Lib.ID, d.ID, err)
		}
		if !loaded {
			continue
		}
		n := d.Slot
		st := driveState(inv, n)
		if s.mode == ModeApply {
			if st == nil || !st.Full {
				return fmt.Errorf("library %d drive %d: %w: it holds a cartridge READ ELEMENT STATUS did not show", sl.Lib.ID, d.ID, ErrRaced)
			}
			continue
		}
		fu := ForcedUnload{Library: sl.Lib.ID, Drive: d.ID}
		if st != nil {
			fu.Barcode, fu.SourceSlot, fu.SourceMAP = st.Barcode, st.SourceSlot, st.SourceMAP
		}
		ls.ForcedUnloads = append(ls.ForcedUnloads, fu)
	}
	return s.snapshot(sl, ls, &ls.Before)
}

// stop runs steps 6-8 for one library.
func (s *stopper) stop(ctx context.Context, sl StopLib, ls *LibStop) error {
	units := []string{libraryUnit(sl.Lib.ID)}
	drives := map[int]bool{}
	for _, d := range sl.Drives {
		drives[d] = true
	}
	for _, fu := range ls.ForcedUnloads {
		drives[fu.Drive] = true
	}
	for _, d := range sl.Lib.Drives {
		if drives[d.ID] {
			units = append(units, driveUnit(d.ID))
			delete(drives, d.ID)
		}
	}
	for d := range drives { // plan drives no longer in the library's stanzas
		units = append(units, driveUnit(d))
	}
	for _, u := range units {
		if _, err := s.cfg.Systemctl(ctx, "stop", u); err != nil {
			return fmt.Errorf("systemctl stop %s: %w", u, err)
		}
		ls.Stopped = append(ls.Stopped, u)
	}
	var err error
	if ls.Result, err = s.show(ctx, libraryUnit(sl.Lib.ID), "Result"); err != nil {
		return err
	}
	if err := s.snapshot(sl, ls, &ls.After); err != nil {
		return err
	}
	ls.Fresh = ls.After != ls.Before
	ls.Trusted = ls.Active && ls.Fresh && ls.Result == "success"
	persist := filepath.Join(s.configDir, persistName(sl.Lib.ID))

	switch {
	case ls.Trusted:
		data, err := os.ReadFile(persist)
		if err != nil {
			return err
		}
		if conffile.Hash(data) != ls.After.SHA256 {
			return fmt.Errorf("library %d: %s changed after the stop", sl.Lib.ID, persist)
		}
		ls.PersistData = data
		ls.Source, err = persistPlacement(data)
		if err != nil {
			return fmt.Errorf("library %d: %s: %w", sl.Lib.ID, persist, err)
		}
		ls.SourceKind = SourcePersist
	case ls.Active:
		ls.Source, ls.SourceKind = *ls.R1, SourceLive
	default:
		if err := s.fileSource(sl, ls); err != nil {
			return err
		}
	}
	if err := s.journal(ls); err != nil {
		return err
	}
	if ls.After.Exists {
		if err := conffile.MoveAcross(persist, s.aside); err != nil {
			return fmt.Errorf("library %d: moving %s aside: %w", sl.Lib.ID, persist, err)
		}
		ls.Aside = filepath.Join(s.aside, persistName(sl.Lib.ID))
	}
	return nil
}

// fileSource is the source of an inactive library: the file vtllibrary
// would load at its next start, by the rule of __init_slot_info, mhvtl
// usr/cmd/vtllibrary.c:958-991 at d65d25f
// (https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/cmd/vtllibrary.c#L958-L991):
// .persist when PERSIST is on, it exists, and library_contents.N is not
// newer by st_mtime in whole seconds; else library_contents.N.
func (s *stopper) fileSource(sl StopLib, ls *LibStop) error {
	ls.SourceKind = SourceFile
	if sl.Persist && ls.After.Exists {
		lc, err := os.Stat(filepath.Join(s.configDir, contentsName(sl.Lib.ID)))
		if err != nil {
			return err
		}
		if lc.ModTime().Unix() <= ls.After.MtimeNS/1e9 {
			data, err := os.ReadFile(filepath.Join(s.configDir, persistName(sl.Lib.ID)))
			if err != nil {
				return err
			}
			ls.PersistData = data
			ls.Source, err = persistPlacement(data)
			ls.SourceKind = SourcePersist
			return err
		}
	}
	p := newPlacement()
	for _, sl := range sl.Lib.Slots {
		if sl.Barcode != "" {
			p.Slots[sl.Number] = sl.Barcode
		}
	}
	for _, m := range sl.Lib.MAPs {
		if m.Barcode != "" {
			p.MAPs[m.Number] = m.Barcode
		}
	}
	for _, pk := range sl.Lib.PickerMedia {
		p.Pickers[pk.Number] = pk.Barcode
	}
	ls.Source = p
	return nil
}

// snapshot records .persist: existence, size, st_mtim in nanoseconds and
// sha256 (plan 3.3 step 5).
func (s *stopper) snapshot(sl StopLib, ls *LibStop, snap *Snapshot) error {
	path := filepath.Join(s.configDir, persistName(sl.Lib.ID))
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		*snap = Snapshot{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("library %d: %w", ls.ID, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("library %d: %s is not a regular file", ls.ID, path)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return err
	}
	*snap = Snapshot{Exists: true, Size: st.Size(), MtimeNS: st.ModTime().UnixNano(), SHA256: conffile.Hash(buf.Bytes())}
	return nil
}

// show returns one property of unit.
func (s *stopper) show(ctx context.Context, unit, prop string) (string, error) {
	out, err := s.cfg.Systemctl(ctx, "show", "--property="+prop, "--value", unit)
	if err != nil {
		return "", fmt.Errorf("systemctl show %s %s: %w", prop, unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func persistName(id int) string  { return contentsName(id) + ".persist" }
func contentsName(id int) string { return "library_contents." + strconv.Itoa(id) }

func driveState(inv changer.Inventory, n int) *changer.DriveState {
	if n < 1 || n > len(inv.Drives) {
		return nil
	}
	return &inv.Drives[n-1]
}

func newPlacement() validate.Placement {
	return validate.Placement{Slots: map[int]string{}, MAPs: map[int]string{}, Drives: map[int]string{}, Pickers: map[int]string{}}
}

// placementOf is a READ ELEMENT STATUS as a placement.
func placementOf(inv changer.Inventory) validate.Placement {
	p := newPlacement()
	for _, sl := range inv.Slots {
		if sl.Full && sl.Barcode != "" {
			p.Slots[sl.Number] = sl.Barcode
		}
	}
	for _, sl := range inv.MAPs {
		if sl.Full && sl.Barcode != "" {
			p.MAPs[sl.Number] = sl.Barcode
		}
	}
	for _, d := range inv.Drives {
		if d.Full && d.Barcode != "" {
			p.Drives[d.Number] = d.Barcode
		}
	}
	return p
}

// persistPlacement reads a .persist file: Slot, MAP and Picker lines with
// barcodes padded with blanks, no VERSION line (Phase 0 amendment 8).
// save_config has already unloaded every drive logically, so Drive lines
// carry nothing.
func persistPlacement(data []byte) (validate.Placement, error) {
	_, pickers, maps, slots, problems, err := config.ParseLibraryContents(bytes.NewReader(data))
	if err != nil {
		return validate.Placement{}, err
	}
	if len(problems) > 0 {
		return validate.Placement{}, problems[0]
	}
	p := newPlacement()
	for _, s := range slots {
		if b := strings.TrimSpace(s.Barcode); b != "" {
			p.Slots[s.Number] = b
		}
	}
	for _, m := range maps {
		if b := strings.TrimSpace(m.Barcode); b != "" {
			p.MAPs[m.Number] = b
		}
	}
	for _, pk := range pickers {
		if b := strings.TrimSpace(pk.Barcode); b != "" {
			p.Pickers[pk.Number] = b
		}
	}
	return p, nil
}
