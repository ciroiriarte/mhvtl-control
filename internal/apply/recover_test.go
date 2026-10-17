package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
)

// killedWrite leaves e as an apply killed after its device.conf write:
// intent.json in place, the lock free. It returns the original bytes of
// the files the apply touched.
func killedWrite(t *testing.T, e *env, it *Intent) map[string][]byte {
	t.Helper()
	req := changeSet(e)
	orig := map[string][]byte{}
	for _, n := range []string{"device.conf", "library_contents.10", "library_contents.30", "mhvtl.conf"} {
		orig[n] = e.read(n)
	}
	if it.V == 0 {
		it.V = Version
	}
	if it.BootID == "" {
		it.BootID = "00000000-0000-0000-0000-000000000000"
	}
	e.h.killPoint = func(s string) bool { return s == "wrote device.conf" }
	if err := e.h.writeFiles(context.Background(), it, changesOf(req), newFileSet(e.dir)); !errors.Is(err, errKilled) {
		t.Fatalf("write: %v", err)
	}
	e.h.killPoint = func(string) bool { return false }
	return orig
}

func (e *env) assertFiles(what string, want map[string][]byte) {
	e.t.Helper()
	for n, b := range want {
		if !bytes.Equal(e.read(n), b) {
			e.t.Errorf("%s: %s differs", what, n)
		}
	}
}

func (e *env) intentExists() bool {
	_, err := os.Stat(e.h.intentPath())
	return err == nil
}

// TestHelloDuringLiveApply: while an apply runs (it holds exclusive.lock,
// its intent is on disk), hello reports apply_in_progress, and neither
// hello nor plan nor recover touches its files or intent.
func TestHelloDuringLiveApply(t *testing.T) {
	e := newEnv(t)
	req := changeSet(e)
	l, _, err := e.h.tryLock()
	if err != nil {
		t.Fatal(err)
	}
	paused, resume, done := make(chan struct{}), make(chan struct{}), make(chan error)
	e.h.killPoint = func(s string) bool {
		if s == "wrote library_contents.10" {
			close(paused)
			<-resume
		}
		return false
	}
	go func() {
		defer l.release()
		done <- e.h.writeFiles(context.Background(), &Intent{V: Version}, changesOf(req), newFileSet(e.dir))
	}()
	<-paused
	snapshot := func() string {
		var s strings.Builder
		for _, n := range []string{"device.conf", "library_contents.10", "library_contents.30", "mhvtl.conf"} {
			s.WriteString(e.sha(n))
		}
		b, _ := os.ReadFile(e.h.intentPath())
		return s.String() + string(b)
	}
	before := snapshot()
	other := New(*e.cfg) // another instance of the helper
	if resp := other.Handle(context.Background(), e.peer, &Request{V: Version, Op: OpHello}); resp.State != StateInProgress {
		t.Errorf("hello: %+v", resp)
	}
	for _, op := range []string{OpPlan, OpRecover, OpApply} {
		if resp := other.Handle(context.Background(), e.peer, &Request{V: Version, Op: op}); resp.Result != ResultInProgress {
			t.Errorf("%s: %+v", op, resp)
		}
	}
	if snapshot() != before {
		t.Error("the live apply's files or intent changed")
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRecoverBootGuards: --recover-boot does nothing, successfully, unless
// the intent is from another boot, the lock is free and no instance runs.
func TestRecoverBootGuards(t *testing.T) {
	e := newEnv(t)
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || msg != "no interrupted apply" {
		t.Errorf("no intent: %q, %v", msg, err)
	}

	e = newEnv(t)
	cur, _ := e.h.bootID()
	orig := killedWrite(t, e, &Intent{BootID: cur})
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || !strings.Contains(msg, "this boot") || !e.intentExists() {
		t.Errorf("same boot: %q, %v, intent %v", msg, err, e.intentExists())
	}

	e = newEnv(t)
	orig = killedWrite(t, e, &Intent{})
	release := e.holdLock()
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || msg != exclusiveHeld || !e.intentExists() {
		t.Errorf("lock held: %q, %v", msg, err)
	}
	release()

	// The intent replaced, or removed, between the first read and the
	// lock: left alone.
	e.h.beforeBootLock = func() {
		it, err := e.h.loadIntent()
		if err != nil || it == nil {
			t.Fatalf("intent %v, %v", it, err)
		}
		it.Started = it.Started.Add(time.Second)
		if err := e.h.saveIntent(it); err != nil {
			t.Fatal(err)
		}
	}
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || msg != intentChanged || !e.intentExists() {
		t.Errorf("replaced: %q, %v, intent %v", msg, err, e.intentExists())
	}
	saved, err := os.ReadFile(e.h.intentPath())
	if err != nil {
		t.Fatal(err)
	}
	e.h.beforeBootLock = func() { os.Remove(e.h.intentPath()) }
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || msg != "no interrupted apply" {
		t.Errorf("removed: %q, %v", msg, err)
	}
	e.h.beforeBootLock = func() {}
	if err := os.WriteFile(e.h.intentPath(), saved, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, out := range []string{
		"vtllibrary@10.service loaded active running Robot Library Daemon\n",
		"● vtltape@11.service loaded activating start Tape Daemon\n",
		"vtltape@99.service not-found inactive dead vtltape@99.service\nvtltape@12.service loaded deactivating stop-sigterm Tape\n",
	} {
		e.systemctlOut = out
		if msg, err := e.h.RecoverBoot(context.Background()); err != nil || !strings.Contains(msg, "already active") || !e.intentExists() {
			t.Errorf("active instance %q: %q, %v", out, msg, err)
		}
	}
	if len(e.systemctl) == 0 || e.systemctl[0][0] != "list-units" {
		t.Errorf("systemctl calls %v", e.systemctl)
	}

	// Every guard holds: the files are restored and the intent removed.
	e.systemctlOut = "vtllibrary@10.service loaded failed failed Robot Library Daemon\nvtltape@11.service loaded inactive dead Tape\n"
	msg, err := e.h.RecoverBoot(context.Background())
	if err != nil || !strings.HasPrefix(msg, "recovered") || e.intentExists() {
		t.Fatalf("guards pass: %q, %v, intent %v", msg, err, e.intentExists())
	}
	e.assertFiles("boot recovery", orig)
	if l := e.auditLines(); len(l) != 1 || !strings.Contains(l[0], `"op":"recover"`) || !strings.Contains(l[0], `"result":"recovered"`) {
		t.Errorf("audit %v", l)
	}

	// An intent naming libraries is restored at boot without the unit
	// lifecycle: nothing runs yet.
	e = newEnv(t)
	orig = killedWrite(t, e, &Intent{Libraries: []int{10, 30}})
	if msg, err := e.h.RecoverBoot(context.Background()); err != nil || e.intentExists() {
		t.Errorf("libraries at boot: %q, %v", msg, err)
	}
	e.assertFiles("boot recovery with libraries", orig)
}

type fakeLifecycle struct{ calls []string }

func (f *fakeLifecycle) Stop(_ context.Context, it *Intent) error {
	f.calls = append(f.calls, "stop")
	return nil
}

func (f *fakeLifecycle) Start(_ context.Context, it *Intent) error {
	f.calls = append(f.calls, "start")
	return nil
}

// TestRecoverLibraries: runtime recovery of an apply that had stopped
// libraries goes through the unit lifecycle around the file restore; a
// recovery killed after the stop resumes without stopping again.
func TestRecoverLibraries(t *testing.T) {
	e := newEnv(t)
	orig := killedWrite(t, e, &Intent{Libraries: []int{10}})
	lc := &fakeLifecycle{}
	e.cfg.Lifecycle = lc
	e.rebuild()
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultRecovered {
		t.Fatalf("recover: %+v", resp)
	}
	e.assertFiles("recover", orig)
	if strings.Join(lc.calls, ",") != "stop,start" {
		t.Errorf("lifecycle calls %v", lc.calls)
	}

	e = newEnv(t)
	orig = killedWrite(t, e, &Intent{Libraries: []int{10}})
	it, err := e.h.loadIntent()
	if err != nil {
		t.Fatal(err)
	}
	it.Recovery = &Recovery{Step: "stopped"}
	if err := e.h.saveIntent(it); err != nil {
		t.Fatal(err)
	}
	lc = &fakeLifecycle{}
	e.cfg.Lifecycle = lc
	e.rebuild()
	if resp := e.do(Request{Op: OpApply}); resp.Result != ResultRecovered {
		t.Fatalf("resume: %+v", resp)
	}
	e.assertFiles("resume", orig)
	if strings.Join(lc.calls, ",") != "start" {
		t.Errorf("lifecycle calls on resume %v", lc.calls)
	}
}

// TestRecoverConflict: a file changed after the apply died (hash neither
// the original nor the post-write one) is reported and left as it is; the
// others are restored.
func TestRecoverConflict(t *testing.T) {
	e := newEnv(t)
	it := &Intent{}
	orig := killedWrite(t, e, it)
	hand := []byte("# fixed by hand\n")
	if err := os.WriteFile(filepath.Join(e.dir, "device.conf"), hand, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := e.do(Request{Op: OpRecover})
	if resp.Result != ResultRecovered || len(resp.Conflicts) != 1 || resp.Conflicts[0].File != "device.conf" {
		t.Fatalf("recover: %+v", resp)
	}
	if !bytes.Equal(e.read("device.conf"), hand) {
		t.Error("the hand fix was overwritten")
	}
	delete(orig, "device.conf")
	e.assertFiles("recover with a conflict", orig)
	rb := filepath.Join(e.state, rollbackDir, it.RollbackDir)
	if it.RollbackDir == "" || resp.RollbackDir != rb {
		t.Errorf("rollback_dir %q, want %q", resp.RollbackDir, rb)
	}
	if l := e.auditLines(); len(l) != 1 || !strings.Contains(l[0], `"conflicts":[{"file":"device.conf"`) ||
		!strings.Contains(l[0], `"rollback_dir":"`+rb+`"`) {
		t.Errorf("audit %v", l)
	}
}

// TestRecoverIgnoresRollbackDir: recovery restores from intent.json, never
// from rollback/, so files planted there do not reach the configuration.
func TestRecoverIgnoresRollbackDir(t *testing.T) {
	e := newEnv(t)
	it := &Intent{}
	orig := killedWrite(t, e, it)
	rb := filepath.Join(e.state, rollbackDir, it.RollbackDir)
	for _, n := range []string{"device.conf", "library_contents.10", "mhvtl.conf", "library_contents.30"} {
		os.Remove(filepath.Join(rb, n))
		if err := os.WriteFile(filepath.Join(rb, n), []byte("planted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("/etc/shadow", filepath.Join(rb, "x"))
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultRecovered {
		t.Fatalf("recover: %+v", resp)
	}
	e.assertFiles("recover", orig)
}

// TestRecoverRefusesBadIntent: an intent naming a file outside the
// allowlist is not acted on.
func TestRecoverRefusesBadIntent(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(e.h.intentPath(), []byte(`{"v":1,"files":[{"name":"../shadow","existed":true,"original_sha256":"","new_sha256":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{OpRecover, OpPlan, OpApply} {
		resp := e.do(Request{Op: op})
		if resp.Result != ResultRefused || resp.Error != ErrIntentUnreadable || !strings.Contains(resp.Message, "../shadow") || !e.intentExists() {
			t.Errorf("%s with a bad intent: %+v", op, resp)
		}
	}
	if resp := e.do(Request{Op: OpHello}); resp.State != StateIntentUnreadable {
		t.Errorf("hello: %+v", resp)
	}
	if err := os.WriteFile(e.h.intentPath(), []byte(`{"v":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(Request{Op: OpHello}); resp.State != StateIntentUnreadable {
		t.Errorf("hello, truncated intent: %+v", resp)
	}
	if _, err := e.h.RecoverBoot(context.Background()); err == nil || !e.intentExists() {
		t.Errorf("boot recovery of a truncated intent: %v", err)
	}
}

func TestRecoverNothing(t *testing.T) {
	e := newEnv(t)
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultOK {
		t.Errorf("recover without an intent: %+v", resp)
	}
}

// killPoints lists the steps of an apply of req at which the helper can
// die, by running it once.
func killPoints(t *testing.T, setup func(*testing.T) (*env, *fakeSystem, Request)) []string {
	e, _, req := setup(t)
	var steps []string
	e.h.killPoint = func(step string) bool { steps = append(steps, step); return false }
	if resp := e.do(req); resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	return steps
}

func compressionEnv(t *testing.T) (*env, *fakeSystem, Request) {
	e, s := newExecEnv(t)
	return e, s, compression(e)
}

// TestRecoverAfterEveryStep: an apply killed after any step is recovered
// by the next request through the real lifecycle: guardedStop in recover
// mode (stopping whatever the apply had started), the files restored, the
// library's units started again, held offline, then online.
func TestRecoverAfterEveryStep(t *testing.T) {
	for _, step := range killPoints(t, compressionEnv) {
		t.Run(step, func(t *testing.T) {
			e, s, req := compressionEnv(t)
			dc, lc := e.read("device.conf"), e.read("library_contents.10")
			e.h.killPoint = func(st string) bool { return st == step }
			if resp := e.do(req); resp.Result != ResultError || !e.intentExists() {
				t.Fatalf("killed apply: %+v", resp)
			}
			s.calls = nil
			e.rebuild()
			resp := e.do(Request{Op: OpRecover})
			if resp.Result != ResultRecovered || e.intentExists() {
				t.Fatalf("recover: %+v", resp)
			}
			if string(e.read("device.conf")) != string(dc) || string(e.read("library_contents.10")) != string(lc) {
				t.Errorf("files not restored:\n%s", diffLines(string(lc), string(e.read("library_contents.10"))))
			}
			for _, u := range []string{"vtllibrary@10.service", "vtltape@11.service"} {
				if s.unitState(u) != "active" {
					t.Errorf("%s is %s", u, s.unitState(u))
				}
			}
			if !s.before("daemon-reload", "start vtllibrary@10.service") || !s.before("start vtllibrary@10.service", "online 10") {
				t.Errorf("recovery calls %v", s.calls)
			}
			if step == "started" && !s.before("offline 10", "stop vtllibrary@10.service") {
				t.Errorf("a running library was not stopped through guardedStop: %v", s.calls)
			}
		})
	}
}

// TestRecoverPersistJournaled: killed between journaling the .persist
// verdict and moving it aside (and so before any write), recovery writes
// the journaled .persist placements into library_contents and moves the
// .persist aside, so a host move made before the stop is kept.
func TestRecoverPersistJournaled(t *testing.T) {
	e, s := newExecEnv(t)
	e.replaceFile("device.conf", " PERSIST: False", " PERSIST: True")
	lc := string(e.read("library_contents.10"))
	moved := strings.Replace(strings.Replace(lc, "Slot 1: E01001L8", "Slot 1: ", 1), "Slot 21: \n", "Slot 21: E01001L8        \n", 1)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[0] = changer.SlotState{Number: 1}
		inv.Slots[20] = changer.SlotState{Number: 21, Full: true, Barcode: "E01001L8"}
	})
	s.onStop = func(u string) {
		if u == "vtllibrary@10.service" {
			os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(moved), 0o644)
		}
	}
	e.h.killPoint = func(st string) bool { return st == "apply: library 10 stopped" }
	req := compression(e)
	req.Files = append(req.Files, File{Name: "library_contents.10", BaseSHA256: e.sha("library_contents.10"),
		Content: []byte(strings.Replace(strings.Replace(lc, "Slot 1: E01001L8", "Slot 1:", 1), "Slot 21: \n", "Slot 21: E01001L8\n", 1))})
	if resp := e.do(req); resp.Result != ResultError {
		t.Fatalf("killed apply: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); err != nil {
		t.Fatalf(".persist not left by the kill: %v", err)
	}
	e.rebuild()
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultRecovered {
		t.Fatalf("recover: %+v", resp)
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nSlot 1:\n") || !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("journaled placements not written:\n%s", diffLines(lc, got))
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); !os.IsNotExist(err) {
		t.Errorf(".persist left in place: %v", err)
	}
}

// TestSIGTERMMidApply: the context cancelled while units start (SIGTERM)
// rolls back in the stop slack: the libraries come back online and the
// intent is gone.
func TestSIGTERMMidApply(t *testing.T) {
	e, s := newExecEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onStart = func(u string) {
		if u == "vtltape@11.service" {
			cancel()
		}
	}
	req := compression(e)
	req.V = Version
	resp := e.h.Handle(ctx, e.peer, &req)
	if resp.Result != ResultRolledBack || e.intentExists() {
		t.Fatalf("apply: %+v", resp)
	}
	if s.unitState("vtllibrary@10.service") != "active" || !strings.HasPrefix(s.calls[len(s.calls)-1], "online 10") {
		t.Errorf("library 10 not back online: %v", s.calls)
	}
}

// TestKilledOffline: an apply killed once its libraries are offline and
// before any stop has journaled R1′ and the offline set; recovery brings
// library 10 back online.
func TestKilledOffline(t *testing.T) {
	e, s, req := compressionEnv(t)
	e.h.killPoint = func(st string) bool { return st == "apply: offline" }
	if resp := e.do(req); resp.Result != ResultError {
		t.Fatalf("killed apply: %+v", resp)
	}
	if s.index("stop vtllibrary@10.service", 0) >= 0 {
		t.Errorf("stopped before the journal: %v", s.calls)
	}
	it, err := e.h.loadIntent()
	if err != nil {
		t.Fatal(err)
	}
	if it.Exec == nil || len(it.Exec.Prepared) != 1 || !it.Exec.Prepared[0].Offline || it.Exec.Prepared[0].R1 == nil || len(it.Libraries) != 1 {
		t.Fatalf("intent %+v", it.Exec)
	}
	s.calls = nil
	e.rebuild()
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultRecovered {
		t.Fatalf("recover: %+v", resp)
	}
	if s.index("online 10", 0) < 0 || s.unitState("vtllibrary@10.service") != "active" {
		t.Errorf("library 10 not back online: %v", s.calls)
	}
}

// TestKilledMidPrepare: an apply over libraries 10 and 30 killed after
// library 10's offline, during library 30's prepare, has journaled both
// as possibly offline; recovery brings both back online.
func TestKilledMidPrepare(t *testing.T) {
	e, s := newExecEnv(t)
	dc := string(e.read("device.conf"))
	i := strings.Index(dc, "Drive: 31 ")
	next := strings.Replace(dc[:i], " Compression type: lzo", " Compression type: zlib", 1) +
		strings.Replace(dc[i:], " Compression type: lzo", " Compression type: zlib", 1)
	req := Request{Op: OpApply, Actor: "admin", Files: []File{{Name: "device.conf", BaseSHA256: e.sha("device.conf"), Content: []byte(next)}},
		ExpectUnits: []string{"vtllibrary@10.service", "vtllibrary@30.service", "vtltape@11.service", "vtltape@31.service"}}
	e.h.killPoint = func(st string) bool { return st == "apply: offlining 30" }
	if resp := e.do(req); resp.Result != ResultError {
		t.Fatalf("killed apply: %+v", resp)
	}
	if !s.has("offline 10") || s.has("offline 30") || s.has("stop vtllibrary@10.service") {
		t.Errorf("calls before the kill: %v", s.calls)
	}
	it, err := e.h.loadIntent()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(it.Exec.Offlining) != "[10 30]" {
		t.Errorf("offlining %v", it.Exec.Offlining)
	}
	s.calls = nil
	e.rebuild()
	if resp := e.do(Request{Op: OpRecover}); resp.Result != ResultRecovered || e.intentExists() {
		t.Fatalf("recover: %+v", resp)
	}
	for _, id := range []string{"10", "30"} {
		if !s.has("online "+id) || s.unitState("vtllibrary@"+id+".service") != "active" {
			t.Errorf("library %s not back online: %v", id, s.calls)
		}
	}
}
