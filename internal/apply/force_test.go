package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
)

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

// TestForceToken covers plan 3.4 at the token level: valid once, then
// reused; expired; and bound to the diff and the live placement.
func TestForceToken(t *testing.T) {
	e := newEnv(t)
	b := forceBinding{Diff: "d", Bases: []string{"a", ""}, Units: []string{"vtllibrary@10.service"}, R1: "r"}
	refusedWith := func(name string, err error) {
		t.Helper()
		if r, ok := err.(*refusal); !ok || r.code != ErrForce {
			t.Errorf("%s: %v", name, err)
		}
	}

	tok, exp, err := e.h.mintForce(b)
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(e.clock); d < forceTTL-2*time.Second || d > forceTTL {
		t.Errorf("expires in %v", d)
	}
	if err := e.h.checkForce(tok, b); err != nil {
		t.Fatalf("valid: %v", err)
	}
	refusedWith("reused", e.h.checkForce(tok, b))

	tok, _, _ = e.h.mintForce(b)
	e.advance(forceTTL)
	refusedWith("expired", e.h.checkForce(tok, b))

	tok, _, _ = e.h.mintForce(b)
	diff := b
	diff.Diff = "d2"
	refusedWith("diff changed", e.h.checkForce(tok, diff))
	live := b
	live.R1 = "r2"
	refusedWith("live state changed", e.h.checkForce(tok, live))
	bases := b
	bases.Bases = []string{"a", "x"}
	refusedWith("base changed", e.h.checkForce(tok, bases))
	tok, _, _ = e.h.mintForce(b)
	loaded := b
	loaded.Loaded = []string{"drive 50"}
	refusedWith("loaded drives changed", e.h.checkForce(tok, loaded))
	refusedWith("malformed", e.h.checkForce("x", b))

	// The key is 0600 and kept; expired nonces are pruned.
	fi, err := os.Stat(filepath.Join(e.run, forceKeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 32 {
		t.Errorf("force.key: %v %v", fi, err)
	}
	e.advance(2 * forceTTL)
	tok, _, _ = e.h.mintForce(b)
	if err := e.h.checkForce(tok, b); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.run, nonceDir)); len(ents) != 1 {
		t.Errorf("nonces not pruned: %d left", len(ents))
	}
}

// loadedEnv is library 10 with drive 12 loaded from slot 2, and the
// request that restarts it, built from the live view.
func loadedEnv(t *testing.T) (*env, *fakeSystem, Request) {
	e, s := newExecEnv(t)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
	})
	s.loaded["XYZZY_A2"] = true
	req := compression(e)
	req.Files = append(req.Files, e.edit("library_contents.10", "Slot 2: E01002L8\n", "Slot 2:\n"))
	return e, s, req
}

func (e *env) forceToken(req Request) string {
	e.t.Helper()
	req.Op = OpPlan
	resp := e.do(req)
	if resp.Error != ErrDriveLoaded || resp.ForceToken == "" {
		e.t.Fatalf("plan: %+v", resp)
	}
	return resp.ForceToken
}

// TestForceApply covers plan 3.4 through the helper: a token voids when
// the live placement or the change differs at apply time, and does not
// override a validation error; the valid case is TestExecLoaded.
func TestForceApply(t *testing.T) {
	e, s, req := loadedEnv(t)
	req.ForceToken = e.forceToken(req)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Slots[2] = changer.SlotState{Number: 3}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
		inv.MAPs[0] = changer.SlotState{Number: 1, Full: true, Barcode: "E01003L8"}
	})
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrForce {
		t.Errorf("live state changed: %+v", resp)
	}

	e, _, req = loadedEnv(t)
	tok := e.forceToken(req)
	other := req
	other.Files = append([]File{}, req.Files...)
	other.Files[0] = e.edit("device.conf", " Compression type: lzo", " Compression type: zlib ")
	other.ForceToken = tok
	if resp := e.do(other); resp.Error != ErrForce {
		t.Errorf("diff changed: %+v", resp)
	}

	e, _, req = loadedEnv(t)
	tok = e.forceToken(req)
	bad := req
	bad.Files = append([]File{}, req.Files...)
	bad.Files[0] = e.edit("device.conf", "Unit serial number: XYZZY_A2", "Unit serial number: XYZZY_A1")
	bad.ExpectUnits = []string{"vtllibrary@10.service", "vtltape@12.service"}
	bad.ForceToken = tok
	if resp := e.do(bad); resp.Error != ErrInvalid {
		t.Errorf("validation error: %+v", resp)
	}
}

// TestForceUnitSet: a forced apply stops and starts the vtltape@ of each
// drive it unloads (plan amendment 2), so the plan refused with
// drive_loaded lists those units already, the token binds them, and the
// forced apply's expect_units must name them and its response lists them.
func TestForceUnitSet(t *testing.T) {
	forcedUnits := []string{"vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service"}
	e, s, req := loadedEnv(t)
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Error != ErrDriveLoaded || fmt.Sprint(pr.Units) != fmt.Sprint(forcedUnits) {
		t.Fatalf("plan: units %v, want %v (%+v)", pr.Units, forcedUnits, pr)
	}
	// The units of the change alone no longer do.
	req.ForceToken = pr.ForceToken
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrUnit || !strings.Contains(resp.Message, "vtltape@12.service") {
		t.Fatalf("forced apply with the unforced units: %+v", resp)
	}
	if s.has("stop vtllibrary@10.service") {
		t.Errorf("stopped: %v", s.calls)
	}
	req.ExpectUnits = pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied || fmt.Sprint(resp.Units) != fmt.Sprint(forcedUnits) {
		t.Fatalf("forced apply: %+v", resp)
	}
	if !s.before("stop vtltape@12.service", "start vtltape@12.service") {
		t.Errorf("drive 12 not restarted: %v", s.calls)
	}
}

// TestForceUnitSetContentsOnly is the VM case: an edit of library_contents
// alone restarts only vtllibrary@10, but forced it restarts the loaded
// drive's vtltape@ too, and says so.
func TestForceUnitSetContentsOnly(t *testing.T) {
	e, s := newExecEnv(t)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
	})
	s.loaded["XYZZY_A2"] = true
	req := Request{Op: OpApply, Actor: "admin", Files: []File{e.edit("library_contents.10", "Slot 2: E01002L8\n", "Slot 2:\n")},
		ExpectUnits: []string{"vtllibrary@10.service"}}
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	want := []string{"vtllibrary@10.service", "vtltape@12.service"}
	if pr.Error != ErrDriveLoaded || fmt.Sprint(pr.Units) != fmt.Sprint(want) {
		t.Fatalf("plan: units %v, want %v (%+v)", pr.Units, want, pr)
	}
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied || fmt.Sprint(resp.Units) != fmt.Sprint(want) {
		t.Fatalf("forced apply: %+v", resp)
	}
	if !s.has("start vtltape@12.service") {
		t.Errorf("drive 12 not restarted: %v", s.calls)
	}
}

// TestForceTokenNoLoaded: a force token on an apply that finds no loaded
// drive is not used, and expect_units is checked against the change's own
// units.
func TestForceTokenNoLoaded(t *testing.T) {
	e, s, req := loadedEnv(t)
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	delete(s.loaded, "XYZZY_A2")
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
	})
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrUnit {
		t.Fatalf("forced units with nothing loaded: %+v", resp)
	}
}
