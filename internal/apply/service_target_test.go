package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mhvtl-control/internal/changer"
)

// library30Units are library 30's units, drive 34 standalone (standalone34).
var library30Units = []string{"vtllibrary@30.service", "vtltape@31.service", "vtltape@32.service", "vtltape@33.service"}

// targetUnits are the units of a target op over the shipped configuration
// with drive 34 standalone, sorted.
var targetUnits = append(append(append([]string{}, library10Units...), library30Units...), "vtltape@34.service")

// target is a service op on mhvtl.target; service echoes units.
func target(op, action string, units []string) Request {
	r := Request{Op: op, Action: action, Target: true, Actor: "admin"}
	if op == OpService {
		r.ExpectUnits = units
	}
	return r
}

// start is a start of library 10 echoing units.
func start(op string, units ...string) Request {
	r := service(op, ActionStart)
	r.ExpectUnits = nil
	if op == OpService {
		r.ExpectUnits = units
	}
	return r
}

// standalone34 makes drive 34 a standalone drive.
func standalone34(e *env) {
	e.replaceFile("device.conf", " Library ID: 30 Slot: 04", " Library ID: 0 Slot: 00")
	e.replaceFile("library_contents.30", "Drive 4:\n", "")
}

// partOf has stopping mhvtl.target stop its support units, as PartOf= does.
func partOf(s *fakeSystem) {
	s.onStop = func(u string) {
		if u == mhvtlTarget {
			for _, su := range append(append([]string{}, targetSupport...), bayMonitor) {
				s.state[su] = "inactive"
			}
		}
	}
}

// supportCalls are the calls naming the target or a support unit.
func supportCalls(s *fakeSystem) []string {
	var out []string
	for _, c := range s.calls {
		for _, u := range append(append([]string{mhvtlTarget}, targetSupport...), bayMonitor) {
			if strings.HasSuffix(c, " "+u) {
				out = append(out, c)
			}
		}
	}
	return out
}

// noModuleCommand fails on a modprobe or rmmod the helper issues itself,
// or a stop of mhvtl-load-modules by name. A start of it by name (a target
// restart) runs only its idempotent modprobe, so it is allowed.
func noModuleCommand(t *testing.T, s *fakeSystem) {
	t.Helper()
	for _, c := range s.calls {
		if strings.Contains(c, "modprobe") || strings.Contains(c, "rmmod") || c == "stop mhvtl-load-modules.service" {
			t.Errorf("module command %q", c)
		}
	}
}

// AC 1: a target restart guarded-stops every library and stops every
// standalone drive, writes each library's placement, and only then stops
// mhvtl.target; it starts the support units that ran, the instances held
// offline, then mhvtl.target.
func TestServiceTargetRestart(t *testing.T) {
	e, s := newExecEnv(t)
	standalone34(e)
	partOf(s)
	s.state[bayMonitor] = "active"
	moved(s)
	pr := e.do(target(OpServicePlan, ActionRestart, nil))
	if pr.Result != ResultValidated || !slices.Equal(pr.Units, sortedCopy(targetUnits)) {
		t.Fatalf("service_plan: %+v", pr)
	}
	resp := e.do(target(OpService, ActionRestart, pr.Units))
	if resp.Result != ResultApplied || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	for _, u := range targetUnits {
		if !s.before("stop "+u, "stop "+mhvtlTarget) || !s.before("stop "+mhvtlTarget, "start "+u) || !s.before("start "+u, "start "+mhvtlTarget) {
			t.Errorf("%s out of order: %v", u, s.calls)
		}
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
	for _, u := range targetSupport {
		if !s.before("stop "+mhvtlTarget, "start "+u) || !s.before("start "+u, "start vtltape@11.service") || s.unitState(u) != "active" {
			t.Errorf("%s: %v", u, s.calls)
		}
	}
	if !s.before("start "+mhvtlTarget, "start "+bayMonitor) || s.unitState(mhvtlTarget) != "active" {
		t.Errorf("target and bay monitor: %v", s.calls)
	}
	if got := string(e.read("library_contents.10")); !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("library_contents.10 lacks the live placement:\n%s", got)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "restart ") || c == "daemon-reload" || strings.HasPrefix(c, "enable ") || strings.HasPrefix(c, "disable ") {
			t.Errorf("service ran %q", c)
		}
	}
	noModuleCommand(t, s)
	if rec := e.lastAudit(); rec.Op != "service:restart" || rec.Result != ResultApplied || len(rec.Files) != 1 {
		t.Errorf("audit %+v", rec)
	}
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	slices.Sort(out)
	return out
}

// AC 2: a target stop leaves every unit down and enabled, the target
// stopped after them; no module command, and the bay monitor is not
// started again.
func TestServiceTargetStop(t *testing.T) {
	e, s := newExecEnv(t)
	standalone34(e)
	partOf(s)
	s.state[bayMonitor] = "active"
	moved(s)
	resp := e.do(target(OpService, ActionStop, targetUnits))
	if resp.Result != ResultApplied || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	for _, u := range targetUnits {
		if !s.before("stop "+u, "stop "+mhvtlTarget) || s.unitState(u) != "inactive" {
			t.Errorf("%s: %s, %v", u, s.unitState(u), s.calls)
		}
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "start ") || strings.HasPrefix(c, "disable ") || strings.HasPrefix(c, "enable ") || c == "daemon-reload" {
			t.Errorf("target stop ran %q", c)
		}
	}
	noModuleCommand(t, s)
	if got := supportCalls(s); !slices.Equal(got, []string{"stop " + mhvtlTarget}) {
		t.Errorf("support calls %v", got)
	}
	if got := string(e.read("library_contents.10")); !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("library_contents.10 lacks the live placement:\n%s", got)
	}
}

// AC 3: a target start starts mhvtl.target and waits for each instance it
// wants; one that fails comes back with its unit and journal.
func TestServiceTargetStart(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail %v", fail), func(t *testing.T) {
			e, s := newExecEnv(t)
			all := append(append([]string{}, library10Units...), "vtllibrary@30.service", "vtltape@31.service", "vtltape@32.service", "vtltape@33.service", "vtltape@34.service")
			for _, u := range all {
				s.state[u] = "inactive"
				s.enabled[u] = true
			}
			s.enabled["vtltape@33.service"] = false // not wanted: not waited for
			s.onStart = func(u string) {
				if u != mhvtlTarget {
					return
				}
				for _, w := range all {
					if s.enabled[w] {
						s.state[w] = "active"
					}
				}
				if fail {
					s.state["vtltape@13.service"] = "failed"
				}
			}
			pr := e.do(target(OpServicePlan, ActionStart, nil))
			if pr.Result != ResultValidated || slices.Contains(pr.Units, "vtltape@33.service") || len(pr.Units) != len(all)-1 {
				t.Fatalf("service_plan: %+v", pr)
			}
			resp := e.do(target(OpService, ActionStart, pr.Units))
			if got := filterPrefix(s.calls, "start ", "stop "); !slices.Equal(got, []string{"start " + mhvtlTarget}) {
				t.Errorf("calls %v", s.calls)
			}
			if !fail {
				if resp.Result != ResultApplied || e.intentExists() {
					t.Fatalf("service: %+v", resp)
				}
				return
			}
			if resp.Result != ResultError || resp.Error != ErrStart || resp.FailedUnit != "vtltape@13.service" || resp.JournalTail != "tail of vtltape@13.service" {
				t.Errorf("service: %+v", resp)
			}
			if rec := e.lastAudit(); rec.Op != "service:start" || rec.Error != ErrStart {
				t.Errorf("audit %+v", rec)
			}
		})
	}
}

// filterPrefix are the calls starting with one of prefixes.
func filterPrefix(calls []string, prefixes ...string) []string {
	var out []string
	for _, c := range calls {
		for _, p := range prefixes {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
			}
		}
	}
	return out
}

// A target start that would start a running library's drives alone is
// refused as a library start is (D6).
func TestServiceTargetStartD6(t *testing.T) {
	e, s := newExecEnv(t)
	s.state["vtltape@12.service"] = "inactive"
	for _, u := range library10Units {
		s.enabled[u] = true
	}
	if pr := e.do(target(OpServicePlan, ActionStart, nil)); pr.Result != ResultRefused || pr.Error != ErrUseRestart || !strings.Contains(pr.Message, "vtltape@12.service") {
		t.Errorf("service_plan: %+v", pr)
	}
}

// addLibraries adds n copies of library 30, with its drives, as libraries
// 40, 50, ...
func addLibraries(e *env, n int) {
	dc := string(e.read("device.conf"))
	i := strings.Index(dc, "Library: 30 ")
	j := strings.LastIndex(dc, "# fifo")
	stanza := dc[i:j]
	add := ""
	for k := 1; k <= n; k++ {
		id := 30 + 10*k
		r := strings.NewReplacer("Library: 30 ", fmt.Sprintf("Library: %d ", id), "Library ID: 30", fmt.Sprintf("Library ID: %d", id), "Drive: 3", fmt.Sprintf("Drive: %d", id/10))
		add += r.Replace(stanza)
		lc := e.read("library_contents.30")
		if err := os.WriteFile(filepath.Join(e.dir, fmt.Sprintf("library_contents.%d", id)), lc, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.dir, "device.conf"), []byte(dc[:j]+add+dc[j:]), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// AC 4 (D5): a target op over the budget is refused, telling the client to
// go library by library; one library's fits.
func TestServiceTargetBudget(t *testing.T) {
	e, _ := newExecEnv(t)
	addLibraries(e, 3)
	e.rebuild()
	for _, action := range []string{ActionRestart, ActionStop} {
		pr := e.do(target(OpServicePlan, action, nil))
		if pr.Result != ResultRefused || pr.Error != ErrBudget || !strings.Contains(pr.Message, action+" the libraries one at a time") {
			t.Errorf("%s: %+v", action, pr)
		}
	}
	if pr := e.do(service(OpServicePlan, ActionRestart)); pr.Result != ResultValidated {
		t.Errorf("library restart: %+v", pr)
	}
}

// AC 5 (D6): a library start is refused while vtllibrary@N runs, drives
// down or not; an inactive library starts with its drives that are down,
// held offline, with no stop and no write.
func TestServiceStart(t *testing.T) {
	e, s := newExecEnv(t)
	if pr := e.do(start(OpServicePlan)); pr.Error != ErrUseRestart || !strings.Contains(pr.Message, "already active") {
		t.Errorf("running library: %+v", pr)
	}
	s.state["vtltape@12.service"] = "failed"
	if pr := e.do(start(OpServicePlan)); pr.Error != ErrUseRestart || !strings.Contains(pr.Message, "vtltape@12.service down") {
		t.Errorf("running library, drive down: %+v", pr)
	}
	for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service"} {
		s.state[u] = "inactive"
	}
	lc := e.read("library_contents.10")
	pr := e.do(start(OpServicePlan))
	want := []string{"vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service"}
	if pr.Result != ResultValidated || !slices.Equal(pr.Units, want) {
		t.Fatalf("service_plan: %+v", pr)
	}
	s.calls = nil
	resp := e.do(start(OpService, want...))
	if resp.Result != ResultApplied || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	if !s.before("start vtltape@12.service", "start vtllibrary@10.service") || !s.before("start vtllibrary@10.service", "offline 10") || !s.before("offline 10", "online 10") {
		t.Errorf("calls %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "stop ") || c == "start vtltape@13.service" || strings.HasPrefix(c, "start mhvtl") {
			t.Errorf("start ran %q", c)
		}
	}
	for _, u := range library10Units {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
	if got := e.read("library_contents.10"); string(got) != string(lc) {
		t.Errorf("library_contents.10 written:\n%s", diffLines(string(lc), string(got)))
	}
	if rec := e.lastAudit(); rec.Op != "service:start" || len(rec.Files) != 0 {
		t.Errorf("audit %+v", rec)
	}
}

// A library start that fails stops again what it started, and leaves the
// drives that ran before it running.
func TestServiceStartFailure(t *testing.T) {
	e, s := newExecEnv(t)
	for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service"} {
		s.state[u] = "inactive"
	}
	s.failStart["vtllibrary@10.service"] = true
	resp := e.do(start(OpService, "vtllibrary@10.service", "vtltape@11.service"))
	if resp.Result != ResultRolledBack || resp.FailedUnit != "vtllibrary@10.service" || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service"} {
		if s.unitState(u) == "active" {
			t.Errorf("%s is active", u)
		}
	}
	for _, u := range []string{"vtltape@12.service", "vtltape@13.service", "vtltape@14.service"} {
		if s.unitState(u) != "active" || s.has("stop "+u) {
			t.Errorf("%s: %s, %v", u, s.unitState(u), s.calls)
		}
	}
}

// A library start killed after any step is recovered with what it started
// stopped again; the message says so once the library had started.
func TestServiceStartRecoverAfterEveryStep(t *testing.T) {
	setup := func(t *testing.T) (*env, *fakeSystem, Request) {
		e, s := newExecEnv(t)
		for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service"} {
			s.state[u] = "inactive"
		}
		return e, s, start(OpService, "vtllibrary@10.service", "vtltape@11.service")
	}
	steps := killPoints(t, setup)
	if len(steps) < 3 {
		t.Fatalf("kill points %v", steps)
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			e, s, req := setup(t)
			e.h.killPoint = func(st string) bool { return st == step }
			if resp := e.do(req); resp.Result != ResultError || !e.intentExists() {
				t.Fatalf("killed: %+v", resp)
			}
			e.rebuild()
			resp := e.do(start(OpServicePlan))
			if resp.Result != ResultRecovered || e.intentExists() {
				t.Fatalf("next request: %+v", resp)
			}
			if said := strings.Contains(resp.Message, "the units the start had started were stopped again"); said != (step == "started") {
				t.Errorf("message %q", resp.Message)
			}
			for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service"} {
				if s.unitState(u) == "active" {
					t.Errorf("%s is active", u)
				}
			}
			if s.unitState("vtltape@12.service") != "active" {
				t.Error("vtltape@12.service stopped")
			}
		})
	}
}

// Item 6: a restart of a library that is not running, with drives that
// are, stays a restart: neither D6 refusal applies, and its drives are
// stopped and started again with the library, which registers them.
func TestServiceRestartInactiveDrivesRun(t *testing.T) {
	e, s := newExecEnv(t)
	s.state["vtllibrary@10.service"] = "failed"
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	for _, d := range []string{"vtltape@11.service", "vtltape@14.service"} {
		if !s.before("stop "+d, "start "+d) || !s.before("start "+d, "start vtllibrary@10.service") {
			t.Errorf("%s not restarted with the library: %v", d, s.calls)
		}
	}
}

// Item 7: a library that is not running loads its .persist; one that puts
// a cartridge on a picker library_contents lists otherwise cannot be
// written, and is refused at the plan, before anything stops.
func TestServicePlanPersistPicker(t *testing.T) {
	for _, action := range []string{ActionRestart, ActionStop} {
		t.Run(action, func(t *testing.T) {
			e, s := newExecEnv(t)
			for _, u := range library10Units {
				s.state[u] = "inactive"
			}
			e.replaceFile("device.conf", " PERSIST: False", " PERSIST: True")
			p := strings.Replace(strings.Replace(string(e.read("library_contents.10")), "Picker 1:\n", "Picker 1: E01003L8\n", 1), "Slot 3: E01003L8\n", "Slot 3: \n", 1)
			os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(p), 0o644)
			pr := e.do(service(OpServicePlan, action))
			if pr.Result != ResultRefused || pr.Error != ErrPlacement || !strings.Contains(pr.Message, "picker 1") {
				t.Errorf("service_plan: %+v", pr)
			}
			if resp := e.do(service(OpService, action)); resp.Error != ErrPlacement || s.has("stop vtllibrary@10.service") {
				t.Errorf("service: %+v, calls %v", resp, s.calls)
			}
		})
	}
}

// Item 7: a running library writes its .persist at the stop, after the
// plan; one with a cartridge on the picker rolls back with placement.
func TestServiceRestartPersistPickerRollback(t *testing.T) {
	e, s := newExecEnv(t)
	lc := string(e.read("library_contents.10"))
	writesPersist(e, s, strings.Replace(strings.Replace(lc, "Picker 1:\n", "Picker 1: E01003L8\n", 1), "Slot 3: E01003L8\n", "Slot 3: \n", 1))
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[2] = changer.SlotState{Number: 3}
	})
	resp := e.do(service(OpService, ActionRestart))
	if resp.Result != ResultRolledBack || resp.Error != ErrPlacement || e.intentExists() {
		t.Fatalf("service: %+v", resp)
	}
	for _, u := range library10Units {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
}

// AC 8: only a target op reaches mhvtl.target and the fixed support units;
// a library op never does, whatever its action.
func TestServiceSupportAllowList(t *testing.T) {
	lib := service(OpService, ActionRestart)
	tgt := target(OpService, ActionRestart, nil)
	for _, u := range append([]string{mhvtlTarget}, targetSupport...) {
		if supportAllowed(&lib, u) == nil {
			t.Errorf("library op allowed %s", u)
		}
		if err := supportAllowed(&tgt, u); err != nil {
			t.Errorf("target op: %v", err)
		}
	}
	for _, u := range []string{bayMonitor, "sshd.service", "vtllibrary@10.service", "mhvtl.target.wants"} {
		if supportAllowed(&tgt, u) == nil {
			t.Errorf("target op allowed %s", u)
		}
	}
	for _, action := range []string{ActionRestart, ActionStop, ActionStart} {
		e, s := newExecEnv(t)
		partOf(s)
		req := service(OpService, action)
		if action == ActionStart {
			s.state["vtllibrary@10.service"] = "inactive"
			req = start(OpService, "vtllibrary@10.service")
		}
		if resp := e.do(req); resp.Result != ResultApplied {
			t.Fatalf("%s: %+v", action, resp)
		}
		if got := supportCalls(s); len(got) > 0 {
			t.Errorf("library %s ran %v", action, got)
		}
	}
}

// A target op killed after any step is recovered: the instances, the
// support units that ran and mhvtl.target run again.
func TestServiceTargetRecoverAfterEveryStep(t *testing.T) {
	for _, action := range []string{ActionRestart, ActionStop} {
		setup := func(t *testing.T) (*env, *fakeSystem, Request) {
			e, s := newExecEnv(t)
			standalone34(e)
			partOf(s)
			return e, s, target(OpService, action, targetUnits)
		}
		steps := killPoints(t, setup)
		if !slices.Contains(steps, "stop "+mhvtlTarget) {
			t.Fatalf("%s: kill points %v", action, steps)
		}
		for _, step := range steps {
			t.Run(action+" "+step, func(t *testing.T) {
				e, s, req := setup(t)
				e.h.killPoint = func(st string) bool { return st == step }
				if resp := e.do(req); resp.Result != ResultError || !e.intentExists() {
					t.Fatalf("killed: %+v", resp)
				}
				e.rebuild()
				if resp := e.do(target(OpServicePlan, action, nil)); resp.Result != ResultRecovered || e.intentExists() {
					t.Fatalf("next request: %+v", resp)
				}
				for _, u := range append(append([]string{mhvtlTarget}, targetSupport...), targetUnits...) {
					if s.unitState(u) != "active" {
						t.Errorf("%s is %s", u, s.unitState(u))
					}
				}
			})
		}
	}
}

// The target is a scope of its own: a request names it or a library,
// never both, and only service ops take it. A target token is no library
// token.
func TestServiceTargetRequests(t *testing.T) {
	for name, body := range map[string]string{
		"both":        `{"v":1,"op":"service_plan","action":"restart","library":10,"target":true}`,
		"apply":       `{"v":1,"op":"apply","target":true}`,
		"units":       `{"v":1,"op":"units","target":true}`,
		"no units":    `{"v":1,"op":"service","action":"stop","target":true}`,
		"bad action":  `{"v":1,"op":"service_plan","action":"reload","target":true}`,
		"target only": `{"v":1,"op":"service_plan","target":true}`,
	} {
		if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, body := range []string{
		`{"v":1,"op":"service_plan","action":"start","target":true}`,
		`{"v":1,"op":"service_plan","action":"start","library":10}`,
	} {
		if _, err := DecodeRequest(strings.NewReader(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
	lib := service(OpServicePlan, ActionStop)
	tgt := target(OpServicePlan, ActionStop, nil)
	if bindingOf(&lib, nil, nil, nil).Scope == bindingOf(&tgt, nil, nil, nil).Scope {
		t.Error("target and library scopes alike")
	}
}

// wantsBay models mhvtl.target's Wants= of the bay monitor: starting the
// target starts it, and it records how many libraries were still offline
// to hosts at that moment.
func wantsBay(s *fakeSystem, offlineAtStart *int) {
	*offlineAtStart = -1
	s.onStart = func(u string) {
		if u == mhvtlTarget {
			*offlineAtStart = len(s.offline) + len(s.pendingOnline)
			s.state[bayMonitor] = "active"
		}
	}
}

// #91 review 1: a target restart starts mhvtl.target, and with it the bay
// monitor, only once every library it held offline is online again.
func TestServiceTargetStartsTargetOnline(t *testing.T) {
	e, s := newExecEnv(t)
	standalone34(e)
	partOf(s)
	s.state[bayMonitor] = "active"
	var offline int
	wantsBay(s, &offline)
	if resp := e.do(target(OpService, ActionRestart, targetUnits)); resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	if offline != 0 {
		t.Errorf("mhvtl.target started with %d libraries offline: %v", offline, s.calls)
	}
	for _, id := range []string{"10", "30"} {
		if !s.before("online "+id, "start "+mhvtlTarget) {
			t.Errorf("library %s online after the target: %v", id, s.calls)
		}
	}
}

// #91 review 2: a standalone drive that was down before a target stop is
// not started by its rollback, nor by the recovery of one killed at any
// step.
func TestServiceTargetStopLeavesDownDrive(t *testing.T) {
	setup := func(t *testing.T) (*env, *fakeSystem, Request) {
		e, s := newExecEnv(t)
		standalone34(e)
		partOf(s)
		s.state["vtltape@34.service"] = "inactive"
		moved(s)
		return e, s, target(OpService, ActionStop, targetUnits)
	}
	e, s, req := setup(t)
	e.h.beforeWrite = func(name string) {
		if name == "library_contents.10" {
			e.replaceFile(name, "Slot 2: E01002L8", "Slot 2: ")
		}
	}
	if resp := e.do(req); resp.Result != ResultRolledBack {
		t.Fatalf("service: %+v", resp)
	}
	if s.unitState("vtltape@34.service") != "inactive" || s.has("start vtltape@34.service") {
		t.Errorf("rollback started vtltape@34: %v", s.calls)
	}
	if s.unitState("vtltape@11.service") != "active" {
		t.Errorf("rollback left vtltape@11 %s", s.unitState("vtltape@11.service"))
	}
	for _, step := range killPoints(t, setup) {
		t.Run(step, func(t *testing.T) {
			e, s, req := setup(t)
			e.h.killPoint = func(st string) bool { return st == step }
			if resp := e.do(req); resp.Result != ResultError {
				t.Fatalf("killed: %+v", resp)
			}
			e.rebuild()
			if resp := e.do(target(OpServicePlan, ActionStop, nil)); resp.Result != ResultRecovered {
				t.Fatalf("next request: %+v", resp)
			}
			if s.unitState("vtltape@34.service") != "inactive" || s.has("start vtltape@34.service") {
				t.Errorf("recovery started vtltape@34: %v", s.calls)
			}
		})
	}
}

// #91 review 3: a target restart brings back exactly the instances that
// ran: a stopped library with its drives, a down drive of a running
// library and a down standalone drive stay down. A stopped library with a
// drive that runs is refused: the drive would come back alone.
func TestServiceTargetRestartOnlyActive(t *testing.T) {
	e, s := newExecEnv(t)
	standalone34(e)
	partOf(s)
	for _, u := range append(append([]string{}, library30Units...), "vtltape@12.service", "vtltape@34.service") {
		s.state[u] = "inactive"
	}
	pr := e.do(target(OpServicePlan, ActionRestart, nil))
	want := append(append([]string{}, library10Units...), "vtltape@34.service")
	if pr.Result != ResultValidated || !slices.Equal(pr.Units, sortedCopy(want)) {
		t.Fatalf("service_plan: %+v", pr)
	}
	if resp := e.do(target(OpService, ActionRestart, pr.Units)); resp.Result != ResultApplied {
		t.Fatalf("service: %+v", resp)
	}
	for _, u := range append(append([]string{}, library30Units...), "vtltape@12.service", "vtltape@34.service") {
		if s.has("start "+u) || s.unitState(u) != "inactive" {
			t.Errorf("%s started: %v", u, s.calls)
		}
	}
	for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service", "vtltape@13.service", "vtltape@14.service"} {
		if !s.before("stop "+u, "start "+u) || s.unitState(u) != "active" {
			t.Errorf("%s not restarted: %v", u, s.calls)
		}
	}

	s.state["vtltape@31.service"] = "active"
	if pr := e.do(target(OpServicePlan, ActionRestart, nil)); pr.Result != ResultRefused || pr.Error != ErrUseRestart || !strings.Contains(pr.Message, "vtltape@31.service running") {
		t.Errorf("stopped library, drive running: %+v", pr)
	}
}

// #91 review 6: the recovery of a target stop that had stopped only a
// standalone drive says the units were started again.
func TestServiceTargetStopRecoverStandaloneNote(t *testing.T) {
	e, s := newExecEnv(t)
	standalone34(e)
	partOf(s)
	for _, u := range append(append([]string{}, library10Units...), library30Units...) {
		s.state[u] = "inactive"
	}
	e.h.killPoint = func(st string) bool { return st == "stop "+mhvtlTarget }
	if resp := e.do(target(OpService, ActionStop, targetUnits)); resp.Result != ResultError {
		t.Fatalf("killed: %+v", resp)
	}
	e.rebuild()
	resp := e.do(target(OpServicePlan, ActionStop, nil))
	if resp.Result != ResultRecovered || !strings.Contains(resp.Message, "the units the stop had stopped were started again") {
		t.Errorf("next request: %+v", resp)
	}
	if s.unitState("vtltape@34.service") != "active" {
		t.Errorf("vtltape@34 is %s", s.unitState("vtltape@34.service"))
	}
}
