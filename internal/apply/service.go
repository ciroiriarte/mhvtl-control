package apply

import (
	"context"

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

// checkServiceRequest refuses a service op that names no library or an
// action other than restart and stop, or that carries a change set: the
// helper derives everything else from the library. service must echo the
// units service_plan showed.
func checkServiceRequest(req *Request) error {
	if len(req.Files)+len(req.Delete)+len(req.Dispositions) > 0 {
		return refuse(ErrBadRequest, "%s takes no files, deletions or dispositions", req.Op)
	}
	switch req.Action {
	case ActionRestart, ActionStop:
	default:
		return refuse(ErrBadRequest, "unknown action %q", req.Action)
	}
	if req.Library == nil {
		return refuse(ErrBadRequest, "%s needs a library", req.Op)
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

// checkService is check for a service op: the configuration on disk is
// both the current and the next one, the live placement (R1) is read as
// for an apply, and the plan is servicePlan's. What follows, planChecks
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
	lib, ok := findLib(cur, *req.Library)
	if !ok {
		return nil, refuse(ErrBadRequest, "library %d is not in device.conf", *req.Library)
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
	// A restart of a library whose vtllibrary@ is inactive or failed is in
	// effect the library start of plan 4.2, without that row's checks
	// (D6). Phase 3 routes that case through the start action and puts its
	// check here; until then it runs as a restart, whose guardedStop finds
	// nothing running.
	plan := servicePlan(lib)
	c := &checked{plan: &plan, cur: cur, next: cur, curDC: dc, nextDC: dc, curHome: home, nextHome: home,
		nextFiles: files.withChanges(req), live: live, inv: invs, host: host}
	if req.Op == OpService && req.ForceToken == "" {
		if err := checkExpectUnits(req.ExpectUnits, plan); err != nil {
			return c, err
		}
	}
	if err := checkBudget(plan); err != nil {
		return c, err
	}
	// A stop starts nothing, so no cartridge is created blank.
	if req.Action == ActionRestart {
		c.warnings = missingCartridges(c)
	}
	c.warnings = append(c.warnings, h.chWarnings(c)...)
	return c, nil
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
