package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mhvtl-control/internal/conffile"
)

const (
	fabSecret    = "pbs1-secret-0123"
	fabNewSecret = "pbs2-secret-4567"
	fabConf      = "VERSION: 1\n\niSCSI:\n Base name: iqn.2003-01.org.linux-iscsi.vtl1:mhvtl\n\niSCSI target: 10\n Initiator name: iqn.2024-01.com.example:pbs1\n CHAP name: pbs1\n CHAP secret: " + fabSecret + "\n"
)

// fabEnv is a helper env whose systemctl answers show for
// mhvtl-fabrics.service and mhvtl.target with the given states, and fails
// the verbs in fail with err once, those in failAlways every time. Like
// exec.CommandContext, it fails every call once its context has ended.
type fabEnv struct {
	*env
	fab, target string // ActiveState
	load        string // LoadState of mhvtl-fabrics.service
	canReload   string
	fail        map[string]error
	failAlways  map[string]error
	// failedStart is the ActiveState a failed start leaves: failed, as
	// systemd leaves a oneshot whose ExecStart fails.
	failedStart string
	// onRun, when set, runs before a reload, start or stop.
	onRun func(verb string)
	tail  string
}

func newFabEnv(t *testing.T) *fabEnv {
	f := &fabEnv{env: newEnv(t), fab: "active", target: "active", load: "loaded", canReload: "yes",
		fail: map[string]error{}, failAlways: map[string]error{}, failedStart: "failed"}
	f.writeConf("mhvtl.conf", string(f.read("mhvtl.conf"))+"MHVTL_BACKEND=tcmu\n", 0o644)
	f.cfg.Systemctl = func(ctx context.Context, args ...string) ([]byte, error) {
		f.mu.Lock()
		f.systemctl = append(f.systemctl, args)
		f.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch args[0] {
		case "show":
			if strings.Contains(args[1], "CanReload") {
				return []byte("LoadState=" + f.load + "\nActiveState=" + f.fab + "\nCanReload=" + f.canReload + "\n\nLoadState=loaded\nActiveState=" + f.target + "\nCanReload=no\n"), nil
			}
			return []byte(f.fab + "\n"), nil
		case FabricsReload, FabricsStart, fabricsStop:
			if f.onRun != nil {
				f.onRun(args[0])
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			err := f.fail[args[0]]
			delete(f.fail, args[0])
			if err == nil {
				err = f.failAlways[args[0]]
			}
			switch {
			case err != nil && args[0] == FabricsStart:
				f.fab = f.failedStart
			case err != nil:
			case args[0] == FabricsStart:
				f.fab = "active"
			case args[0] == fabricsStop:
				f.fab = "inactive"
			}
			return nil, err
		}
		return nil, nil
	}
	f.cfg.JournalTail = func(context.Context, string) ([]byte, error) { return []byte(f.tail), nil }
	f.cfg.FabricsState = filepath.Join(t.TempDir(), "fabrics.state")
	f.rebuild()
	return f
}

func (f *fabEnv) writeConf(name, data string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fabEnv) fabricsReq(op, content string) Request {
	base := ""
	if b, err := os.ReadFile(filepath.Join(f.dir, fabricsConf)); err == nil {
		base = conffile.Hash(b)
	}
	return Request{Op: op, Actor: "test", Files: []File{{Name: fabricsConf, BaseSHA256: base, Content: []byte(content)}}}
}

// verbs are the systemctl calls other than show.
func (f *fabEnv) verbs() []string {
	var out []string
	for _, c := range f.systemctl {
		if c[0] != "show" {
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

func (f *fabEnv) mode(name string) os.FileMode {
	f.t.Helper()
	st, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Mode().Perm()
}

// noSecret fails when the audit file or the journal holds a secret.
func (f *fabEnv) noSecret(secrets ...string) {
	f.t.Helper()
	audit := strings.Join(f.auditLines(), "\n")
	for _, s := range secrets {
		if strings.Contains(audit, s) {
			f.t.Errorf("audit.jsonl holds the secret %q:\n%s", s, audit)
		}
		for _, j := range f.journal {
			for k, v := range j {
				if strings.Contains(v, s) {
					f.t.Errorf("journal field %s holds the secret %q", k, s)
				}
			}
		}
	}
}

func TestFabricsReload(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o644)
	next := strings.Replace(fabConf, fabSecret, fabNewSecret, 1)

	plan := f.do(f.fabricsReq(OpFabricsPlan, next))
	if plan.Result != ResultValidated || plan.FabricsAction != FabricsReload {
		t.Fatalf("plan = %+v, want validated reload", plan)
	}
	if string(f.read(fabricsConf)) != fabConf || len(f.verbs()) != 0 || len(f.auditLines()) != 0 {
		t.Fatalf("plan wrote, ran %v or audited %v", f.verbs(), f.auditLines())
	}

	resp := f.do(f.fabricsReq(OpFabrics, next))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsReload {
		t.Fatalf("apply = %+v, want applied reload", resp)
	}
	if got := string(f.read(fabricsConf)); got != next {
		t.Errorf("fabrics.conf = %q", got)
	}
	if m := f.mode(fabricsConf); m != 0o600 {
		t.Errorf("fabrics.conf mode %o, want 0600 (it was 0644)", m)
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want reload alone", v)
	}
	if f.intentExists() {
		t.Error("intent left behind")
	}
	audit := f.auditLines()
	if len(audit) != 1 || !strings.Contains(audit[0], `"op":"fabrics"`) || !strings.Contains(audit[0], conffile.SecretMask) {
		t.Errorf("audit %v, want one fabrics record with a masked diff", audit)
	}
	f.noSecret(fabSecret, fabNewSecret)
}

func TestFabricsFirstStart(t *testing.T) {
	f := newFabEnv(t)
	f.fab = "inactive"
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsStart {
		t.Fatalf("apply = %+v, want applied start", resp)
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "start "+FabricsUnit {
		t.Errorf("systemctl %v, want start alone", v)
	}
	if m := f.mode(fabricsConf); m != 0o600 {
		t.Errorf("new fabrics.conf mode %o, want 0600", m)
	}
}

func TestFabricsDeferred(t *testing.T) {
	f := newFabEnv(t)
	f.fab, f.target = "inactive", "inactive"
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsDeferred || !strings.Contains(resp.Message, "when it starts") {
		t.Fatalf("apply = %+v, want applied none", resp)
	}
	if len(f.verbs()) != 0 {
		t.Errorf("systemctl %v, want none", f.verbs())
	}
	if string(f.read(fabricsConf)) != fabConf {
		t.Error("file not written")
	}
}

func TestFabricsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*fabEnv)
		content   string
		result    string
		code, msg string
	}{
		{"failed unit", func(f *fabEnv) { f.writeConf(fabricsConf, fabConf, 0o600); f.fab = "failed" }, "", ResultRefused, ErrFabricsState, "then systemctl restart " + FabricsUnit},
		{"stopped while the target runs", func(f *fabEnv) { f.writeConf(fabricsConf, fabConf, 0o600); f.fab = "inactive" }, "", ResultRefused, ErrFabricsState, "stopped while"},
		{"reloading", func(f *fabEnv) { f.fab = "reloading" }, "", ResultRefused, ErrFabricsState, "reloading"},
		{"not installed", func(f *fabEnv) { f.load = "not-found" }, "", ResultRefused, ErrFabricsUnit, "mhvtl-tcmu"},
		{"no ExecReload", func(f *fabEnv) { f.canReload = "no" }, "", ResultRefused, ErrFabricsUnit, "drop-in"},
		{"backend mhvtl", func(f *fabEnv) {
			f.writeConf("mhvtl.conf", strings.Replace(string(f.read("mhvtl.conf")), "MHVTL_BACKEND=tcmu", "MHVTL_BACKEND=mhvtl", 1), 0o644)
		}, "", ResultRefused, ErrFabricsBackend, "not tcmu"},
		{"backend unset", func(f *fabEnv) {
			f.writeConf("mhvtl.conf", strings.Replace(string(f.read("mhvtl.conf")), "MHVTL_BACKEND=tcmu\n", "", 1), 0o644)
		}, "", ResultRefused, ErrFabricsBackend, `"mhvtl"`},
		{"invalid content", nil, "iSCSI:\n Portal: 1.2.3.4:99999\n", ResultRefused, ErrInvalid, "fabrics.conf"},
		{"no device.conf", func(f *fabEnv) { os.Remove(filepath.Join(f.dir, deviceConf)) }, "", ResultRefused, ErrInvalid, "device.conf does not exist"},
		{"plan fails", nil, strings.Replace(fabConf, "iSCSI target: 10", "iSCSI target: 99", 1), ResultRefused, ErrInvalid, "no Library or Drive 99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFabEnv(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			before, _ := os.ReadFile(filepath.Join(f.dir, fabricsConf))
			content := tc.content
			if content == "" {
				content = strings.Replace(fabConf, fabSecret, fabNewSecret, 1)
			}
			resp := f.do(f.fabricsReq(OpFabrics, content))
			if resp.Result != tc.result || resp.Error != tc.code || !strings.Contains(resp.Message, tc.msg) {
				t.Fatalf("got %s/%s %q, want %s/%s containing %q", resp.Result, resp.Error, resp.Message, tc.result, tc.code, tc.msg)
			}
			after, _ := os.ReadFile(filepath.Join(f.dir, fabricsConf))
			if string(after) != string(before) {
				t.Error("fabrics.conf changed on a refusal")
			}
			if len(f.verbs()) != 0 {
				t.Errorf("systemctl %v on a refusal", f.verbs())
			}
			f.noSecret(fabSecret, fabNewSecret)
		})
	}
}

func TestFabricsConflict(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	req := f.fabricsReq(OpFabrics, strings.Replace(fabConf, fabSecret, fabNewSecret, 1))
	f.writeConf(fabricsConf, fabConf+"# edited by hand\n", 0o600)
	resp := f.do(req)
	if resp.Result != ResultConflict || resp.CurrentSHA256 == nil || *resp.CurrentSHA256 != f.sha(fabricsConf) {
		t.Fatalf("got %+v, want a conflict with the current hash", resp)
	}
	if len(f.verbs()) != 0 {
		t.Errorf("systemctl %v", f.verbs())
	}
}

func TestFabricsRequestShape(t *testing.T) {
	f := newFabEnv(t)
	lib := 10
	for name, req := range map[string]Request{
		"other file":     {Op: OpFabrics, Files: []File{{Name: deviceConf, Content: []byte("x")}}},
		"two files":      {Op: OpFabrics, Files: []File{{Name: fabricsConf}, {Name: fabricsConf}}},
		"with a delete":  {Op: OpFabrics, Files: []File{{Name: fabricsConf}}, Delete: []Delete{{Name: "library_contents.10"}}},
		"with a library": {Op: OpFabricsPlan, Files: []File{{Name: fabricsConf}}, Library: &lib},
		"with units":     {Op: OpFabrics, Files: []File{{Name: fabricsConf}}, ExpectUnits: []string{"vtllibrary@10.service"}},
		"bad base":       {Op: OpFabrics, Files: []File{{Name: fabricsConf, BaseSHA256: "nothex"}}},
		"through apply":  {Op: OpApply, Files: []File{{Name: fabricsConf, Content: []byte(fabConf)}}},
	} {
		t.Run(name, func(t *testing.T) {
			req.V = Version
			b, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRequest(strings.NewReader(string(b))); err == nil && req.Op != OpApply {
				t.Fatalf("DecodeRequest accepted %+v", req)
			}
			if req.Op == OpApply {
				if resp := f.do(req); resp.Result != ResultRefused || resp.Error != ErrName {
					t.Fatalf("apply with fabrics.conf: %+v, want refused name", resp)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(f.dir, fabricsConf)); err == nil {
		t.Error("fabrics.conf written")
	}
}

func TestFabricsRollBack(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	next := strings.Replace(fabConf, fabSecret, fabNewSecret, 1)
	f.fail[FabricsReload] = &exec.ExitError{Stderr: []byte("Job failed; secret " + fabNewSecret + " in output")}
	f.tail = "mhvtl-fabrics: refusing CHAP secret " + fabNewSecret + "\n"

	resp := f.do(f.fabricsReq(OpFabrics, next))
	if resp.Result != ResultRolledBack || resp.Error != ErrFabricsApply || resp.FailedUnit != FabricsUnit {
		t.Fatalf("got %+v, want rolled_back", resp)
	}
	if got := string(f.read(fabricsConf)); got != fabConf {
		t.Errorf("fabrics.conf = %q, want the original back", got)
	}
	if v := f.verbs(); len(v) != 2 || v[0] != "reload "+FabricsUnit || v[1] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want the failed reload, then a reload of the restored file", v)
	}
	if !strings.Contains(resp.Message, "restored") || !strings.Contains(resp.Message, "reloaded") {
		t.Errorf("message %q", resp.Message)
	}
	for _, s := range []string{resp.Message, resp.JournalTail} {
		if strings.Contains(s, fabNewSecret) || !strings.Contains(s, conffile.SecretMask) {
			t.Errorf("%q: secret not masked", s)
		}
	}
	if resp.RollbackDir == "" || f.intentExists() {
		t.Errorf("rollback dir %q, intent left %v", resp.RollbackDir, f.intentExists())
	}
	f.noSecret(fabSecret, fabNewSecret)
}

// A failed first start leaves the unit failed and the new file removed;
// the next export starts it again, which replaces the failed state.
func TestFabricsStartFailureRemovesNewFile(t *testing.T) {
	f := newFabEnv(t)
	f.fab = "inactive"
	f.fail[FabricsStart] = errors.New("exit status 1")
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultRolledBack {
		t.Fatalf("got %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(f.dir, fabricsConf)); !os.IsNotExist(err) {
		t.Errorf("new fabrics.conf not removed: %v", err)
	}
	if f.fab != "failed" || !strings.Contains(resp.Message, "is failed, not reloaded") {
		t.Errorf("unit %s, message %q, want the failed unit left alone", f.fab, resp.Message)
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "start "+FabricsUnit {
		t.Errorf("systemctl %v, want the failed start alone", v)
	}

	f.systemctl = nil
	if plan := f.do(f.fabricsReq(OpFabricsPlan, fabConf)); plan.Result != ResultValidated || plan.FabricsAction != FabricsStart {
		t.Fatalf("plan after the failed start = %+v, want validated start", plan)
	}
	resp = f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsStart || f.fab != "active" {
		t.Fatalf("export after the failed start = %+v (unit %s), want applied start", resp, f.fab)
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "start "+FabricsUnit {
		t.Errorf("systemctl %v, want start alone", v)
	}
}

// A failed unit with no fabrics.conf while mhvtl.target is stopped only
// gets the file: starting mhvtl.target starts the unit.
func TestFabricsFailedDeferred(t *testing.T) {
	f := newFabEnv(t)
	f.fab, f.target = "failed", "inactive"
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsDeferred || len(f.verbs()) != 0 {
		t.Fatalf("got %+v, systemctl %v, want applied none", resp, f.verbs())
	}
}

// A start that fails while the unit ends up active (systemctl cut off, the
// job done) removes the new file, then stops the unit: mhvtl-fabrics apply
// fails without fabrics.conf, and stopping removes the targets.
func TestFabricsStartRollBackStops(t *testing.T) {
	f := newFabEnv(t)
	f.fab = "inactive"
	f.fail[FabricsStart] = errors.New("signal: terminated")
	f.failedStart = "active"
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultRolledBack || !strings.Contains(resp.Message, "stopped "+FabricsUnit) {
		t.Fatalf("got %+v", resp)
	}
	if v := f.verbs(); len(v) != 2 || v[1] != "stop "+FabricsUnit {
		t.Errorf("systemctl %v, want the start, then stop", v)
	}
	if _, err := os.Stat(filepath.Join(f.dir, fabricsConf)); !os.IsNotExist(err) || f.intentExists() {
		t.Errorf("new fabrics.conf left (%v) or intent left %v", err, f.intentExists())
	}
}

// When the reload of the restored file fails too, the answer is
// fabrics_recovery_required, not rolled_back, and the intent stays: the
// next request's recovery reloads again.
func TestFabricsReconcileFailure(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	next := strings.Replace(fabConf, fabSecret, fabNewSecret, 1)
	f.fail[FabricsReload] = &exec.ExitError{Stderr: []byte("Job failed near " + fabNewSecret)}
	f.failAlways[FabricsReload] = &exec.ExitError{Stderr: []byte("Job failed")}
	resp := f.do(f.fabricsReq(OpFabrics, next))
	if resp.Result != ResultError || resp.Error != ErrFabricsState {
		t.Fatalf("got %+v, want error fabrics_recovery_required", resp)
	}
	for _, want := range []string{"configfs may not match", "journalctl -u " + FabricsUnit, "systemctl reload " + FabricsUnit} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message %q lacks %q", resp.Message, want)
		}
	}
	if strings.Contains(resp.Message, fabNewSecret) {
		t.Errorf("message %q holds the secret", resp.Message)
	}
	if string(f.read(fabricsConf)) != fabConf || !f.intentExists() {
		t.Fatalf("want the original back and the intent kept (intent %v)", f.intentExists())
	}

	// Recovery while the reload still fails keeps the intent.
	if resp := f.do(Request{Op: OpRecover, Actor: "test"}); resp.Result != ResultError || resp.Error != ErrFabricsState || !f.intentExists() {
		t.Fatalf("recover with reload failing: %+v, intent %v", resp, f.intentExists())
	}
	delete(f.failAlways, FabricsReload)
	f.systemctl = nil
	resp = f.do(Request{Op: OpRecover, Actor: "test"})
	if resp.Result != ResultRecovered || !strings.Contains(resp.Message, "reloaded "+FabricsUnit) || f.intentExists() {
		t.Fatalf("recover: %+v, intent %v", resp, f.intentExists())
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want one reload", v)
	}
	f.noSecret(fabSecret, fabNewSecret)
}

// SIGTERM during the reload ends the request's context: the rollback and
// its reload of the restored file still run.
func TestFabricsRollBackAfterCancel(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	next := strings.Replace(fabConf, fabSecret, fabNewSecret, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onRun = func(string) { cancel(); f.onRun = nil }
	req := f.fabricsReq(OpFabrics, next)
	req.V = Version
	resp := f.h.Handle(ctx, f.peer, &req)
	if resp.Result != ResultRolledBack || !strings.Contains(resp.Message, "reloaded "+FabricsUnit) {
		t.Fatalf("got %+v, want rolled back and reloaded", resp)
	}
	if string(f.read(fabricsConf)) != fabConf || f.intentExists() {
		t.Errorf("file not restored or intent left %v", f.intentExists())
	}
	if v := f.verbs(); len(v) != 2 || v[1] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want the cut-off reload, then a reload", v)
	}
}

// A first start killed with the helper leaves the unit running the new
// file: recovery removes the file and stops the unit, which removes the
// targets (a reload would fail without the file).
func TestFabricsRecoveryOfFirstStart(t *testing.T) {
	f := newFabEnv(t)
	f.fab = "inactive"
	f.h.killPoint = func(step string) bool { return step == "applying" }
	if resp := f.do(f.fabricsReq(OpFabrics, fabConf)); resp.Result != ResultError {
		t.Fatalf("killed apply: %+v", resp)
	}
	f.fab = "active" // the start job went on
	f.h.killPoint = func(string) bool { return false }
	f.systemctl = nil
	resp := f.do(Request{Op: OpRecover, Actor: "test"})
	if resp.Result != ResultRecovered || !strings.Contains(resp.Message, "stopped "+FabricsUnit) {
		t.Fatalf("recover: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(f.dir, fabricsConf)); !os.IsNotExist(err) || f.intentExists() {
		t.Errorf("new file left (%v) or intent left %v", err, f.intentExists())
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "stop "+FabricsUnit {
		t.Errorf("systemctl %v, want one stop", v)
	}
}

// Recovery while a job of the unit still runs cannot reconcile: it keeps
// the intent for the next request.
func TestFabricsRecoveryUnitBusy(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	f.h.killPoint = func(step string) bool { return step == "applying" }
	f.do(f.fabricsReq(OpFabrics, strings.Replace(fabConf, fabSecret, fabNewSecret, 1)))
	f.h.killPoint = func(string) bool { return false }
	f.fab = "reloading"
	resp := f.do(Request{Op: OpRecover, Actor: "test"})
	if resp.Result != ResultError || resp.Error != ErrFabricsState || !f.intentExists() || !strings.Contains(resp.Message, "reloading") {
		t.Fatalf("recover: %+v, intent %v", resp, f.intentExists())
	}
	if string(f.read(fabricsConf)) != fabConf {
		t.Error("file not restored")
	}
}

func TestFabricsRecovery(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	next := strings.Replace(fabConf, fabSecret, fabNewSecret, 1)
	f.h.killPoint = func(step string) bool { return step == "applying" }
	if resp := f.do(f.fabricsReq(OpFabrics, next)); resp.Result != ResultError {
		t.Fatalf("killed apply: %+v", resp)
	}
	if !f.intentExists() || string(f.read(fabricsConf)) != next {
		t.Fatal("want the intent and the new file left by the dead apply")
	}
	f.h.killPoint = func(string) bool { return false }
	f.systemctl = nil
	resp := f.do(Request{Op: OpRecover, Actor: "test"})
	if resp.Result != ResultRecovered || !strings.Contains(resp.Message, "reloaded "+FabricsUnit) {
		t.Fatalf("recover: %+v", resp)
	}
	if string(f.read(fabricsConf)) != fabConf || f.intentExists() {
		t.Error("file not restored or intent left")
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want one reload", v)
	}
	f.noSecret(fabSecret, fabNewSecret)
}

// Content equal to fabrics.conf is applied like a change: a refusal would
// tell the daemon's client that a secret it sent is the stored one.
func TestFabricsUnchangedContent(t *testing.T) {
	f := newFabEnv(t)
	f.writeConf(fabricsConf, fabConf, 0o600)
	if plan := f.do(f.fabricsReq(OpFabricsPlan, fabConf)); plan.Result != ResultValidated || plan.FabricsAction != FabricsReload {
		t.Fatalf("plan = %+v, want validated reload", plan)
	}
	resp := f.do(f.fabricsReq(OpFabrics, fabConf))
	if resp.Result != ResultApplied || resp.FabricsAction != FabricsReload {
		t.Fatalf("apply = %+v, want applied reload", resp)
	}
	if string(f.read(fabricsConf)) != fabConf || f.intentExists() {
		t.Errorf("file changed or intent left %v", f.intentExists())
	}
	if v := f.verbs(); len(v) != 1 || v[0] != "reload "+FabricsUnit {
		t.Errorf("systemctl %v, want reload alone", v)
	}
	f.noSecret(fabSecret)
}

// A stored secret quoted outside a secret field, in a comment between two
// changed lines, is masked in the audit diff too.
func TestFabricsAuditMasksFileLines(t *testing.T) {
	f := newFabEnv(t)
	cur := strings.Replace(fabConf, " CHAP name: pbs1\n", "# old credential "+fabSecret+"\n CHAP name: pbs1\n", 1)
	f.writeConf(fabricsConf, cur, 0o600)
	next := strings.Replace(strings.Replace(cur, "example:pbs1\n", "example:pbs9\n", 1), "CHAP name: pbs1", "CHAP name: pbs9", 1)
	if resp := f.do(f.fabricsReq(OpFabrics, next)); resp.Result != ResultApplied {
		t.Fatalf("apply = %+v", resp)
	}
	audit := strings.Join(f.auditLines(), "\n")
	if !strings.Contains(audit, "# old credential "+conffile.SecretMask) {
		t.Fatalf("comment not in the audit diff, test does not cover the case: %s", audit)
	}
	f.noSecret(fabSecret)
}

// The backend is MHVTL_BACKEND as systemd's EnvironmentFile= passes it.
func TestFabricsBackendValue(t *testing.T) {
	for _, c := range []struct {
		line, passes string
		ok           bool
	}{
		{"MHVTL_BACKEND=tcmu # c", "tcmu # c", false},
		{"MHVTL_BACKEND=tcmu  ", "tcmu", true},
		{`MHVTL_BACKEND="tcmu"`, "tcmu", true},
		{"MHVTL_BACKEND='tcmu'", "tcmu", true},
		{"MHVTL_BACKEND=tcmu x", "tcmu x", false},
	} {
		t.Run(c.line, func(t *testing.T) {
			f := newFabEnv(t)
			f.writeConf("mhvtl.conf", strings.Replace(string(f.read("mhvtl.conf")), "MHVTL_BACKEND=tcmu\n", c.line+"\n", 1), 0o644)
			resp := f.do(f.fabricsReq(OpFabricsPlan, fabConf))
			switch {
			case c.ok && resp.Result != ResultValidated:
				t.Errorf("got %+v, want validated", resp)
			case !c.ok && (resp.Error != ErrFabricsBackend || !strings.Contains(resp.Message, `MHVTL_BACKEND="`+c.passes+`"`)):
				t.Errorf("got %s %q, want fabrics_backend naming %q", resp.Error, resp.Message, c.passes)
			}
		})
	}
}

// The plan is checked against device.conf before anything is written, for
// a plan, an apply and a deferred apply alike.
func TestFabricsPlanChecked(t *testing.T) {
	bad := strings.Replace(fabConf, "iSCSI target: 10", "iSCSI target: 11", 1) // a drive of library 10
	for _, c := range []struct {
		name string
		op   string
		fab  string
		tgt  string
	}{
		{"plan", OpFabricsPlan, "active", "active"},
		{"apply", OpFabrics, "active", "active"},
		{"deferred", OpFabrics, "inactive", "inactive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFabEnv(t)
			f.fab, f.target = c.fab, c.tgt
			resp := f.do(f.fabricsReq(c.op, bad))
			if resp.Result != ResultRefused || resp.Error != ErrInvalid || !strings.Contains(resp.Message, "drive belongs to library 10") {
				t.Fatalf("got %+v, want refused invalid", resp)
			}
			if _, err := os.Stat(filepath.Join(f.dir, fabricsConf)); !os.IsNotExist(err) || len(f.verbs()) != 0 {
				t.Errorf("wrote fabrics.conf (%v) or ran %v", err, f.verbs())
			}
		})
	}
	t.Run("device.conf unreadable", func(t *testing.T) {
		f := newFabEnv(t)
		p := filepath.Join(f.dir, deviceConf)
		os.Remove(p)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		resp := f.do(f.fabricsReq(OpFabricsPlan, fabConf))
		if resp.Result != ResultRefused || resp.Error != ErrInvalid || !strings.Contains(resp.Message, "device.conf cannot be read") {
			t.Fatalf("got %+v, want refused invalid", resp)
		}
	})
}

// A failed unit whose state file still lists targets is not reported as
// clean, and the intent is not kept: the state file holds what to remove.
func TestFabricsFailedUnitLeftovers(t *testing.T) {
	for _, c := range []struct {
		name  string
		state func(path string) error
		want  string
	}{
		{"owned", func(p string) error {
			return os.WriteFile(p, []byte("iscsi iqn.2003-01.org.linux-iscsi.vtl1:mhvtl.lib10\n"), 0o600)
		}, "(iqn.2003-01.org.linux-iscsi.vtl1:mhvtl.lib10)"},
		{"unreadable", func(p string) error { return os.Mkdir(p, 0o700) }, "cannot be read"},
		{"symlink", func(p string) error { return os.Symlink("/dev/null", p) }, "cannot be read"},
	} {
		t.Run(c.name+" rollback", func(t *testing.T) {
			f := newFabEnv(t)
			f.fab = "inactive"
			f.fail[FabricsStart] = errors.New("exit status 1")
			if err := c.state(f.cfg.FabricsState); err != nil {
				t.Fatal(err)
			}
			resp := f.do(f.fabricsReq(OpFabrics, fabConf))
			checkLeftovers(t, f, resp, c.want)
		})
		t.Run(c.name+" recovery", func(t *testing.T) {
			f := newFabEnv(t)
			f.writeConf(fabricsConf, fabConf, 0o600)
			f.h.killPoint = func(step string) bool { return step == "applying" }
			f.do(f.fabricsReq(OpFabrics, strings.Replace(fabConf, fabSecret, fabNewSecret, 1)))
			f.h.killPoint = func(string) bool { return false }
			f.fab = "failed"
			if err := c.state(f.cfg.FabricsState); err != nil {
				t.Fatal(err)
			}
			resp := f.do(Request{Op: OpRecover, Actor: "test"})
			checkLeftovers(t, f, resp, c.want)
			if string(f.read(fabricsConf)) != fabConf {
				t.Error("file not restored")
			}
		})
	}
	// Without a state file the failed unit has no targets, as before.
	f := newFabEnv(t)
	f.fab = "inactive"
	f.fail[FabricsStart] = errors.New("exit status 1")
	if resp := f.do(f.fabricsReq(OpFabrics, fabConf)); resp.Result != ResultRolledBack || !strings.Contains(resp.Message, "is failed, not reloaded") {
		t.Errorf("no state file: %+v", resp)
	}
}

func checkLeftovers(t *testing.T, f *fabEnv, resp Response, want string) {
	t.Helper()
	if resp.Result != ResultError || resp.Error != ErrFabricsState {
		t.Fatalf("got %+v, want error fabrics_recovery_required", resp)
	}
	for _, w := range []string{want, "systemctl restart " + FabricsUnit, "mhvtl-fabrics remove", "removed its intent"} {
		if !strings.Contains(resp.Message, w) {
			t.Errorf("message %q lacks %q", resp.Message, w)
		}
	}
	if f.intentExists() {
		t.Error("intent kept")
	}
	if v := f.verbs(); len(v) != 0 && v[len(v)-1] != "start "+FabricsUnit {
		t.Errorf("systemctl %v after the failed unit", v)
	}
	f.noSecret(fabSecret, fabNewSecret)
}

// fabricsOwned parses the state file as state_read's fscanf "%31s %223s".
func TestFabricsOwned(t *testing.T) {
	w := "iqn.2003-01.org.linux-iscsi.vtl1:mhvtl.lib10"
	for _, c := range []struct {
		name, in string
		want     []string
	}{
		{"empty", "", nil},
		{"one", "iscsi " + w + "\n", []string{w}},
		{"other fabric and bad name skipped", "loopback " + w + "\niscsi not-a-name\niscsi " + w + "\n", []string{w}},
		{"any white space", "  iscsi\t\t" + w + "\v\f", []string{w}},
		{"odd token at the end", "iscsi " + w + "\niscsi", []string{w}},
		{"long fabric splits", strings.Repeat("x", 31) + "iscsi " + w + "\n", nil},
		{"NUL ends the compare", "iscsi\x00junk " + w + "\n", []string{w}},
		{"capped", strings.Repeat("iscsi "+w+"\n", 70), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fabrics.state")
			if err := os.WriteFile(p, []byte(c.in), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := fabricsOwned(p)
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			if c.name == "capped" {
				for len(want) < fabricsMaxOwned {
					want = append(want, w)
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	if got, err := fabricsOwned(filepath.Join(t.TempDir(), "absent")); got != nil || err != nil {
		t.Errorf("absent: %v %v", got, err)
	}
}

// mhvtl.conf outside the subset all supported systemd versions read alike
// is refused rather than guessed (Codex re-review 2026-11-13): a continued
// comment (v252 keeps MHVTL_BACKEND=mhvtl, v258 reads tcmu), a Unicode
// noncharacter (systemd rejects the file, the unit keeps mhvtl), a
// vertical tab, an oversized line.
func TestFabricsBackendSubset(t *testing.T) {
	for name, text := range map[string]string{
		"continued comment": "MHVTL_BACKEND=mhvtl\n# comment \\\nMHVTL_BACKEND=tcmu\n",
		"noncharacter":      "MHVTL_BACKEND=tcmu\nOTHER=\uffff\n",
		"vertical tab":      "MHVTL_BACKEND=tcmu\v\n",
		"CRLF":              "MHVTL_BACKEND=tcmu\r\n",
		"long line":         "MHVTL_BACKEND=tcmu\nMHVTL_BACKEND=" + strings.Repeat("x", 5000) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFabEnv(t)
			f.writeConf("mhvtl.conf", text, 0o644)
			resp := f.do(f.fabricsReq(OpFabricsPlan, fabConf))
			if resp.Error != ErrFabricsBackend || !strings.Contains(resp.Message, "plain ASCII") {
				t.Errorf("got %s %q, want fabrics_backend", resp.Error, resp.Message)
			}
		})
	}
}
