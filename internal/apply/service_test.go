package apply

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/validate"
)

// library10Units are the units a restart or stop of library 10 touches.
var library10Units = []string{"vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service", "vtltape@13.service", "vtltape@14.service"}

// service is a service op on library 10.
func service(op, action string) Request {
	lib := 10
	r := Request{Op: op, Action: action, Library: &lib, Actor: "admin"}
	if op == OpService {
		r.ExpectUnits = library10Units
	}
	return r
}

// moved has a host move E01001L8 from slot 1 to slot 21 of library 10 since
// it started: READ ELEMENT STATUS differs from library_contents.10.
func moved(s *fakeSystem) {
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[0] = changer.SlotState{Number: 1}
		inv.Slots[20] = changer.SlotState{Number: 21, Full: true, Barcode: "E01001L8"}
	})
}

// lastAudit is the last audit record.
func (e *env) lastAudit() AuditRecord {
	e.t.Helper()
	l := e.auditLines()
	if len(l) == 0 {
		e.t.Fatal("no audit record")
	}
	var rec AuditRecord
	if err := json.Unmarshal([]byte(l[len(l)-1]), &rec); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// (a) With PERSIST off, a restart writes the live placement into
// library_contents before the start, so the library comes back with it
// rather than the file's old layout. No systemctl restart, no reload, no
// enabling; the write is audited with its diff.
func TestServiceRestartKeepsPlacement(t *testing.T) {
	e, s := newExecEnv(t)
	moved(s)
	want := placementOf(s.live[10])
	pr := e.do(service(OpServicePlan, ActionRestart))
	if pr.Result != ResultValidated || strings.Join(pr.Units, " ") != strings.Join(library10Units, " ") {
		t.Fatalf("service_plan: %+v", pr)
	}
	if l := e.auditLines(); l != nil {
		t.Errorf("a validated service_plan was audited: %v", l)
	}
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultApplied || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nSlot 1:\n") || !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("library_contents.10 lacks the live placement:\n%s", got)
	}
	if p := placementOf(s.started[10]); !samePlacement(p, want) {
		t.Errorf("library 10 started with %+v, want the live %+v", p, want)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "restart ") || c == "daemon-reload" || strings.HasPrefix(c, "enable ") || strings.HasPrefix(c, "disable ") {
			t.Errorf("service ran %q", c)
		}
	}
	if !s.before("stop vtllibrary@10.service", "stop vtltape@11.service") || !s.before("start vtltape@14.service", "start vtllibrary@10.service") ||
		!s.before("start vtllibrary@10.service", "offline 10") || !s.before("offline 10", "online 10") {
		t.Errorf("calls %v", s.calls)
	}
	if s.has("stop vtllibrary@30.service") {
		t.Error("library 30 restarted")
	}
	for _, u := range library10Units {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
	rec := e.lastAudit()
	if rec.Op != "service:restart" || rec.Result != ResultApplied || len(rec.Files) != 1 || rec.Files[0].Name != "library_contents.10" ||
		!strings.Contains(rec.Diff, "+Slot 21: E01001L8") || strings.Join(rec.Units, " ") != strings.Join(library10Units, " ") {
		t.Errorf("audit %+v", rec)
	}
}

// loadedDrive12 has drive 12 of library 10 hold E01002L8, from slot 2.
func loadedDrive12(s *fakeSystem) {
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
	})
	s.loaded["XYZZY_A2"] = true
}

// (b) A loaded drive refuses the plan with drive_loaded and a token;
// forced, the restart puts the cartridge back in its source slot.
func TestServiceRestartLoaded(t *testing.T) {
	e, s := newExecEnv(t)
	loadedDrive12(s)
	if resp := e.do(service(OpService, ActionRestart)); resp.Result != ResultRefused || resp.Error != ErrDriveLoaded || resp.ForceToken != "" {
		t.Fatalf("unforced service: %+v", resp)
	}
	pr := e.do(service(OpServicePlan, ActionRestart))
	if pr.Error != ErrDriveLoaded || pr.ForceToken == "" || pr.ForceExpires == nil {
		t.Fatalf("service_plan: %+v", pr)
	}
	req := service(OpService, ActionRestart)
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied {
		t.Fatalf("forced service: %+v", resp)
	}
	if len(resp.ForcedUnloads) != 1 || resp.ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 12, Barcode: "E01002L8", SourceSlot: 2}) {
		t.Errorf("forced unloads %+v", resp.ForcedUnloads)
	}
	if got := string(e.read("library_contents.10")); !strings.Contains(got, "\nSlot 2: E01002L8\n") || strings.Contains(got, "Drive 2: E") {
		t.Errorf("library_contents.10:\n%s", got)
	}
	if rec := e.lastAudit(); len(rec.ForcedUnloads) != 1 {
		t.Errorf("audit %+v", rec)
	}
}

// (c) A token binds the action: a stop's is refused on a restart, and the
// reverse; it still works for its own action.
func TestServiceTokenScope(t *testing.T) {
	for _, tc := range []struct{ minted, used string }{{ActionStop, ActionRestart}, {ActionRestart, ActionStop}} {
		e, s := newExecEnv(t)
		loadedDrive12(s)
		pr := e.do(service(OpServicePlan, tc.minted))
		if pr.ForceToken == "" {
			t.Fatalf("%s plan: %+v", tc.minted, pr)
		}
		req := service(OpService, tc.used)
		req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
		if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrForce {
			t.Errorf("%s token on a %s: %+v", tc.minted, tc.used, resp)
		}
		for _, c := range s.calls {
			if strings.HasPrefix(c, "stop ") {
				t.Errorf("%s token on a %s stopped %s", tc.minted, tc.used, c)
			}
		}
	}
	e, s := newExecEnv(t)
	loadedDrive12(s)
	pr := e.do(service(OpServicePlan, ActionStop))
	req := service(OpService, ActionStop)
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	if resp := e.do(req); resp.Result != ResultApplied {
		t.Errorf("stop token on a stop: %+v", resp)
	}
}

// (d) A drive that fails to start rolls the restart back: the library's
// units run again, and the response names the unit with its journal tail.
func TestServiceRestartStartFailure(t *testing.T) {
	e, s := newExecEnv(t)
	s.failStart["vtltape@11.service"] = true
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultRolledBack || resp.Error != ErrStart || resp.FailedUnit != "vtltape@11.service" || resp.JournalTail != "tail of vtltape@11.service" {
		t.Fatalf("service: %+v", resp)
	}
	if e.intentExists() {
		t.Error("intent left")
	}
	for _, u := range library10Units {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s after the rollback", u, s.unitState(u))
		}
	}
	if rec := e.lastAudit(); rec.Op != "service:restart" || rec.Result != ResultRolledBack {
		t.Errorf("audit %+v", rec)
	}
}

// (e) A stop leaves the library's units inactive, not disabled, and writes
// the live placement so the next start (a boot) gets it.
func TestServiceStop(t *testing.T) {
	e, s := newExecEnv(t)
	moved(s)
	resp := e.do(service(OpService, ActionStop))
	if resp.Result != ResultApplied || e.intentExists() || strings.Join(resp.Units, " ") != strings.Join(library10Units, " ") {
		t.Fatalf("service stop: %+v", resp)
	}
	for _, u := range library10Units {
		if s.unitState(u) != "inactive" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "disable ") || strings.HasPrefix(c, "enable ") || strings.HasPrefix(c, "start ") || c == "daemon-reload" {
			t.Errorf("stop ran %q", c)
		}
	}
	if s.unitState("vtllibrary@30.service") != "active" {
		t.Error("library 30 stopped")
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nSlot 1:\n") || !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("library_contents.10 lacks the live placement:\n%s", got)
	}
	if rec := e.lastAudit(); rec.Op != "service:stop" || len(rec.Files) != 1 || rec.Diff == "" {
		t.Errorf("audit %+v", rec)
	}
}

// (f) The forward path runs only approved units: an execution approved
// for fewer units than the plan stops refuses the first one outside them
// and stops nothing of it. (checkExpectUnits keeps a request from getting
// here; allow is what holds when a derivation grows after it.)
func TestServiceAllow(t *testing.T) {
	e, s := newExecEnv(t)
	req := service(OpService, ActionRestart)
	files := newFileSet(e.dir)
	c, err := e.h.checkService(context.Background(), &req, files)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectUnits = library10Units[:4] // not vtltape@14
	rec := AuditRecord{Op: auditOp(&req)}
	resp := e.h.execute(context.Background(), &req, c, nil, &rec)
	if resp.Result == ResultApplied || !strings.Contains(resp.Message, "vtltape@14.service is not among the units") {
		t.Fatalf("execute: %+v", resp)
	}
	for _, c := range s.calls {
		if strings.HasSuffix(c, "vtltape@14.service") {
			t.Errorf("ran %q", c)
		}
	}
	if e.intentExists() {
		t.Error("intent left")
	}
	// Through the request checks, an extra unit is refused before
	// anything runs.
	extra := service(OpService, ActionRestart)
	extra.ExpectUnits = append(append([]string{}, library10Units...), "vtllibrary@30.service")
	s.calls = nil
	if resp := e.do(extra); resp.Result != ResultRefused || resp.Error != ErrUnit {
		t.Errorf("extra unit: %+v", resp)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "stop ") || strings.HasPrefix(c, "offline ") {
			t.Errorf("extra unit: ran %q", c)
		}
	}
}

func serviceEnv(action string) func(t *testing.T) (*env, *fakeSystem, Request) {
	return func(t *testing.T) (*env, *fakeSystem, Request) {
		e, s := newExecEnv(t)
		moved(s)
		return e, s, service(OpService, action)
	}
}

// (g) A service op killed after any step is recovered by the next request:
// the library's units run again, with the live placement, which is also
// what library_contents ends with. Before the stop's verdict the library
// still runs, and the recovery's own guardedStop reads the same R1′; from
// it on, the intent journals that placement as the source. A stop's
// recovery says the units it had stopped were started again only once it
// had stopped them.
func TestServiceRecoverAfterEveryStep(t *testing.T) {
	for _, action := range []string{ActionRestart, ActionStop} {
		steps := killPoints(t, serviceEnv(action))
		if len(steps) < 5 {
			t.Fatalf("%s: kill points %v", action, steps)
		}
		stopped := false
		for _, step := range steps {
			stopped = stopped || step == "apply: library 10 stopped"
			t.Run(action+" "+step, func(t *testing.T) {
				e, s, req := serviceEnv(action)(t)
				want := placementOf(s.live[10])
				e.h.killPoint = func(st string) bool { return st == step }
				if resp := e.do(req); resp.Result != ResultError || !e.intentExists() {
					t.Fatalf("killed: %+v", resp)
				}
				e.rebuild()
				resp := e.do(service(OpServicePlan, action))
				if resp.Result != ResultRecovered || e.intentExists() || !strings.Contains(resp.Message, "service:"+action) {
					t.Fatalf("next request: %+v", resp)
				}
				said := strings.Contains(resp.Message, "the units the stop had stopped were started again")
				if said != (action == ActionStop && stopped) {
					t.Errorf("message %q", resp.Message)
				}
				for _, u := range library10Units {
					if s.unitState(u) != "active" {
						t.Errorf("%s is %s", u, s.unitState(u))
					}
				}
				if p := placementOf(s.started[10]); !samePlacement(p, want) {
					t.Errorf("library 10 started with %+v, want the live %+v", p, want)
				}
				got := string(e.read("library_contents.10"))
				if !strings.Contains(got, "\nSlot 1:\n") || !strings.Contains(got, "\nSlot 21: E01001L8\n") {
					t.Errorf("library_contents.10 lacks the live placement:\n%s", got)
				}
			})
		}
	}
}

// A stop of a library that was not running, killed after it, starts
// nothing at recovery and does not say it did.
func TestServiceRecoverInactiveStop(t *testing.T) {
	e, s := newExecEnv(t)
	for _, u := range library10Units {
		s.state[u] = "inactive"
	}
	e.h.killPoint = func(st string) bool { return st == "merged" }
	if resp := e.do(service(OpService, ActionStop)); resp.Result != ResultError || !e.intentExists() {
		t.Fatalf("killed: %+v", resp)
	}
	e.rebuild()
	resp := e.do(service(OpServicePlan, ActionStop))
	if resp.Result != ResultRecovered || strings.Contains(resp.Message, "started again") {
		t.Fatalf("next request: %+v", resp)
	}
	for _, u := range library10Units {
		if s.unitState(u) != "inactive" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
}

// writesPersist turns PERSIST on, and has vtllibrary@10 write p as its
// .persist when it stops.
func writesPersist(e *env, s *fakeSystem, p string) {
	e.replaceFile("device.conf", " PERSIST: False", " PERSIST: True")
	s.onStop = func(u string) {
		if u == "vtllibrary@10.service" {
			os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(p), 0o644)
		}
	}
}

// livePersist is library_contents.10 as vtllibrary saves it after moved.
func livePersist(lc string) string {
	return strings.Replace(strings.Replace(lc, "Slot 1: E01001L8", "Slot 1: ", 1), "Slot 21: \n", "Slot 21: E01001L8        \n", 1)
}

// restarted checks that a restart of library 10 applied, started it with
// want, wrote want's slots 1 and 21 into library_contents.10 and moved the
// .persist aside.
func restarted(t *testing.T, e *env, s *fakeSystem, want validate.Placement) {
	t.Helper()
	if resp := e.do(service(OpService, ActionRestart)); resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	if p := placementOf(s.started[10]); !samePlacement(p, want) {
		t.Errorf("library 10 started with %+v, want %+v", p, want)
	}
	got := string(e.read("library_contents.10"))
	for _, n := range []int{1, 21} {
		line := fmt.Sprintf("\nSlot %d: %s\n", n, want.Slots[n])
		if want.Slots[n] == "" {
			line = fmt.Sprintf("\nSlot %d: ?\n", n)
		}
		if !regexp.MustCompile(line).MatchString(got) {
			t.Errorf("library_contents.10 lacks %q:\n%s", line, got)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); !os.IsNotExist(err) {
		t.Errorf(".persist left in place: %v", err)
	}
}

// With PERSIST on, a trusted .persist written at the stop is the source.
func TestServiceRestartTrustedPersist(t *testing.T) {
	e, s := newExecEnv(t)
	writesPersist(e, s, livePersist(string(e.read("library_contents.10"))))
	moved(s)
	restarted(t, e, s, placementOf(s.live[10]))
	if rec := e.lastAudit(); !strings.Contains(rec.Diff, "+Slot 21: E01001L8") {
		t.Errorf("audit %+v", rec)
	}
}

// A .persist of a library that stopped with Result other than success is
// not trusted: the live placement is the source, whatever it says.
func TestServiceRestartUntrustedPersist(t *testing.T) {
	e, s := newExecEnv(t)
	writesPersist(e, s, string(e.read("library_contents.10")))
	moved(s)
	inner := e.cfg.Systemctl
	e.cfg.Systemctl = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "show" && args[1] == "--property=Result" && args[3] == "vtllibrary@10.service" {
			return []byte("exit-code\n"), nil
		}
		return inner(ctx, args...)
	}
	e.rebuild()
	restarted(t, e, s, placementOf(s.live[10]))
}

// A trusted .persist that disagrees with READ ELEMENT STATUS rolls the
// restart back with persist_mismatch, keeping its placement.
func TestServiceRestartPersistMismatch(t *testing.T) {
	e, s := newExecEnv(t)
	lc := string(e.read("library_contents.10"))
	writesPersist(e, s, lc)
	moved(s)
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultRolledBack || resp.Error != ErrPersistMismatch {
		t.Fatalf("service: %+v", resp)
	}
	if got := string(e.read("library_contents.10")); got != lc {
		t.Errorf("library_contents.10:\n%s", diffLines(lc, got))
	}
	for _, u := range library10Units {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
	if rec := e.lastAudit(); rec.Op != "service:restart" || rec.Error != ErrPersistMismatch {
		t.Errorf("audit %+v", rec)
	}
}

// A restart of an inactive library takes the file it would load: with
// PERSIST on, its .persist; with it off, library_contents.
func TestServiceRestartInactive(t *testing.T) {
	for _, persist := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist %v", persist), func(t *testing.T) {
			e, s := newExecEnv(t)
			for _, u := range library10Units {
				s.state[u] = "inactive"
			}
			lc := string(e.read("library_contents.10"))
			want := placementOf(s.fromDisk(10))
			if persist {
				e.replaceFile("device.conf", " PERSIST: False", " PERSIST: True")
				os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(livePersist(lc)), 0o644)
				want.Slots[21] = want.Slots[1]
				delete(want.Slots, 1)
			}
			restarted(t, e, s, want)
			if !s.before("start vtllibrary@10.service", "offline 10") {
				t.Errorf("an inactive library was taken offline before its start: %v", s.calls)
			}
		})
	}
}

// Low 4: a .persist lists the pickers, which then come from it. A
// cartridge the file has on the picker, which the library put in a slot
// since, stays in the slot.
func TestServiceRestartPersistPicker(t *testing.T) {
	e, s := newExecEnv(t)
	lc := string(e.read("library_contents.10"))
	e.replaceFile("library_contents.10", "Picker 1:\n", "Picker 1: E01003L8\n")
	e.replaceFile("library_contents.10", "Slot 3: E01003L8\n", "Slot 3: \n")
	writesPersist(e, s, lc)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[2] = changer.SlotState{Number: 3, Full: true, Barcode: "E01003L8"}
	})
	if resp := e.do(service(OpService, ActionRestart)); resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nPicker 1:\n") || !strings.Contains(got, "\nSlot 3: E01003L8\n") {
		t.Errorf("library_contents.10:\n%s", diffLines(lc, got))
	}
}

// (h) The plan warns about the ch leak on the kernel module backend with
// ch loaded, and not on TCMU (no mhvtl.ko).
func TestServiceChWarning(t *testing.T) {
	e, _ := newExecEnv(t)
	if err := os.MkdirAll(filepath.Join(e.cfg.SysRoot, "bus", "scsi", "drivers", "ch"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.KernelRelease = "6.12.111+deb13-amd64"
	e.rebuild()
	chWarned := func() bool {
		resp := e.do(service(OpServicePlan, ActionRestart))
		if resp.Result != ResultValidated {
			t.Fatalf("service_plan: %+v", resp)
		}
		for _, w := range resp.Warnings {
			if strings.HasPrefix(w, "library 10: ") && strings.Contains(w, "ch driver") {
				return true
			}
		}
		return false
	}
	if chWarned() {
		t.Error("warned without mhvtl.ko (TCMU)")
	}
	mod := filepath.Join(e.cfg.SysRoot, "module", "mhvtl")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "refcnt"), []byte("12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !chWarned() {
		t.Error("no warning with mhvtl.ko and ch loaded")
	}
	// A warning, never a refusal: the restart goes ahead with it.
	if resp := e.do(service(OpService, ActionRestart)); resp.Result != ResultApplied || len(resp.Warnings) != 1 {
		t.Errorf("service: %+v", resp)
	}
}

// (i) A placement that matches library_contents writes nothing, and the
// audit lists no file.
func TestServiceUnchanged(t *testing.T) {
	e, s := newExecEnv(t)
	before := e.read("library_contents.10")
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	if !bytes.Equal(e.read("library_contents.10"), before) {
		t.Error("library_contents.10 rewritten")
	}
	if rec := e.lastAudit(); rec.Op != "service:restart" || len(rec.Files) != 0 || rec.Diff != "" {
		t.Errorf("audit %+v", rec)
	}
	if !s.has("start vtllibrary@10.service") {
		t.Errorf("not restarted: %v", s.calls)
	}
}

// (j) A service op carries no change set, names a known action and a
// library, and service echoes the units; other ops take no action.
func TestServiceRequests(t *testing.T) {
	lib := 10
	for name, body := range map[string]string{
		"files":         `{"v":1,"op":"service","action":"restart","library":10,"expect_units":["vtllibrary@10.service"],"files":[{"name":"device.conf","base_sha256":"","content":""}]}`,
		"plan files":    `{"v":1,"op":"service_plan","action":"restart","library":10,"files":[{"name":"device.conf","base_sha256":"","content":""}]}`,
		"delete":        `{"v":1,"op":"service_plan","action":"stop","library":10,"delete":[{"name":"library_contents.10","base_sha256":""}]}`,
		"dispositions":  `{"v":1,"op":"service_plan","action":"stop","library":10,"dispositions":[{"library":10,"barcode":"E01001L8","to":"shelf"}]}`,
		"action":        `{"v":1,"op":"service_plan","action":"reload","library":10}`,
		"no action":     `{"v":1,"op":"service_plan","library":10}`,
		"no library":    `{"v":1,"op":"service_plan","action":"restart"}`,
		"no units":      `{"v":1,"op":"service","action":"restart","library":10}`,
		"apply action":  `{"v":1,"op":"apply","action":"restart"}`,
		"plan library":  `{"v":1,"op":"plan","library":10}`,
		"units library": `{"v":1,"op":"units","library":10}`,
	} {
		if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if code, _, _ := Refused(err); code != ErrBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	req, err := DecodeRequest(strings.NewReader(`{"v":1,"op":"service","action":"stop","library":10,"expect_units":["vtllibrary@10.service"]}`))
	if err != nil || req.Library == nil || *req.Library != lib || req.Action != ActionStop {
		t.Errorf("valid request: %+v, %v", req, err)
	}
	// Handle checks it too, for a caller that did not decode.
	e, s := newExecEnv(t)
	bad := service(OpService, ActionRestart)
	bad.Dispositions = []validate.Disposition{{Library: 10, Barcode: "E01001L8", To: "shelf"}}
	if resp := e.do(bad); resp.Result != ResultRefused || resp.Error != ErrBadRequest || len(s.calls) != 0 {
		t.Errorf("Handle with dispositions: %+v, calls %v", resp, s.calls)
	}
	other := 99
	unknown := service(OpServicePlan, ActionRestart)
	unknown.Library = &other
	if resp := e.do(unknown); resp.Result != ResultRefused || resp.Error != ErrBadRequest {
		t.Errorf("unknown library: %+v", resp)
	}
}

// An apply's force binding, and so its token, is what it was before
// service ops: the MAC below was computed with main at d260843. A service
// op's binding differs by its scope.
func TestServiceBindingKeepsApply(t *testing.T) {
	req := &Request{Op: OpApply, Files: []File{{Name: "device.conf", BaseSHA256: "ab", Content: []byte("x")}}}
	live := &validate.Live{Libraries: map[int]validate.Placement{10: {Slots: map[int]string{1: "E01001L8"}}}}
	loaded := []string{"library 10 drive slot 2 (E01002L8)"}
	b := bindingOf(req, []string{"vtllibrary@10.service"}, live, loaded)
	if got := hex.EncodeToString(b.mac([]byte("k"), []byte("n"), 7)); b.Scope != "" || got != "5cac05cabf39cb2d802b79c27fda70aae3b11ec1bed311dfac5675bfe9844005" {
		t.Errorf("apply binding changed: scope %q, mac %s", b.Scope, got)
	}
	macs := map[string]bool{}
	for _, action := range []string{ActionRestart, ActionStop} {
		r := service(OpService, action)
		sb := bindingOf(&r, library10Units, live, loaded)
		if sb.Scope != "service:"+action+" library 10" {
			t.Errorf("scope %q", sb.Scope)
		}
		macs[hex.EncodeToString(sb.mac([]byte("k"), []byte("n"), 7))] = true
	}
	if len(macs) != 2 {
		t.Error("restart and stop bind alike")
	}
}

// A write that fails is audited with the file it was writing.
func TestServiceWriteFailureAudited(t *testing.T) {
	e, s := newExecEnv(t)
	moved(s)
	e.h.beforeWrite = func(name string) {
		if name == "library_contents.10" {
			e.replaceFile(name, "Slot 2: E01002L8", "Slot 2: ")
		}
	}
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result == ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	if rec := e.lastAudit(); len(rec.Files) != 1 || rec.Files[0].Name != "library_contents.10" || !strings.Contains(rec.Diff, "+Slot 21: E01001L8") {
		t.Errorf("audit %+v", rec)
	}
}
