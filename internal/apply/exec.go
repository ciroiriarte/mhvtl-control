package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/scsi"
	"mhvtl-control/internal/validate"
)

// mediaService is mhVTL's oneshot that runs make_vtl_media
// (etc/mhvtl-media.service.in at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/etc/mhvtl-media.service.in).
const mediaService = "mhvtl-media.service"

// ExecJournal is the progress of the apply sequence, in intent.json. Each
// field is journaled before the step it records acts, so that a rollback
// or a recovery knows what to undo.
type ExecJournal struct {
	// Plan is the unit plan carried out, forced drives included.
	Plan UnitPlan `json:"plan"`
	// BayStopped: the apply stopped the bay monitor.
	BayStopped bool `json:"bay_stopped,omitempty"`
	// Offlining are the libraries any guardedStop may have taken offline,
	// each recorded just before its offline; recovery takes every one
	// down and back up, which brings it online.
	Offlining []int `json:"offlining,omitempty"`
	// Prepared is what guardedStop found of each library before its first
	// stop: R1′, the MAP, the drives, and that it is offline.
	Prepared []*LibStop `json:"prepared,omitempty"`
	// Stops are guardedStop's verdicts and placement sources, per library.
	Stops []*LibStop `json:"stops,omitempty"`
	// StandaloneStopped are the standalone drives stopped.
	StandaloneStopped []string `json:"standalone_stopped,omitempty"`
	// Forced are the loaded drives a force token let the apply stop.
	Forced []ForcedUnload `json:"forced,omitempty"`
	// Merged is the placement written to each library's library_contents.
	Merged map[int]validate.Placement `json:"merged,omitempty"`
	// NewMedia are the cartridges make_vtl_media created, as home/barcode.
	NewMedia []string `json:"new_media,omitempty"`
	// Disabled are the removed instances that were enabled, Enabled the
	// added ones the apply enabled.
	Disabled []string `json:"disabled,omitempty"`
	Enabled  []string `json:"enabled,omitempty"`
	// Started are the units the apply started, each recorded before its
	// start.
	Started []string `json:"started,omitempty"`
	// Rollback and Recovery are the verdicts of the guardedStop of a
	// rollback and of a recovery (the plan's recovery.persist), kept apart
	// from Stops so neither overwrites the apply's.
	Rollback []*LibStop `json:"rollback,omitempty"`
	Recovery []*LibStop `json:"recovery_persist,omitempty"`
}

// checked is what check found: the parsed configurations, the live
// placement it validated against, and the unit plan.
type checked struct {
	res               *validate.Result
	plan              *UnitPlan
	cur, next         config.Inventory
	curDC, nextDC     *conffile.DeviceConf
	curHome, nextHome string
	nextFiles         *fileSet
	// warnings are the missing cartridges (missingCartridges), found
	// before anything runs.
	warnings []string
	// live is R1, READ ELEMENT STATUS of every running library, and inv
	// the inventories it came from; nil when the helper has no device
	// access (tests of the request checks).
	live *validate.Live
	inv  map[int]changer.Inventory
}

// execError is a failure of a step, with its response code and the unit
// it concerns.
type execError struct {
	code, unit string
	err        error
}

func (e *execError) Error() string { return e.err.Error() }
func (e *execError) Unwrap() error { return e.err }

func fail(code, unit string, err error) error { return &execError{code: code, unit: unit, err: err} }

// codeOf is the response code of a failed step.
func codeOf(err error) string {
	var x *execError
	var r *refusal
	switch {
	case errors.As(err, &x):
		return x.code
	case errors.Is(err, errRaced):
		return ErrRaced
	case errors.As(err, &r):
		return r.code
	}
	return ErrInternal
}

func unitOf(err error) string {
	var x *execError
	if errors.As(err, &x) {
		return x.unit
	}
	return ""
}

// readLive reads R1: READ ELEMENT STATUS of every library of cur whose
// unit runs, through the SG_IO guard. It is the live placement validation
// runs with (plan 3.3, exec step 2) and the one a force token binds.
func (h *Helper) readLive(ctx context.Context, cur config.Inventory) (*validate.Live, map[int]changer.Inventory, error) {
	if h.cfg.Devices == nil {
		return nil, nil, nil
	}
	live := &validate.Live{Libraries: map[int]validate.Placement{}}
	invs := map[int]changer.Inventory{}
	for _, lib := range cur.Libraries {
		state, err := showProp(ctx, &h.cfg, libraryUnit(lib.ID), "ActiveState")
		if err != nil {
			return nil, nil, err
		}
		if state == "inactive" || state == "failed" {
			continue
		}
		hctl, err := h.cfg.Devices.Locate(lib.Serial, scsi.PeripheralMediumChanger)
		if err != nil {
			return nil, nil, refuse(ErrLive, "library %d: %v", lib.ID, err)
		}
		inv, err := h.cfg.Devices.Inventory(lib, hctl)
		if err != nil {
			return nil, nil, refuse(ErrLive, "library %d: READ ELEMENT STATUS: %v", lib.ID, err)
		}
		invs[lib.ID] = inv
		live.Libraries[lib.ID] = placementOf(inv)
	}
	return live, invs, nil
}

// loadedDrives lists the drives the plan stops that hold a cartridge
// (plan 3.3, exec step 2, drive_loaded): every drive of a library that
// restarts or goes, by R1, since a library restart leaves a cartridge
// mounted in vtltape that the restarted library no longer knows of (Phase
// 0 amendment 2); of such a library that is not running, each drive whose
// vtltape@ runs, by TEST UNIT READY, with no barcode; and each standalone
// drive stopped, by TEST UNIT READY.
func (h *Helper) loadedDrives(ctx context.Context, c *checked) (map[int][]changer.DriveState, []int, error) {
	byLib := map[int][]changer.DriveState{}
	if c.live == nil {
		return byLib, nil, nil
	}
	for _, id := range append(append([]int{}, c.plan.Restart...), c.plan.Stop...) {
		lib, _ := findLib(c.cur, id)
		inv, ok := c.inv[id]
		if !ok {
			for _, d := range lib.Drives {
				loaded, err := h.idleLoaded(ctx, d)
				if err != nil {
					return nil, nil, refuse(ErrLive, "library %d drive %d: %v", id, d.ID, err)
				}
				if loaded {
					byLib[id] = append(byLib[id], changer.DriveState{Number: d.Slot, Full: true})
				}
			}
			continue
		}
		for _, d := range lib.Drives {
			if st := driveState(inv, d.Slot); st != nil && st.Full {
				byLib[id] = append(byLib[id], *st)
			}
		}
	}
	var standalone []int
	for _, id := range c.plan.StopStandalone {
		d, ok := standaloneDrive(c.cur, id)
		if !ok {
			continue
		}
		hctl, err := h.cfg.Devices.Locate(d.Serial, scsi.PeripheralSequential)
		if err != nil {
			return nil, nil, refuse(ErrLive, "drive %d: %v", id, err)
		}
		loaded, err := h.cfg.Devices.Loaded(hctl)
		if err != nil {
			return nil, nil, refuse(ErrLive, "drive %d: %v", id, err)
		}
		if loaded {
			standalone = append(standalone, id)
		}
	}
	return byLib, standalone, nil
}

// idleLoaded reports whether drive d, of a library that is not running,
// holds a cartridge: its vtltape@ runs and TEST UNIT READY is GOOD.
func (h *Helper) idleLoaded(ctx context.Context, d config.Drive) (bool, error) {
	state, err := showProp(ctx, &h.cfg, driveUnit(d.ID), "ActiveState")
	if err != nil || state != "active" {
		return false, err
	}
	hctl, err := h.cfg.Devices.Locate(d.Serial, scsi.PeripheralSequential)
	if err != nil {
		return false, err
	}
	return h.cfg.Devices.Loaded(hctl)
}

// checkMediaPlan runs the media refusals of plan 3.3 exec step 7 when the
// apply adds pre-filled barcodes. make_vtl_media loops over every library
// of device.conf; without the --per-library-home option of
// markh794/mhvtl#276 (usr/cmd/make_vtl_media.in:43,271 at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L271)
// it creates every library's cartridges in MHVTL_HOME_PATH, so a library
// with a home of its own and pre-filled slots, affected or not, would get
// duplicates there. A MAP pre-fill, which make_vtl_media never creates
// (:313), is refused by validation (media.not_in_home) before this runs.
func (h *Helper) checkMediaPlan(c *checked) error {
	if c.plan.NewBarcodes == 0 {
		return nil
	}
	// make_vtl_media skips only an existing directory
	// ([usr/cmd/make_vtl_media.in:365](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L365)),
	// so it runs mktape on a bay cartridge's symlink in a Slot line.
	// mktape takes the raw backend for it
	// ([usr/vtlcart.c:168-179](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/vtlcart.c#L168-L179))
	// and, when the exclusive open succeeds, finds the cartridge existing
	// and writes nothing
	// ([usr/vtlcart_raw.c:575-585](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/vtlcart_raw.c#L575-L585)).
	// The open fails when the device is busy or has no valid superblock
	// ([:238](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/vtlcart_raw.c#L238)); mktape then fails,
	// and make_vtl_media exits before creating the cartridges after it
	// ([usr/cmd/make_vtl_media.in:380](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L380)).
	for _, lib := range c.next.Libraries {
		home := homeOf(lib, c.nextHome)
		bay := bayIn(home)
		for _, s := range lib.Slots {
			if bay(s.Barcode) {
				return refuse(ErrMedia, "library %d slot %d: %s is a bay cartridge (a symlink in %s); make_vtl_media, which the apply runs for the new barcodes, runs mktape on it, which writes nothing to it but fails when its device is busy or has no valid superblock, and make_vtl_media then stops before creating the cartridges after it; move it back to the MAP first, or add the new barcodes in an apply of their own once it is there", lib.ID, s.Number, s.Barcode, home)
			}
		}
	}
	if media.PerLibraryHome(h.cfg.MakeVTLMedia) {
		return nil
	}
	for _, lib := range c.next.Libraries {
		if lib.HomeDir == "" || lib.HomeDir == c.nextHome {
			continue
		}
		for _, s := range lib.Slots {
			if s.Barcode != "" {
				return refuse(ErrMedia, "library %d has its own home %s and pre-filled slots, and this make_vtl_media has no --per-library-home: it would create them in %s; update mhVTL first", lib.ID, lib.HomeDir, c.nextHome)
			}
		}
	}
	return nil
}

// execution is one run of the apply sequence.
type execution struct {
	h    *Helper
	req  *Request
	c    *checked
	plan UnitPlan
	it   *Intent
	rb   string
	// held are the libraries bringUp started and holds offline; a
	// rollback's guardedStop takes them over.
	held []int
}

// execute runs the apply sequence, plan 3.3 exec.go steps 3-12, for a
// request check passed; the caller holds exclusive.lock and apply.lock,
// found no move-home journal planned or unsettled (step 1) and ran the plan checks
// (step 2). forced are the loaded drives a valid force token lets it stop.
//
//  3. stop the bay monitor if it runs and an affected library is a bay
//     target;
//  4. guardedStop(affected, apply); a trusted .persist that disagrees
//     with R1 rolls back (Q7), keeping the .persist placements;
//  5. merge each library's placement source with next (mergeLibrary);
//  6. write the files with ReplaceIfHash, every stopped library with a
//     merged library_contents;
//  7. restart mhvtl-media.service when the apply adds barcodes; a stat of
//     each new cartridge decides, not the exit code;
//  8. daemon-reload when the generator exists, disable the removed
//     instances, and without the generator enable the added ones;
//  9. start, holding each library offline as soon as it answers, then wait
//     for every unit to be active;
//  10. online, restart the bay monitor, and read R2 (advisory drift);
//  11. end the intent.
//
// A failure from step 4 on, once a unit was stopped, rolls back (step 12).
func (h *Helper) execute(ctx context.Context, req *Request, c *checked, forced map[int][]changer.DriveState, rec *AuditRecord) Response {
	x := &execution{h: h, req: req, c: c, plan: withForced(*c.plan, c, forced)}
	boot, err := h.bootID()
	if err != nil {
		return responseOf(err)
	}
	x.it = &Intent{V: Version, BootID: boot, Started: h.cfg.Now().UTC(), Actor: req.Actor, Peer: rec.Peer, Op: req.Op,
		Step: "planned", Libraries: affected(x.plan), Exec: &ExecJournal{Plan: x.plan, Merged: map[int]validate.Placement{}}}
	for _, id := range sortedInts(forced) {
		lib, _ := findLib(c.cur, id)
		for _, st := range forced[id] {
			fu := ForcedUnload{Library: id, Barcode: st.Barcode, SourceSlot: st.SourceSlot, SourceMAP: st.SourceMAP, Unknown: st.Barcode == ""}
			for _, d := range lib.Drives {
				if d.Slot == st.Number {
					fu.Drive = d.ID
				}
			}
			x.it.Exec.Forced = append(x.it.Exec.Forced, fu)
		}
	}
	if x.rb, err = h.rollbackDir(x.it); err != nil {
		return responseOf(err)
	}
	if err := h.saveIntent(x.it); err != nil {
		return responseOf(err)
	}

	// 3. Bay monitor.
	targets, err := bayTargets(h.cfg.ConfigDir)
	if err != nil {
		return x.abort(ctx, err)
	}
	bay := false
	for _, id := range x.it.Libraries {
		bay = bay || targets[id]
	}
	if bay {
		state, err := showProp(ctx, &h.cfg, bayMonitor, "ActiveState")
		if err != nil {
			return x.abort(ctx, err)
		}
		if state == "active" {
			if err := x.step("bay monitor", func() { x.it.Exec.BayStopped = true }); err != nil {
				return x.abort(ctx, err)
			}
			if _, err := h.cfg.Systemctl(ctx, "stop", bayMonitor); err != nil {
				return x.abort(ctx, fmt.Errorf("systemctl stop %s: %w", bayMonitor, err))
			}
		}
	}

	// 4. guardedStop.
	var libs []StopLib
	for _, id := range append(append([]int{}, x.plan.Restart...), x.plan.Stop...) {
		lib, _ := findLib(c.cur, id)
		libs = append(libs, StopLib{Lib: lib, Persist: persistOn(c.curDC, id), Drives: x.plan.StopDrives[id]})
	}
	st := x.stopper(ModeApply, "apply", func(ls *LibStop) { x.it.Exec.Stops = append(x.it.Exec.Stops, ls) })
	st.expect = c.live
	stops, err := st.guardedStop(ctx, libs)
	if err != nil {
		if !anyStopped(stops) {
			return x.abort(ctx, err)
		}
		// A library whose stop failed midway has no verdict, but its
		// units must come back: journal what was stopped.
		journaled := map[*LibStop]bool{}
		for _, ls := range x.it.Exec.Stops {
			journaled[ls] = true
		}
		for _, ls := range stops {
			if len(ls.Stopped) > 0 && !journaled[ls] {
				x.it.Exec.Stops = append(x.it.Exec.Stops, ls)
			}
		}
		if serr := x.h.saveIntent(x.it); serr != nil {
			return responseOf(errors.Join(err, serr))
		}
		return x.rollback(ctx, err)
	}
	for _, id := range x.plan.StopStandalone {
		if err := x.step("stop "+driveUnit(id), func() {
			x.it.Exec.StandaloneStopped = append(x.it.Exec.StandaloneStopped, driveUnit(id))
		}); err != nil {
			return x.rollback(ctx, err)
		}
		if _, err := h.cfg.Systemctl(ctx, "stop", driveUnit(id)); err != nil {
			return x.rollback(ctx, fmt.Errorf("systemctl stop %s: %w", driveUnit(id), err))
		}
	}
	for _, ls := range stops {
		if ls.SourceKind == SourcePersist && ls.Active && ls.R1 != nil {
			if diff := persistDiffers(ls.Source, *ls.R1); len(diff) > 0 {
				return x.rollback(ctx, fail(ErrPersistMismatch, "", fmt.Errorf("library %d: the .persist written at the stop disagrees with READ ELEMENT STATUS (%s); a host moved cartridges: kept the .persist placements", ls.ID, strings.Join(diff, "; "))))
			}
		}
	}

	// 5. Merge.
	files := map[string][]byte{}
	for _, ls := range stops {
		next, ok := findLib(c.next, ls.ID)
		if !ok {
			continue // a library deleted: its units stay down
		}
		cur, _ := findLib(c.cur, ls.ID)
		merged, err := mergeLibrary(cur, next, ls.Source, req.Dispositions, forced[ls.ID], bayIn(homeOf(next, c.nextHome)))
		if err != nil {
			return x.rollback(ctx, fail(ErrPlacement, "", err))
		}
		base, err := c.nextFiles.next(contentsName(ls.ID))
		if err != nil {
			return x.rollback(ctx, err)
		}
		data, err := renderContents(base.data, merged)
		if err != nil {
			return x.rollback(ctx, fail(ErrPlacement, "", fmt.Errorf("library %d: %w", ls.ID, err)))
		}
		files[contentsName(ls.ID)] = data
		x.it.Exec.Merged[ls.ID] = merged
	}
	if err := x.step("merged", nil); err != nil {
		return x.rollback(ctx, err)
	}

	// 6. Re-hash and write.
	changes, err := x.changes(files)
	if err != nil {
		return x.rollback(ctx, err)
	}
	if err := h.writeFiles(ctx, x.it, changes, c.nextFiles); err != nil {
		return x.rollback(ctx, err)
	}

	// 7. Media.
	if x.plan.NewBarcodes > 0 {
		if err := x.media(ctx); err != nil {
			return x.rollback(ctx, err)
		}
	}
	warnings := append([]string{}, c.warnings...)

	// 8. Units.
	if err := x.units(ctx); err != nil {
		return x.rollback(ctx, err)
	}

	// 9. Start, then hold offline.
	offline, err := h.bringUp(ctx, startGroups(x.plan), func(u string) error {
		return x.step("start "+u, func() { x.it.Exec.Started = append(x.it.Exec.Started, u) })
	})
	if err != nil {
		// Left offline: the rollback stops them through guardedStop, and
		// puts back online any it does not reach.
		x.held = offline
		return x.rollback(ctx, err)
	}
	if err := x.step("started", nil); err != nil {
		return x.rollback(ctx, err)
	}

	// 10. Online and verify.
	h.online(offline)
	if x.it.Exec.BayStopped {
		if _, err := h.cfg.Systemctl(ctx, "start", bayMonitor); err != nil {
			warnings = append(warnings, fmt.Sprintf("systemctl start %s: %v", bayMonitor, err))
		}
	}
	resp := Response{Result: ResultApplied, Units: c.plan.Units(), Notes: x.plan.Notes, Warnings: warnings, PlacementDrift: x.readback(), ForcedUnloads: x.forcedReport()}
	// A forced drive whose cartridge no READ ELEMENT STATUS showed (its
	// library was not running) unloads a cartridge no slot was given.
	for _, f := range x.it.Exec.Forced {
		if f.Unknown {
			resp.Unplaced = append(resp.Unplaced, unknownCartridge(f.Drive))
		}
	}

	// 11. Done.
	if err := h.removeIntent(); err != nil {
		return responseOf(err)
	}
	return resp
}

// step records a step in the intent, after change updates it.
func (x *execution) step(name string, change func()) error {
	if change != nil {
		change()
	}
	x.it.Step = name
	if err := x.h.saveIntent(x.it); err != nil {
		return err
	}
	if x.h.killPoint(name) {
		return errKilled
	}
	return nil
}

// stopper returns guardedStop for mode, moving .persist files into their
// own directory of the apply's rollback directory, and journaling each
// verdict through record.
func (x *execution) stopper(mode StopMode, aside string, record func(*LibStop)) *stopper {
	return x.h.newStopper(x.it, x.rb, mode, aside, record)
}

// newStopper returns guardedStop for mode, journaling the prepared
// libraries before the first stop and each verdict through record. Each
// call gets a directory of its own, created at its first verdict: a
// rollback or recovery in the same stamp as the apply must not move a
// .persist onto one already aside (MoveAcross refuses EEXIST).
func (h *Helper) newStopper(it *Intent, rb string, mode StopMode, aside string, record func(*LibStop)) *stopper {
	st := &stopper{cfg: &h.cfg, configDir: h.cfg.ConfigDir, mode: mode, poll: h.poll}
	save := func(step string) error {
		it.Step = step
		if err := h.saveIntent(it); err != nil {
			return err
		}
		if h.killPoint(step) {
			return errKilled
		}
		return nil
	}
	st.offlining = func(id int) error {
		it.Exec.Offlining = addInt(it.Exec.Offlining, id)
		return save(fmt.Sprintf("%s: offlining %d", mode, id))
	}
	st.prepared = func(libs []*LibStop) error {
		if mode == ModeApply {
			it.Exec.Prepared = libs
		}
		return save(fmt.Sprintf("%s: offline", mode))
	}
	st.journal = func(ls *LibStop) error {
		if st.aside == "" {
			dir, err := uniqueDir(rb, "persist-"+aside+"-"+h.cfg.Now().UTC().Format(rollbackStamp))
			if err != nil {
				return err
			}
			st.aside = dir
		}
		record(ls)
		return save(fmt.Sprintf("%s: library %d stopped", mode, ls.ID))
	}
	return st
}

// uniqueDir creates a new directory under parent named name, or name-2,
// name-3, ... when that exists.
func uniqueDir(parent, name string) (string, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	for i := 1; ; i++ {
		dir := filepath.Join(parent, name)
		if i > 1 {
			dir += "-" + strconv.Itoa(i)
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

// abort ends an apply that stopped nothing: guardedStop put every library
// it took offline back online; the bay monitor is started again.
func (x *execution) abort(ctx context.Context, cause error) Response {
	if errors.Is(cause, errKilled) {
		return killed(cause)
	}
	if x.it.Exec.BayStopped {
		if _, err := x.h.cfg.Systemctl(ctx, "start", bayMonitor); err != nil {
			x.h.cfg.Logf("systemctl start %s: %v", bayMonitor, err)
		}
	}
	if err := x.h.removeIntent(); err != nil {
		return responseOf(errors.Join(cause, err))
	}
	resp := responseOf(cause)
	if code := codeOf(cause); code != ErrInternal {
		resp = Response{Result: ResultRefused, Error: code, Message: cause.Error()}
	}
	return resp
}

// killed is what a test kill point returns: the helper stops as if it had
// died, and leaves everything to recovery.
func killed(err error) Response {
	return Response{Result: ResultError, Error: ErrInternal, Message: err.Error()}
}

// changes are the request's changes with each stopped library's
// library_contents replaced by its merged one, and added for a library
// the request did not write; in the write order of changesOf.
func (x *execution) changes(merged map[string][]byte) ([]change, error) {
	r := *x.req
	r.Files = nil
	seen := map[string]bool{}
	for _, f := range x.req.Files {
		if data, ok := merged[f.Name]; ok {
			f.Content = data
		}
		seen[f.Name] = true
		r.Files = append(r.Files, f)
	}
	for _, name := range sortedNames(merged) {
		if seen[name] {
			continue
		}
		cur, err := x.c.nextFiles.current(name)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(cur.data, merged[name]) {
			continue
		}
		r.Files = append(r.Files, File{Name: name, BaseSHA256: cur.sha, Content: merged[name]})
	}
	return changesOf(&r), nil
}

// media restarts mhvtl-media.service and checks that every new cartridge
// exists (plan 3.3 exec step 7). make_vtl_media exits 1 when mktape fails
// for any library (usr/cmd/make_vtl_media.in:329 at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L329),
// so its exit code is logged and the stat decides.
func (x *execution) media(ctx context.Context) error {
	if _, err := x.h.cfg.Systemctl(ctx, "restart", mediaService); err != nil {
		x.h.cfg.Logf("systemctl restart %s: %v; checking the cartridges", mediaService, err)
	}
	var missing []string
	for _, nb := range newSlotBarcodes(x.c.cur, x.c.next, x.c.nextHome) {
		lib, _ := findLib(x.c.next, nb.lib)
		home := homeOf(lib, x.c.nextHome)
		if media.Exists(home, nb.barcode) {
			x.it.Exec.NewMedia = append(x.it.Exec.NewMedia, filepath.Join(home, nb.barcode))
		} else {
			missing = append(missing, filepath.Join(home, nb.barcode))
		}
	}
	if err := x.step("media", nil); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fail(ErrMedia, mediaService, fmt.Errorf("make_vtl_media did not create %s", strings.Join(missing, ", ")))
	}
	return nil
}

// missingCartridges warns, before anything runs, about each Slot
// cartridge of the new configuration missing from its home that
// mhvtl-media.service would create blank, so data it was expected to hold
// would become an empty tape (mhvtl-control#43). The service runs
// make_vtl_media, which creates every missing Slot barcode of every
// library
// ([usr/cmd/make_vtl_media.in:340-375](https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L340-L375)):
// when the apply restarts it for new barcodes every library is covered,
// those new barcodes excepted; otherwise the libraries that start, whose
// vtllibrary@ wants the service.
func missingCartridges(c *checked) []string {
	ids := startLibs(*c.plan)
	created := map[string]bool{}
	if c.plan.NewBarcodes > 0 {
		ids = nil
		for _, l := range c.next.Libraries {
			ids = append(ids, l.ID)
		}
		sort.Ints(ids)
		for _, nb := range newSlotBarcodes(c.cur, c.next, c.nextHome) {
			created[nb.barcode] = true
		}
	}
	var out []string
	for _, id := range ids {
		lib, ok := findLib(c.next, id)
		if !ok {
			continue
		}
		home := homeOf(lib, c.nextHome)
		for _, sl := range lib.Slots {
			if b := sl.Barcode; b != "" && !created[b] && !media.Exists(home, b) {
				out = append(out, fmt.Sprintf("library %d slot %d: %s is not in %s; mhvtl-media.service creates it blank when the library starts", id, sl.Number, b, home))
			}
		}
	}
	return out
}

// units is plan 3.3 exec step 8: daemon-reload when the generator exists
// (it then derives the instances from device.conf), disable every removed
// instance, and without the generator enable the added ones (AC 11). A
// removed instance is journaled only when it was enabled, so a rollback
// enables just those again.
func (x *execution) units(ctx context.Context) error {
	h := x.h
	gen := h.generator()
	if gen {
		if _, err := h.cfg.Systemctl(ctx, "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	stop, start := set(x.plan.StopOrder()), set(x.plan.StartOrder())
	for _, u := range x.plan.StopOrder() {
		if start[u] {
			continue
		}
		if h.enabled(ctx, u) {
			if err := x.step("disable "+u, func() { x.it.Exec.Disabled = append(x.it.Exec.Disabled, u) }); err != nil {
				return err
			}
		}
		if _, err := h.cfg.Systemctl(ctx, "disable", u); err != nil {
			return fmt.Errorf("systemctl disable %s: %w", u, err)
		}
	}
	if gen {
		return nil
	}
	for _, u := range x.plan.StartOrder() {
		if stop[u] {
			continue
		}
		if err := x.step("enable "+u, func() { x.it.Exec.Enabled = append(x.it.Exec.Enabled, u) }); err != nil {
			return err
		}
		if _, err := h.cfg.Systemctl(ctx, "enable", u); err != nil {
			return fmt.Errorf("systemctl enable %s: %w", u, err)
		}
	}
	return nil
}

// unitGroup is a library's units in start order, its drives then the
// library (lib >= 0), or standalone drives (lib < 0).
type unitGroup struct {
	lib   int
	units []string
}

func startGroups(p UnitPlan) []unitGroup {
	var out []unitGroup
	for _, id := range startLibs(p) {
		g := unitGroup{lib: id}
		for _, d := range p.StartDrives[id] {
			g.units = append(g.units, driveUnit(d))
		}
		g.units = append(g.units, libraryUnit(id))
		out = append(out, g)
	}
	if len(p.StartStandalone) > 0 {
		g := unitGroup{lib: -1}
		for _, d := range p.StartStandalone {
			g.units = append(g.units, driveUnit(d))
		}
		out = append(out, g)
	}
	return out
}

// bringUp starts the groups' units (plan 3.3 exec step 9): record
// journals each before its start; after a library's own start it polls
// "list map" until the library answers and takes it offline, so hosts
// cannot use it while the others come up; then it waits for every unit to
// be active, up to startWait each. It returns the libraries it took
// offline, which the caller puts back online.
//
// Residual, stated: vtllibrary comes up online (usr/cmd/vtllibrary.c:1727
// at 852b78a), discards the messages queued before it (:1757-1761), and
// opens its transport (:1764) before it handles messages, so hosts can
// reach it for about one poll interval before the offline.
func (h *Helper) bringUp(ctx context.Context, groups []unitGroup, record func(string) error) ([]int, error) {
	var all []string
	var libs []int
	for _, g := range groups {
		for _, u := range g.units {
			if err := record(u); err != nil {
				return libs, err
			}
			if _, err := h.cfg.Systemctl(ctx, "start", u); err != nil {
				return libs, fail(ErrStart, u, fmt.Errorf("systemctl start %s: %w", u, err))
			}
			all = append(all, u)
		}
		if g.lib < 0 {
			continue
		}
		if err := h.holdOffline(ctx, g.lib); err != nil {
			return libs, err
		}
		libs = append(libs, g.lib)
	}
	for _, u := range all {
		if err := h.waitActive(ctx, u); err != nil {
			return libs, err
		}
	}
	return libs, nil
}

// holdOffline polls "list map" until library id answers, then takes it
// offline.
func (h *Helper) holdOffline(ctx context.Context, id int) error {
	deadline := time.Now().Add(startWait)
	for {
		_, err := h.cfg.Queue.ListMap(int64(id))
		if err == nil {
			if err := h.cfg.Queue.Offline(int64(id)); err != nil {
				return fail(ErrStart, libraryUnit(id), fmt.Errorf("library %d: offline: %w", id, err))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fail(ErrStart, libraryUnit(id), fmt.Errorf("library %d did not answer within %v: %w", id, startWait, err))
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
}

// waitActive waits up to startWait for unit to be active.
func (h *Helper) waitActive(ctx context.Context, unit string) error {
	deadline := time.Now().Add(startWait)
	for {
		state, err := showProp(ctx, &h.cfg, unit, "ActiveState")
		if err != nil {
			return fail(ErrStart, unit, err)
		}
		switch {
		case state == "active":
			return nil
		case state == "failed" || time.Now().After(deadline):
			return fail(ErrStart, unit, fmt.Errorf("%s is %s", unit, state))
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
}

func (h *Helper) sleep(ctx context.Context) error {
	t := time.NewTimer(h.poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// online puts libraries back online, logging what fails: hosts must get
// them back whatever else went wrong.
func (h *Helper) online(libs []int) {
	for _, id := range libs {
		if err := onlineBarrier(h.cfg.Queue, int64(id)); err != nil {
			h.cfg.Logf("library %d: %v", id, err)
		}
	}
}

// readback is R2 (plan 3.3 exec step 10): READ ELEMENT STATUS of every
// library started, compared with the placement written and the element
// counts of the new configuration. The changer is located again by serial:
// its sg node changes at every restart (Phase 0 amendment 5); the read is
// retried once on the unit attention a restart leaves (amendment 4,
// SCSIAccess.Inventory). Drift is advisory: hosts are online by now.
func (x *execution) readback() []string {
	var drift []string
	for _, id := range startLibs(x.plan) {
		lib, ok := findLib(x.c.next, id)
		if !ok {
			continue
		}
		bay := bayIn(homeOf(lib, x.c.nextHome))
		want, ok := x.it.Exec.Merged[id]
		if !ok {
			want = validate.Baseline(lib, nil, bay)
		}
		hctl, err := x.h.cfg.Devices.Locate(lib.Serial, scsi.PeripheralMediumChanger)
		var inv changer.Inventory
		if err == nil {
			inv, err = x.h.cfg.Devices.Inventory(lib, hctl)
		}
		if err != nil {
			drift = append(drift, fmt.Sprintf("library %d: READ ELEMENT STATUS after the restart: %v", id, err))
			continue
		}
		if n, m := len(inv.Slots), len(slotNumbers(lib)); n != m {
			drift = append(drift, fmt.Sprintf("library %d: %d storage slots, the configuration has %d", id, n, m))
		}
		if n, m := len(inv.MAPs), len(lib.MAPs); n != m {
			drift = append(drift, fmt.Sprintf("library %d: %d MAP slots, the configuration has %d", id, n, m))
		}
		got := placementOf(inv)
		for _, k := range []struct {
			kind      string
			want, got map[int]string
		}{{"slot", want.Slots, got.Slots}, {"MAP slot", want.MAPs, got.MAPs}, {"drive", nil, got.Drives}} {
			nums := map[int]bool{}
			for n := range k.want {
				nums[n] = true
			}
			for n := range k.got {
				nums[n] = true
			}
			for _, n := range sortedInts(nums) {
				w, g := k.want[n], k.got[n]
				if w != g && !(w == "" && k.kind == "MAP slot" && bay(g)) {
					drift = append(drift, fmt.Sprintf("library %d %s %d: written %q, holds %q", id, k.kind, n, w, g))
				}
			}
		}
	}
	return drift
}

// forcedReport lists the forced unloads with the slot each cartridge was
// written to (AC 13).
func (x *execution) forcedReport() []ForcedUnload {
	var out []ForcedUnload
	for _, f := range x.it.Exec.Forced {
		if m, ok := x.it.Exec.Merged[f.Library]; ok {
			f.SourceSlot, f.SourceMAP = 0, 0
			for n, b := range m.Slots {
				if b == f.Barcode {
					f.SourceSlot = n
				}
			}
		}
		out = append(out, f)
	}
	return out
}

// rollback is plan 3.3 exec step 12 (AC 10): guardedStop in rollback mode
// for the libraries the apply started, restore the files, then unwind:
// each library_contents gets the original layout with the journaled
// placements, daemon-reload (always, Phase 0 amendment 6: PartOf would
// otherwise restart stale instances), the units that ran come back held
// offline, then online, and the bay monitor restarts. A rollback that
// fails leaves the intent for recovery.
//
// It runs on a context of its own: the apply's may have been cancelled by
// SIGTERM, which leaves stopSlack before systemd's SIGKILL; the rollback
// then gets sigtermRollback of it.
func (x *execution) rollback(ctx context.Context, cause error) Response {
	if errors.Is(cause, errKilled) {
		return killed(cause)
	}
	h := x.h
	budget := x.plan.Budget()
	if ctx.Err() != nil {
		budget = sigtermRollback
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()

	resp := Response{Result: ResultRolledBack, Error: codeOf(cause), Message: cause.Error(), FailedUnit: unitOf(cause)}
	if resp.FailedUnit != "" && h.cfg.JournalTail != nil {
		if out, err := h.cfg.JournalTail(rctx, resp.FailedUnit); err == nil {
			resp.JournalTail = string(out)
		}
	}
	started := set(x.it.Exec.Started)
	var libs []StopLib
	covered := map[string]bool{}
	for _, id := range startLibs(x.plan) {
		if !started[libraryUnit(id)] {
			continue
		}
		lib, _ := findLib(x.c.next, id)
		libs = append(libs, StopLib{Lib: lib, Persist: persistOn(x.c.nextDC, id), Drives: x.plan.StartDrives[id]})
		covered[libraryUnit(id)] = true
		for _, d := range x.plan.StartDrives[id] {
			covered[driveUnit(d)] = true
		}
	}
	st := x.stopper(ModeRollback, "rollback", func(ls *LibStop) { x.it.Exec.Rollback = append(x.it.Exec.Rollback, ls) })
	again, err := st.guardedStop(rctx, libs)
	for _, u := range x.it.Exec.Started {
		if err == nil && !covered[u] {
			_, err = h.cfg.Systemctl(rctx, "stop", u)
		}
	}
	for _, ls := range again {
		resp.ForcedUnloads = append(resp.ForcedUnloads, ls.ForcedUnloads...)
	}
	if err == nil {
		var conflicts []Conflict
		conflicts, err = h.restore(x.it)
		resp.Conflicts = conflicts
	}
	if err == nil {
		resp.Unplaced, err = h.unwind(rctx, x.it, again)
	}
	if err == nil {
		err = h.removeIntent()
	}
	if err != nil && !errors.Is(err, errKilled) {
		// A held library guardedStop did not reach is still offline.
		reached := map[int]bool{}
		for _, ls := range again {
			reached[ls.ID] = true
		}
		var left []int
		for _, id := range x.held {
			if !reached[id] {
				left = append(left, id)
			}
		}
		h.online(left)
	}
	if err != nil {
		return Response{Result: ResultError, Error: ErrInternal, Conflicts: resp.Conflicts,
			Message: fmt.Sprintf("the apply failed (%v) and so did its rollback (%v); intent.json is kept and the next request recovers it", cause, err)}
	}
	resp.OrphanedMedia = x.it.Exec.NewMedia
	return resp
}

// unwind brings the units back after a rollback or recovery restored the
// files, shared by both (plan 3.3 exec step 12; plan 3.2, "Placement
// source in rollback and recovery"). latest are the verdicts of the
// rollback's or recovery's own guardedStop.
//
// For each library the original configuration has, the placement source
// is the apply's journaled one (the trusted .persist, else R1′) when the
// latest guardedStop did not find it running (not started again, or
// started and never came up); otherwise the latest guardedStop's, with the
// apply's source and the forced unloads after it,
// so a cartridge a disposition shelved or a drive held is not lost. It is
// mapped back to the original layout (mapBack) and written over the
// restored library_contents, unless that file changed after the apply (a
// restore conflict). Then daemon-reload, the enabled state is reverted,
// the units that ran before the apply start held offline, and come back
// online; the bay monitor restarts if the apply stopped it. It returns the
// cartridges no original slot was free for.
func (h *Helper) unwind(ctx context.Context, it *Intent, latest []*LibStop) ([]string, error) {
	ex := it.Exec
	files := newFileSet(h.cfg.ConfigDir)
	orig, err := config.ReadInventory(h.cfg.ConfigDir, reader(files.current))
	if err != nil && !missingDeviceConf(err) {
		return nil, err
	}
	conf, err := mhvtlConfOf(files.current)
	if err != nil {
		return nil, err
	}
	home := h.defaultHome(conf)
	applied, recent := stopsByID(ex.Stops), stopsByID(latest)
	// The cartridges make_vtl_media created for the apply are not placed
	// into the original layout, which did not list them; their media is
	// kept and reported as orphaned.
	newMedia := map[string]bool{}
	for _, p := range ex.NewMedia {
		newMedia[filepath.Base(p)] = true
	}
	intentFiles := map[string]IntentFile{}
	for _, f := range it.Files {
		intentFiles[f.Name] = f
	}
	var unplaced []string
	var groups []unitGroup
	for _, id := range unionIDs(applied, recent) {
		lib, ok := findLib(orig, id)
		if !ok {
			continue // added by the apply: nothing to bring back
		}
		a, l := applied[id], recent[id]
		// The latest guardedStop's source holds only for a library that
		// ran: one started that never came up has the new layout's file
		// as its source.
		src := l
		if a != nil && (l == nil || !l.Active) {
			src = a
		}
		var extra []validate.Placement
		if src == l && a != nil && a.SourceKind != "" {
			extra = append(extra, a.Source)
		}
		fu := newPlacement()
		for _, list := range [][]ForcedUnload{ex.Forced, forcedOf(l)} {
			for _, f := range list {
				if f.Library == id && (f.Unknown || f.Barcode == "") {
					unplaced = append(unplaced, unknownCartridge(f.Drive))
					continue
				}
				if _, taken := fu.MAPs[f.SourceMAP]; f.Library == id && f.SourceMAP > 0 && !taken {
					fu.MAPs[f.SourceMAP] = f.Barcode
					continue
				}
				if f.Library == id {
					// A cartridge without a free source slot gets a
					// negative key, which mapBack has no slot for: it
					// goes to the first empty one.
					if _, taken := fu.Slots[f.SourceSlot]; !taken && f.SourceSlot > 0 {
						fu.Slots[f.SourceSlot] = f.Barcode
					} else {
						fu.Slots[-len(fu.Slots)-1] = f.Barcode
					}
				}
			}
		}
		extra = append(extra, fu)
		// A library whose stop failed before its verdict has no source:
		// its restored file stays as it is.
		if src.SourceKind != "" {
			for i := range extra {
				extra[i] = withoutBarcodes(extra[i], newMedia)
			}
			mapped, un := mapBack(lib, withoutBarcodes(src.Source, newMedia), extra, bayIn(homeOf(lib, home)))
			unplaced = append(unplaced, un...)
			if err := h.writePlacement(id, mapped, intentFiles); err != nil {
				return unplaced, err
			}
		}

		ref := a
		if ref == nil {
			ref = l
		}
		if !ref.Active {
			continue
		}
		g := unitGroup{lib: id}
		for _, u := range ref.Stopped {
			if u != libraryUnit(id) {
				g.units = append(g.units, u)
			}
		}
		g.units = append(g.units, libraryUnit(id))
		groups = append(groups, g)
	}
	if len(ex.StandaloneStopped) > 0 {
		groups = append(groups, unitGroup{lib: -1, units: ex.StandaloneStopped})
	}

	if _, err := h.cfg.Systemctl(ctx, "daemon-reload"); err != nil {
		return unplaced, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	for _, u := range ex.Enabled {
		if _, err := h.cfg.Systemctl(ctx, "disable", u); err != nil {
			return unplaced, fmt.Errorf("systemctl disable %s: %w", u, err)
		}
	}
	for _, u := range ex.Disabled {
		if _, err := h.cfg.Systemctl(ctx, "enable", u); err != nil {
			return unplaced, fmt.Errorf("systemctl enable %s: %w", u, err)
		}
	}
	offline, err := h.bringUp(ctx, groups, func(string) error { return nil })
	h.online(offline)
	if err != nil {
		return unplaced, err
	}
	if ex.BayStopped {
		if _, err := h.cfg.Systemctl(ctx, "start", bayMonitor); err != nil {
			return unplaced, fmt.Errorf("systemctl start %s: %w", bayMonitor, err)
		}
	}
	return unplaced, nil
}

// writePlacement writes p into library id's restored library_contents. A
// file that is not the original the intent recorded changed after the
// apply, and is left alone like a restore conflict.
func (h *Helper) writePlacement(id int, p validate.Placement, intent map[string]IntentFile) error {
	name := contentsName(id)
	cur, err := readTarget(h.cfg.ConfigDir, name)
	if err != nil || !cur.exists {
		return err
	}
	if f, ok := intent[name]; ok && cur.sha != f.OriginalSHA {
		return nil
	}
	data, err := renderContents(cur.data, p)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if bytes.Equal(data, cur.data) {
		return nil
	}
	return conffile.ReplaceIfHash(h.cfg.ConfigDir, name, cur.sha, data)
}

// lifecycle is Lifecycle for runtime recovery: guardedStop in recover mode
// over the libraries the dead apply touched, as the configuration on disk
// now has them, then, after the files are restored, unwind.
type lifecycle struct{ h *Helper }

func (l lifecycle) Stop(ctx context.Context, it *Intent) error {
	h := l.h
	if it.Exec == nil {
		it.Exec = &ExecJournal{}
	}
	files := newFileSet(h.cfg.ConfigDir)
	inv, err := config.ReadInventory(h.cfg.ConfigDir, reader(files.current))
	if err != nil && !missingDeviceConf(err) {
		return err
	}
	dc, err := deviceConfOf(files.current)
	if err != nil {
		return err
	}
	p := it.Exec.Plan
	var libs []StopLib
	ids := append([]int{}, it.Libraries...)
	for _, id := range it.Exec.Offlining {
		ids = addInt(ids, id)
	}
	for _, id := range ids {
		lib, ok := findLib(inv, id)
		if !ok {
			continue
		}
		drives := append(append([]int{}, p.StopDrives[id]...), p.StartDrives[id]...)
		libs = append(libs, StopLib{Lib: lib, Persist: persistOn(dc, id), Drives: drives})
	}
	rb, err := h.rollbackDir(it)
	if err != nil {
		return err
	}
	st := h.newStopper(it, rb, ModeRecover, "recover", func(ls *LibStop) { it.Exec.Recovery = append(it.Exec.Recovery, ls) })
	if _, err := st.guardedStop(ctx, libs); err != nil {
		return err
	}
	for _, d := range append(append([]int{}, p.StopStandalone...), p.StartStandalone...) {
		if _, err := h.cfg.Systemctl(ctx, "stop", driveUnit(d)); err != nil {
			return fmt.Errorf("systemctl stop %s: %w", driveUnit(d), err)
		}
	}
	return nil
}

func (l lifecycle) Start(ctx context.Context, it *Intent) error {
	if it.Exec == nil {
		it.Exec = &ExecJournal{}
	}
	unplaced, err := l.h.unwind(ctx, it, it.Exec.Recovery)
	if len(unplaced) > 0 {
		l.h.cfg.Logf("recovery: no free slot for %s; they stay in their home, out of the library", strings.Join(unplaced, ", "))
	}
	return err
}

// withForced adds the forced drives to the plan: each is stopped with its
// library and started again with the library that has it next (Phase 0
// amendment 2: a forced apply also restarts the drive's vtltape@).
func withForced(p UnitPlan, c *checked, forced map[int][]changer.DriveState) UnitPlan {
	p.StopDrives, p.StartDrives = copyDrives(p.StopDrives), copyDrives(p.StartDrives)
	nextDrives := drivesOf(c.next)
	for id, states := range forced {
		lib, _ := findLib(c.cur, id)
		for _, st := range states {
			for _, d := range lib.Drives {
				if d.Slot != st.Number {
					continue
				}
				p.StopDrives[id] = addInt(p.StopDrives[id], d.ID)
				if nl, ok := nextDrives[d.ID]; ok {
					if _, isLib := findLib(c.next, nl); isLib {
						p.StartDrives[nl] = addInt(p.StartDrives[nl], d.ID)
					} else {
						p.StartStandalone = addInt(p.StartStandalone, d.ID)
					}
				}
			}
		}
	}
	return p
}

func copyDrives(m map[int][]int) map[int][]int {
	out := map[int][]int{}
	for k, v := range m {
		out[k] = append([]int{}, v...)
	}
	return out
}

func addInt(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	s = append(s, v)
	sort.Ints(s)
	return s
}

// affected are the libraries whose units the plan stops or starts.
func affected(p UnitPlan) []int {
	ids := map[int]bool{}
	for _, s := range [][]int{p.Restart, p.Stop, p.Start} {
		for _, id := range s {
			ids[id] = true
		}
	}
	return sortedInts(ids)
}

// startLibs are the libraries the plan starts, restarted or added.
func startLibs(p UnitPlan) []int {
	out := append(append([]int{}, p.Restart...), p.Start...)
	sort.Ints(out)
	return out
}

// unknownCartridge names, in Unplaced, the cartridge of a forced drive
// that no READ ELEMENT STATUS showed.
func unknownCartridge(drive int) string {
	return fmt.Sprintf("the cartridge of drive %d (not shown by READ ELEMENT STATUS)", drive)
}

// withoutBarcodes is p without the cartridges in drop.
func withoutBarcodes(p validate.Placement, drop map[string]bool) validate.Placement {
	out := newPlacement()
	for _, m := range []struct{ from, to map[int]string }{{p.Slots, out.Slots}, {p.MAPs, out.MAPs}, {p.Drives, out.Drives}, {p.Pickers, out.Pickers}} {
		for n, b := range m.from {
			if !drop[b] {
				m.to[n] = b
			}
		}
	}
	return out
}

func anyStopped(stops []*LibStop) bool {
	for _, ls := range stops {
		if len(ls.Stopped) > 0 {
			return true
		}
	}
	return false
}

func stopsByID(stops []*LibStop) map[int]*LibStop {
	m := map[int]*LibStop{}
	for _, ls := range stops {
		m[ls.ID] = ls
	}
	return m
}

func unionIDs(a, b map[int]*LibStop) []int {
	ids := map[int]bool{}
	for id := range a {
		ids[id] = true
	}
	for id := range b {
		ids[id] = true
	}
	return sortedInts(ids)
}

func forcedOf(ls *LibStop) []ForcedUnload {
	if ls == nil {
		return nil
	}
	return ls.ForcedUnloads
}

// persistOn reports whether library id has PERSIST on: any PERSIST line
// whose value begins with yes or true, in any case, as vtllibrary reads it
// (usr/cmd/vtllibrary.c:1335-1339 at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/vtllibrary.c#L1335-L1339).
func persistOn(dc *conffile.DeviceConf, id int) bool {
	st, ok := dc.Stanza("Library", id)
	if !ok {
		return false
	}
	for _, a := range st.Attrs {
		v := strings.ToLower(a.Value)
		if a.Key == "PERSIST" && (strings.HasPrefix(v, "yes") || strings.HasPrefix(v, "true")) {
			return true
		}
	}
	return false
}

// generator reports whether mhVTL's systemd generator is installed.
func (h *Helper) generator() bool {
	for _, p := range h.cfg.Generators {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// enabled reports whether systemctl is-enabled says unit is enabled; it
// exits non-zero otherwise.
func (h *Helper) enabled(ctx context.Context, unit string) bool {
	out, _ := h.cfg.Systemctl(ctx, "is-enabled", unit)
	return strings.TrimSpace(string(out)) == "enabled"
}

// applyLock takes the cooperative apply.lock (plan 3.3 exec step 1): the
// daemon holds it shared around its moves and media writes. Held → busy.
// Without its directory, the daemon's RuntimeDirectory=, no daemon runs to
// hold it, and there is nothing to take.
func (h *Helper) applyLock() (*os.File, error) {
	if _, err := os.Stat(h.cfg.ApplyLockDir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	f, err := openApplyLock(h.cfg.ApplyLockDir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, refuse(ErrBusy, "the daemon is moving cartridges or writing media (%s is held); try again", filepath.Join(h.cfg.ApplyLockDir, applyLockFile))
		}
		return nil, err
	}
	return f, nil
}

func findLib(inv config.Inventory, id int) (config.Library, bool) {
	for _, l := range inv.Libraries {
		if l.ID == id {
			return l, true
		}
	}
	return config.Library{}, false
}

func standaloneDrive(inv config.Inventory, id int) (config.Drive, bool) {
	for _, d := range inv.Standalone {
		if d.ID == id {
			return d, true
		}
	}
	return config.Drive{}, false
}

func homeOf(lib config.Library, def string) string {
	if lib.HomeDir != "" {
		return lib.HomeDir
	}
	return def
}

func set(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
