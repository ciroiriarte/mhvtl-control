package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mhvtl-control/internal/conffile"
)

// env is a helper over temporary directories, with mhVTL's shipped
// configuration (internal/conffile/testdata/roundtrip/shipped) whose media
// home points at a temporary directory.
type env struct {
	t               *testing.T
	h               *Helper
	cfg             *Config
	dir, state, run string
	home, legacy    string
	mu              sync.Mutex
	journal         []map[string]string
	systemctl       [][]string
	systemctlOut    string
	clock           time.Time
	peer            Peer
	peerErr         error
	// calls returns the fake system's calls so far, when there is one.
	calls func() []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		t:     t,
		dir:   filepath.Join(root, "etc-mhvtl"),
		state: filepath.Join(root, "state"),
		run:   filepath.Join(root, "run"),
		home:  filepath.Join(root, "home"),
		clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		peer:  Peer{PID: 4242, UID: 0, Cgroup: "/system.slice/mhvtl-control.service"},
	}
	e.legacy = filepath.Join(e.dir, "mhvtl-control.env")
	for _, d := range []string{e.dir, e.state, e.run, e.home, filepath.Join(root, "sys"), filepath.Join(root, "daemon-run")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shipped := "../conffile/testdata/roundtrip/shipped"
	for _, name := range []string{"device.conf", "library_contents.10", "library_contents.30", "mhvtl.conf"} {
		b, err := os.ReadFile(filepath.Join(shipped, name))
		if err != nil {
			t.Fatal(err)
		}
		b = []byte(strings.ReplaceAll(string(b), "/var/lib/mhvtl", e.home))
		if err := os.WriteFile(filepath.Join(e.dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bootID := filepath.Join(root, "boot_id")
	if err := os.WriteFile(bootID, []byte("11111111-2222-3333-4444-555555555555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.cfg = &Config{
		ConfigDir:   e.dir,
		StateDir:    e.state,
		RuntimeDir:  e.run,
		LegacyEnv:   e.legacy,
		DefaultHome: e.home,
		SysRoot:     filepath.Join(root, "sys"),
		BootIDPath:  bootID,
		Version:     "test",
		Getenv:      func(string) string { return "" },
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.clock = e.clock.Add(time.Second)
			return e.clock
		},
		Systemctl: func(_ context.Context, args ...string) ([]byte, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.systemctl = append(e.systemctl, args)
			return []byte(e.systemctlOut), nil
		},
		CheckPeer: func(*net.UnixConn) (Peer, error) { return e.peer, e.peerErr },
		Journal: func(f map[string]string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.journal = append(e.journal, f)
		},
		Logf:         t.Logf,
		ReadTimeout:  5 * time.Second,
		ApplyLockDir: filepath.Join(root, "daemon-run"),
		MoveHomeDir:  filepath.Join(root, "move-home"),
	}
	e.h = New(*e.cfg)
	return e
}

// rebuild makes a new helper after a test changed cfg.
func (e *env) rebuild() { e.h = New(*e.cfg) }

func (e *env) read(name string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) sha(name string) string { return conffile.Hash(e.read(name)) }

// edit returns a File replacing old with new in name, based on the file on
// disk.
func (e *env) edit(name, old, new string) File {
	e.t.Helper()
	b := string(e.read(name))
	if !strings.Contains(b, old) {
		e.t.Fatalf("%s has no %q", name, old)
	}
	return File{Name: name, BaseSHA256: conffile.Hash([]byte(b)), Content: []byte(strings.Replace(b, old, new, 1))}
}

// shippedUnits are every instance of the shipped configuration.
var shippedUnits = []string{"vtllibrary@10.service", "vtllibrary@30.service",
	"vtltape@11.service", "vtltape@12.service", "vtltape@13.service", "vtltape@14.service",
	"vtltape@31.service", "vtltape@32.service", "vtltape@33.service", "vtltape@34.service"}

// do sends req through Handle as the configured peer.
func (e *env) do(req Request) Response {
	e.t.Helper()
	if req.V == 0 {
		req.V = Version
	}
	if e.calls == nil || req.Op != OpApply || e.intentExists() {
		return e.h.Handle(context.Background(), e.peer, &req)
	}
	n := len(e.calls())
	resp := e.h.Handle(context.Background(), e.peer, &req)
	// An apply stops and starts only units its response lists: the set
	// the plan showed and expect_units approved. A rollback also stops the
	// vtltape@ of a drive it finds loaded, and reports the forced unload;
	// the drive's library must be one of the units.
	units := map[string]bool{}
	for _, u := range resp.Units {
		units[u] = true
	}
	if resp.Result != ResultApplied {
		for _, fu := range resp.ForcedUnloads {
			if !units[libraryUnit(fu.Library)] {
				e.t.Errorf("forced unload %+v: library %d is not among the units %v", fu, fu.Library, resp.Units)
				continue
			}
			units[driveUnit(fu.Drive)] = true
		}
	}
	for _, c := range e.calls()[n:] {
		verb, u, _ := strings.Cut(c, " ")
		if (verb == "stop" || verb == "start") && (strings.HasPrefix(u, "vtllibrary@") || strings.HasPrefix(u, "vtltape@")) && !units[u] {
			e.t.Errorf("the apply ran %q, but its units are %v", c, resp.Units)
		}
	}
	return resp
}

// auditLines returns the audit records written so far.
func (e *env) auditLines() []string {
	b, err := os.ReadFile(filepath.Join(e.state, auditFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// holdLock takes exclusive.lock as another process would, until the test
// ends or the returned func is called.
func (e *env) holdLock() func() {
	e.t.Helper()
	l, held, err := e.h.tryLock()
	if err != nil || held {
		e.t.Fatalf("tryLock: held %v, %v", held, err)
	}
	e.t.Cleanup(l.release)
	return l.release
}

// TestPlanValidates is the baseline the refusal tests depart from: a plan
// that edits a comment in device.conf passes every check, writes nothing,
// and is not audited.
func TestPlanValidates(t *testing.T) {
	e := newEnv(t)
	before := e.sha("device.conf")
	resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("device.conf", "# Each 'record'", "# Every 'record'")}})
	if resp.Result != ResultValidated || resp.Findings == nil {
		t.Fatalf("plan: %+v", resp)
	}
	if e.sha("device.conf") != before {
		t.Error("plan changed device.conf")
	}
	if l := e.auditLines(); l != nil {
		t.Errorf("a validated plan was audited: %v", l)
	}
}

// TestApplyNoUnits: an apply whose change restarts nothing (a comment in
// device.conf) writes the file, touches no unit, ends its intent and is
// audited.
func TestApplyNoUnits(t *testing.T) {
	e := newEnv(t)
	f := e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	resp := e.do(Request{Op: OpApply, Actor: "admin", Files: []File{f}})
	if resp.Result != ResultApplied || len(resp.Units) != 0 {
		t.Fatalf("apply: %+v", resp)
	}
	if string(e.read("device.conf")) != string(f.Content) || e.intentExists() {
		t.Error("device.conf not written, or intent left")
	}
	for _, c := range e.systemctl {
		if c[0] != "show" {
			t.Errorf("systemctl %v", c)
		}
	}
	if l := e.auditLines(); len(l) != 1 || !strings.Contains(l[0], `"result":"applied"`) {
		t.Errorf("audit: %v", l)
	}
}

// TestHelloStates covers every state a hello reports, and that it changes
// nothing.
func TestHelloStates(t *testing.T) {
	e := newEnv(t)
	hello := func() string {
		t.Helper()
		resp := e.do(Request{Op: OpHello})
		if resp.Result != ResultOK || resp.Version != "test" {
			t.Fatalf("hello: %+v", resp)
		}
		return resp.State
	}
	if s := hello(); s != StateReady {
		t.Errorf("state %s, want ready", s)
	}
	if err := os.WriteFile(e.legacy, []byte("MHVTL_CONTROL_API_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := hello(); s != StateLegacyEnv {
		t.Errorf("state %s, want legacy_env_blocks", s)
	}
	os.Remove(e.legacy)
	if err := e.h.saveIntent(&Intent{V: Version, Op: OpApply}); err != nil {
		t.Fatal(err)
	}
	if s := hello(); s != StateIncomplete {
		t.Errorf("state %s, want incomplete_apply", s)
	}
	release := e.holdLock()
	if s := hello(); s != StateInProgress {
		t.Errorf("state %s, want apply_in_progress", s)
	}
	release()
	if _, err := os.Stat(e.h.intentPath()); err != nil {
		t.Errorf("hello touched the intent: %v", err)
	}
	if l := e.auditLines(); l != nil {
		t.Errorf("hello was audited: %v", l)
	}
}

// units reports the ActiveState of the vtllibrary@ instance of every
// library in device.conf, for the daemon, which cannot ask systemd
// itself. It changes nothing, takes no lock and is not audited.
func TestUnits(t *testing.T) {
	e := newEnv(t)
	e.cfg.Systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[len(args)-1] {
		case "vtllibrary@10.service":
			return []byte("active\n"), nil
		case "vtllibrary@30.service":
			return []byte("failed\n"), nil
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}
	e.rebuild()
	release := e.holdLock()
	resp := e.do(Request{Op: OpUnits})
	release()
	if resp.Result != ResultOK || !reflect.DeepEqual(resp.LibraryStates, map[int]string{10: "active", 30: "failed"}) {
		t.Errorf("units: %+v", resp)
	}
	if l := e.auditLines(); l != nil {
		t.Errorf("units was audited: %v", l)
	}
	if resp := serveOnce(t, e.h, `{"v":1,"op":"units"}`); resp.Result != ResultOK || resp.LibraryStates[30] != "failed" {
		t.Errorf("units over the socket: %+v", resp)
	}
	e.cfg.Systemctl = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("no bus") }
	e.rebuild()
	if resp := e.do(Request{Op: OpUnits}); resp.Result == ResultOK || !strings.Contains(resp.Message, "no bus") {
		t.Errorf("units without systemctl: %+v", resp)
	}
}

// serveOnce sends body to h.Serve over a socket pair, as a client that
// half-closes after the request, and returns the response.
func serveOnce(t *testing.T, h *Helper, body string) Response {
	t.Helper()
	server, client := socketPair(t)
	done := make(chan error, 1)
	go func() { done <- h.Serve(context.Background(), server) }()
	if _, err := client.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	client.CloseWrite()
	b, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("response %q: %v", b, err)
	}
	if resp.V != Version {
		t.Errorf("response version %d", resp.V)
	}
	return resp
}

// socketPair returns the two ends of a connected Unix stream socket.
func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "socket")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.(*net.UnixConn)
	}
	return conn(fds[0]), conn(fds[1])
}

// TestLockWait: a plan waits up to lockWait for a lock held briefly, as a
// hello holds it, and reports apply_in_progress only past that.
func TestLockWait(t *testing.T) {
	e := newEnv(t)
	release := e.holdLock()
	time.AfterFunc(20*time.Millisecond, release)
	if resp := e.do(Request{Op: OpPlan}); resp.Result == ResultInProgress {
		t.Errorf("lock held 20 ms: %+v", resp)
	}

	e.holdLock()
	start := time.Now()
	resp := e.do(Request{Op: OpPlan})
	if resp.Result != ResultInProgress {
		t.Errorf("lock held: %+v", resp)
	}
	if d := time.Since(start); d < lockWait {
		t.Errorf("gave up after %v, before %v", d, lockWait)
	}
}
