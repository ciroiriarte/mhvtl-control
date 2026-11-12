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
		{"unchanged", func(f *fabEnv) { f.writeConf(fabricsConf, fabConf, 0o600) }, fabConf, ResultRefused, ErrBadRequest, "unchanged"},
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
