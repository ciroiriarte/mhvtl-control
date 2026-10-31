package apply

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/validate"
)

// planning reports whether op only checks: plan or service_plan.
func planning(op string) bool { return op == OpPlan || op == OpServicePlan }

// applying reports whether op carries out what it checks: apply or service.
func applying(op string) bool { return op == OpApply || op == OpService }

// auditOp is the op an audit record and an intent name: a service op with
// its action ("service:restart").
func auditOp(req *Request) string {
	if req.Op == OpServicePlan || req.Op == OpService {
		return req.Op + ":" + req.Action
	}
	return req.Op
}

// checkServiceRequest refuses a service op that names neither a library
// nor the target, or both, or an action other than start, stop and
// restart, or that carries a change set: the helper derives everything
// else from the scope. service must echo the units service_plan showed.
func checkServiceRequest(req *Request) error {
	if len(req.Files)+len(req.Delete)+len(req.Dispositions) > 0 {
		return refuse(ErrBadRequest, "%s takes no files, deletions or dispositions", req.Op)
	}
	switch req.Action {
	case ActionRestart, ActionStop, ActionStart:
	default:
		return refuse(ErrBadRequest, "unknown action %q", req.Action)
	}
	if (req.Library == nil) == !req.Target {
		return refuse(ErrBadRequest, "%s needs a library or the target, not both", req.Op)
	}
	if req.Op == OpService && len(req.ExpectUnits) == 0 {
		return refuse(ErrBadRequest, "service needs expect_units, the units service_plan showed")
	}
	return nil
}

// servicePlan is the unit plan of a restart or stop of lib (plan 4.2): the
// library and every drive of it, stopped and started as an apply
// restarting it would. A stop has the same plan, and units, and ends after
// the write (execution.stopOnly); it never uses UnitPlan.Stop, whose
// units the apply disables and whose library the merge skips. checkBudget
// so counts a stop's starts too, which it never runs: conservative.
func servicePlan(lib config.Library) UnitPlan {
	var drives []int
	for _, d := range lib.Drives {
		drives = append(drives, d.ID)
	}
	return UnitPlan{
		Restart:             []int{lib.ID},
		StopDrives:          map[int][]int{lib.ID: drives},
		StartDrives:         map[int][]int{lib.ID: append([]int{}, drives...)},
		GuardDrives:         len(drives),
		RollbackGuardDrives: len(drives),
	}
}

// targetPlan is servicePlan over every library of inv, with every
// standalone drive stopped and started on its own: a target restart or
// stop. Target makes execute cycle mhvtl.target and its support units.
func targetPlan(inv config.Inventory) UnitPlan {
	p := UnitPlan{StopDrives: map[int][]int{}, StartDrives: map[int][]int{}, Target: true}
	for _, lib := range inv.Libraries {
		lp := servicePlan(lib)
		p.Restart = append(p.Restart, lib.ID)
		p.StopDrives[lib.ID], p.StartDrives[lib.ID] = lp.StopDrives[lib.ID], lp.StartDrives[lib.ID]
		p.GuardDrives += lp.GuardDrives
		p.RollbackGuardDrives += lp.RollbackGuardDrives
	}
	for _, d := range inv.Standalone {
		p.StopStandalone = append(p.StopStandalone, d.ID)
	}
	sort.Ints(p.Restart)
	sort.Ints(p.StopStandalone)
	p.StartStandalone = append([]int{}, p.StopStandalone...)
	return p
}

// down reports the ActiveState of unit is inactive or failed: systemctl
// start would start it.
func (h *Helper) down(ctx context.Context, unit string) (bool, string, error) {
	state, err := showProp(ctx, &h.cfg, unit, "ActiveState")
	return state == "inactive" || state == "failed", state, err
}

// downDrives are the drives of lib whose vtltape@ is down, and their IDs.
func (h *Helper) downDrives(ctx context.Context, lib config.Library) ([]int, error) {
	var out []int
	for _, d := range lib.Drives {
		dn, _, err := h.down(ctx, driveUnit(d.ID))
		if err != nil {
			return nil, err
		}
		if dn {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// checkStart is D6 (plan 4.2, library start): a library whose vtllibrary@
// is not inactive or failed is not started. Running, with some drives down,
// it is refused too, since a drive started alone stays out of the library
// until the library's start-time registration (amendment 1): a restart
// brings the drives back with it.
func checkStart(lib config.Library, state string, downDrives []int) error {
	if state == "inactive" || state == "failed" {
		return nil
	}
	if len(downDrives) > 0 {
		var units []string
		for _, d := range downDrives {
			units = append(units, driveUnit(d))
		}
		return refuse(ErrUseRestart, "library %d is %s with %s down; a drive started alone stays out of the library until the library starts, so restart the library", lib.ID, state, strings.Join(units, ", "))
	}
	return refuse(ErrUseRestart, "library %d is already %s; restart it to start it again", lib.ID, state)
}

// startPlan starts lib and those of its drives that are down (downDrives):
// no guardedStop, no write (plan 4.2, library start). A rollback or a
// recovery stops what it started, and nothing else.
func startPlan(lib config.Library, downDrives []int) UnitPlan {
	return UnitPlan{
		Start:               []int{lib.ID},
		StartDrives:         map[int][]int{lib.ID: append([]int{}, downDrives...)},
		RollbackGuardDrives: len(downDrives),
	}
}

// targetStartPlan is what systemctl start mhvtl.target starts and the
// helper then waits for: every instance of device.conf when the generator
// derives them from it, else the enabled ones. A running library with
// drives down is refused as a library start is (D6): the target would
// start those drives alone.
func (h *Helper) targetStartPlan(ctx context.Context, inv config.Inventory) (UnitPlan, error) {
	gen := h.generator()
	wanted := func(u string) bool { return gen || h.enabled(ctx, u) }
	p := UnitPlan{StartDrives: map[int][]int{}}
	for _, lib := range inv.Libraries {
		dn, state, err := h.down(ctx, libraryUnit(lib.ID))
		if err != nil {
			return p, err
		}
		if !dn {
			drives, err := h.downDrives(ctx, lib)
			if err != nil {
				return p, err
			}
			var wantedDown []int
			for _, d := range drives {
				if wanted(driveUnit(d)) {
					wantedDown = append(wantedDown, d)
				}
			}
			if len(wantedDown) > 0 {
				return p, checkStart(lib, state, wantedDown)
			}
		}
		if wanted(libraryUnit(lib.ID)) {
			p.Start = append(p.Start, lib.ID)
		}
		for _, d := range lib.Drives {
			if wanted(driveUnit(d.ID)) {
				p.StartDrives[lib.ID] = append(p.StartDrives[lib.ID], d.ID)
			}
		}
	}
	for _, d := range inv.Standalone {
		if wanted(driveUnit(d.ID)) {
			p.StartStandalone = append(p.StartStandalone, d.ID)
		}
	}
	sort.Ints(p.Start)
	sort.Ints(p.StartStandalone)
	return p, nil
}

// targetSupport are the support units a target restart starts again by
// name before the instances, in the order their After= puts them: the
// TCMU handler must run before a vtltape@ binds on the TCMU backend, and
// vtltape@ pulls in only mhvtl-load-modules (Requires=), not the handler.
// With mhvtl.target itself they are the only units besides the instances a
// service op may stop or start (supportAllowed); the bay monitor has its
// own handling (execute step 3).
var targetSupport = []string{"mhvtl-load-modules.service", "mhvtl-tcmu-handler.service", "mhvtl-media.service"}

// supportAllowed refuses a support unit to every op but a target one, and
// any unit that is not mhvtl.target or one of targetSupport: the allow-list
// of plan 4.2 for the units unitRE does not cover.
func supportAllowed(req *Request, u string) error {
	if req.Target && (u == mhvtlTarget || slices.Contains(targetSupport, u)) {
		return nil
	}
	return fmt.Errorf("%s is not a support unit a %s may stop or start; refused to run it", u, auditOp(req))
}

// serviceBudget is checkBudget for a service op (D5): a target restart or
// stop over the budget is refused, telling the client to go library by
// library. A target start is one systemctl start and the waits for its
// units, which start together; it has no rollback and is not budgeted.
func serviceBudget(req *Request, p UnitPlan) error {
	if req.Target && req.Action == ActionStart {
		return nil
	}
	b := p.Budget()
	if b <= maxBudget {
		return nil
	}
	if req.Target {
		return refuse(ErrBudget, "a %s of mhvtl.target could take %v with its rollback, over the %v the helper's RuntimeMaxSec= leaves; %s the libraries one at a time", req.Action, b, maxBudget, req.Action)
	}
	return refuse(ErrBudget, "a %s of library %d could take %v with its rollback, over the %v the helper's RuntimeMaxSec= leaves", req.Action, *req.Library, b, maxBudget)
}

// budgetOf is the budget check of req's op.
func budgetOf(req *Request, p UnitPlan) error {
	if req.Op == OpServicePlan || req.Op == OpService {
		return serviceBudget(req, p)
	}
	return checkBudget(p)
}

// checkService is check for a service op: the configuration on disk is
// both the current and the next one, the live placement (R1) is read as
// for an apply, and the plan is the action's. What follows, planChecks
// and execute, is the apply's.
func (h *Helper) checkService(ctx context.Context, req *Request, files *fileSet) (*checked, error) {
	if err := checkServiceRequest(req); err != nil {
		return nil, err
	}
	if err := checkUnits(req.ExpectUnits); err != nil {
		return nil, err
	}
	if err := h.checkLegacyEnv(); err != nil {
		return nil, err
	}
	cur, err := config.ReadInventory(h.cfg.ConfigDir, reader(files.current))
	if err != nil && !missingDeviceConf(err) {
		return nil, err
	}
	var lib config.Library
	if !req.Target {
		var ok bool
		if lib, ok = findLib(cur, *req.Library); !ok {
			return nil, refuse(ErrBadRequest, "library %d is not in device.conf", *req.Library)
		}
	}
	conf, err := mhvtlConfOf(files.current)
	if err != nil {
		return nil, err
	}
	dc, err := deviceConfOf(files.current)
	if err != nil {
		return nil, err
	}
	home := h.defaultHome(conf)
	live, invs, err := h.readLive(ctx, cur)
	if err != nil {
		return nil, err
	}
	// The host's devices, for the ch warning; a read error leaves it
	// partial, as it does validation's.
	host, _ := hostids.Read(h.cfg.SysRoot)
	var plan UnitPlan
	switch {
	case req.Target && req.Action == ActionStart:
		if plan, err = h.targetStartPlan(ctx, cur); err != nil {
			return nil, err
		}
	case req.Target:
		plan = targetPlan(cur)
	case req.Action == ActionStart:
		dn, state, err := h.down(ctx, libraryUnit(lib.ID))
		if err != nil {
			return nil, err
		}
		drives, err := h.downDrives(ctx, lib)
		if err != nil {
			return nil, err
		}
		if !dn {
			return nil, checkStart(lib, state, drives)
		}
		plan = startPlan(lib, drives)
	default:
		// A restart of a library whose vtllibrary@ is inactive or failed
		// stays a restart: neither refusal of D6 applies, both being of a
		// running library, and the restart's guardedStop stops the drives
		// that still run, so they start again with the library and are
		// registered by it, and folds the file the library would load
		// (its .persist with PERSIST on) into library_contents.
		plan = servicePlan(lib)
	}
	c := &checked{plan: &plan, cur: cur, next: cur, curDC: dc, nextDC: dc, curHome: home, nextHome: home,
		nextFiles: files.withChanges(req), live: live, inv: invs, host: host}
	if req.Op == OpService && req.ForceToken == "" {
		if err := checkExpectUnits(req.ExpectUnits, plan); err != nil {
			return c, err
		}
	}
	if err := serviceBudget(req, plan); err != nil {
		return c, err
	}
	if err := h.checkPersistPickers(c, invs); err != nil {
		return c, err
	}
	// A stop starts nothing, so no cartridge is created blank.
	if req.Action != ActionStop {
		c.warnings = missingCartridges(c)
	}
	c.warnings = append(c.warnings, h.chWarnings(c)...)
	return c, nil
}

// checkPersistPickers refuses, at the plan, the write a service op could
// not make: a library it stops that is not running loads its .persist
// (FileSource), and when that puts a cartridge on a picker which
// library_contents lists otherwise, renderPickers cannot write it (only an
// emptied picker can be). A running library writes its .persist at the
// stop, after the plan: that case rolls back with placement.
func (h *Helper) checkPersistPickers(c *checked, running map[int]changer.Inventory) error {
	for _, id := range append(append([]int{}, c.plan.Restart...), c.plan.Stop...) {
		if _, ok := running[id]; ok || !persistOn(c.curDC, id) {
			continue
		}
		lib, _ := findLib(c.cur, id)
		src, err := FileSource(h.cfg.ConfigDir, lib, true)
		if err != nil || src.Name != persistName(id) {
			continue // guardedStop reports a bad .persist itself
		}
		listed := map[int]string{}
		for _, p := range lib.PickerMedia {
			listed[p.Number] = p.Barcode
		}
		for _, n := range sortedInts(src.Placement.Pickers) {
			if b := src.Placement.Pickers[n]; b != listed[n] {
				return refuse(ErrPlacement, "library %d: its %s puts %s on picker %d, which library_contents.%d cannot list; move it to a slot first", id, persistName(id), b, n, id)
			}
		}
	}
	return nil
}

// unedited is lib as a client would write it from the placement src with
// nothing edited: each storage slot (the ones the Slot lines skip
// included) and MAP slot holds src's cartridge. Merged with src
// (validate.Merge takes next as an edit of that view), it yields src, so a
// service op writes the placement guardedStop found. kind is src's
// SourceKind: a .persist lists the pickers too, which then come from it,
// so a cartridge the file has on a picker but a host moved to a slot stays
// in the slot. READ ELEMENT STATUS and library_contents give the file's.
func unedited(lib config.Library, src validate.Placement, kind string) config.Library {
	base := validate.Baseline(lib, &src, nil)
	out := lib
	out.Slots, out.MAPs = nil, nil
	for _, n := range sortedInts(slotNumbers(lib)) {
		out.Slots = append(out.Slots, config.Slot{Number: n, Barcode: base.Slots[n]})
	}
	for _, m := range lib.MAPs {
		out.MAPs = append(out.MAPs, config.MAPSlot{Number: m.Number, Barcode: base.MAPs[m.Number]})
	}
	if kind == SourcePersist {
		out.PickerMedia = nil
		for _, n := range sortedInts(src.Pickers) {
			out.PickerMedia = append(out.PickerMedia, config.Slot{Number: n, Barcode: src.Pickers[n]})
		}
	}
	return out
}

// renderPickers sets the Picker lines of data, a library_contents, to p's
// pickers, for a service op whose source is a .persist: renderContents
// keeps them, as an apply's are the client's. Only an emptied picker can
// be written; one p fills that the file has otherwise is refused
// (conffile.SetBarcode).
func renderPickers(data []byte, p validate.Placement) ([]byte, error) {
	c := conffile.ParseLibContents(data)
	for _, e := range c.Entries() {
		if e.Kind == "Picker" && e.Barcode != p.Pickers[e.Number] {
			if err := c.SetBarcode("Picker", e.Number, p.Pickers[e.Number]); err != nil {
				return nil, err
			}
		}
	}
	return c.Render(), nil
}
