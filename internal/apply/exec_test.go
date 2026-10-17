package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// fakeSystem is mhVTL as the exec sequence sees it, over an env's
// configuration directory: units that systemctl starts and stops, the
// message queue, and changers whose READ ELEMENT STATUS is the
// library_contents on disk when the library started, unless a test sets
// the live placement.
type fakeSystem struct {
	e     *env
	mu    sync.Mutex
	calls []string
	state map[string]string // ActiveState by unit; vtl units default to active
	// live overrides READ ELEMENT STATUS of a library until it stops.
	live map[int]changer.Inventory
	// started is the inventory of a library read from disk at its start.
	started map[int]changer.Inventory
	loaded  map[string]bool // TUR by drive serial
	// lost are the drive serials LocateDrive does not find.
	lost map[string]bool
	// killStart makes the next start of the unit return as if SIGTERM
	// killed systemctl: the start job goes on, the unit is left in the
	// state given (activating).
	killStart map[string]string
	// deaf are the running libraries that stop answering the queue.
	deaf map[int]bool
	// onOffline runs at an offline, with the lock held.
	onOffline func(lib int)
	enabled   map[string]bool
	// failStart makes the next start of the unit fail; onStop runs at a
	// stop, onMedia at the restart of mhvtl-media.service.
	failStart map[string]bool
	failStop  map[string]bool
	onStop    func(unit string)
	onMedia   func() error
	onStart   func(unit string)
	// onListMap answers "list map"; nil answers "".
	onListMap func(lib int) string
	// offline are the libraries taken offline: their READ ELEMENT STATUS
	// is NOT READY / OFFLINE (02/04/12) until online or a restart, as
	// vtllibrary answers it (usr/cmd/vtllibrary.c:213-225 at 852b78a).
	// An online takes effect only with the next queue message to the
	// library (pendingOnline): vtllibrary takes one message per pass of
	// its loop (usr/cmd/vtllibrary.c:1877-1924).
	offline, pendingOnline map[int]bool
}

func newExecEnv(t *testing.T) (*env, *fakeSystem) {
	e := newEnv(t)
	s := &fakeSystem{e: e, state: map[string]string{bayMonitor: "inactive"}, live: map[int]changer.Inventory{}, started: map[int]changer.Inventory{},
		loaded: map[string]bool{}, lost: map[string]bool{}, killStart: map[string]string{}, deaf: map[int]bool{}, enabled: map[string]bool{}, offline: map[int]bool{}, pendingOnline: map[int]bool{}, failStart: map[string]bool{}, failStop: map[string]bool{},
		onStop: func(string) {}, onMedia: func() error { return nil }, onStart: func(string) {}}
	e.cfg.Systemctl = s.systemctl
	e.cfg.Queue, e.cfg.Devices = s, s
	e.calls = func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]string{}, s.calls...)
	}
	e.cfg.JournalTail = func(_ context.Context, unit string) ([]byte, error) { return []byte("tail of " + unit), nil }
	e.rebuild()
	// Every cartridge of the shipped configuration exists.
	for _, lib := range s.inventory().Libraries {
		for _, sl := range lib.Slots {
			if sl.Barcode != "" {
				os.MkdirAll(filepath.Join(e.home, sl.Barcode), 0o755)
			}
		}
	}
	return e, s
}

func (s *fakeSystem) log(format string, a ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, a...))
}

func (s *fakeSystem) inventory() config.Inventory {
	inv, err := config.ReadInventory(s.e.dir, func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(s.e.dir, name)) })
	if err != nil {
		s.e.t.Fatal(err)
	}
	return inv
}

// fromDisk is the inventory library id starts with, from its
// library_contents on disk now.
func (s *fakeSystem) fromDisk(id int) changer.Inventory {
	var inv changer.Inventory
	lib, ok := findLib(s.inventory(), id)
	if !ok {
		return inv
	}
	byNum := map[int]string{}
	for _, sl := range lib.Slots {
		byNum[sl.Number] = sl.Barcode
	}
	for _, n := range sortedInts(slotNumbers(lib)) {
		inv.Slots = append(inv.Slots, changer.SlotState{Number: n, Full: byNum[n] != "", Barcode: byNum[n]})
	}
	for _, m := range lib.MAPs {
		inv.MAPs = append(inv.MAPs, changer.SlotState{Number: m.Number, Full: m.Barcode != "", Barcode: m.Barcode})
	}
	for _, d := range lib.DriveLines {
		inv.Drives = append(inv.Drives, changer.DriveState{Number: d.Number})
	}
	return inv
}

func (s *fakeSystem) unitState(u string) string {
	if st, ok := s.state[u]; ok {
		return st
	}
	return "active"
}

func (s *fakeSystem) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err // exec.CommandContext does not start
	}
	switch args[0] {
	case "show":
		unit := args[3]
		switch args[1] {
		case "--property=ActiveState":
			return []byte(s.unitState(unit) + "\n"), nil
		case "--property=Result":
			return []byte("success\n"), nil
		}
	case "stop":
		s.log("stop %s", args[1])
		if s.failStop[args[1]] {
			return nil, errors.New("exit status 1")
		}
		s.state[args[1]] = "inactive"
		var id int
		if _, err := fmt.Sscanf(args[1], "vtllibrary@%d.service", &id); err == nil {
			delete(s.live, id)
			delete(s.started, id)
			delete(s.offline, id)
			delete(s.pendingOnline, id)
		}
		s.onStop(args[1])
		return nil, nil
	case "start":
		s.log("start %s", args[1])
		if st, ok := s.killStart[args[1]]; ok {
			delete(s.killStart, args[1]) // once
			s.state[args[1]] = st
			return nil, errors.New("signal: killed")
		}
		if s.failStart[args[1]] {
			delete(s.failStart, args[1]) // once
			s.state[args[1]] = "failed"
			return nil, errors.New("exit status 1")
		}
		s.state[args[1]] = "active"
		var id int
		if _, err := fmt.Sscanf(args[1], "vtllibrary@%d.service", &id); err == nil {
			s.started[id] = s.fromDisk(id)
			delete(s.offline, id)
			delete(s.pendingOnline, id)
		}
		s.onStart(args[1])
		return nil, nil
	case "restart":
		s.log("restart %s", args[1])
		return nil, s.onMedia()
	case "daemon-reload":
		s.log("daemon-reload")
		return nil, nil
	case "enable", "disable":
		s.log("%s %s", args[0], args[1])
		s.enabled[args[1]] = args[0] == "enable"
		return nil, nil
	case "is-enabled":
		if s.enabled[args[1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	case "list-units":
		return nil, nil
	}
	s.e.t.Fatalf("systemctl %v", args)
	return nil, nil
}

func (s *fakeSystem) Offline(lib int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("offline %d", lib)
	delete(s.pendingOnline, int(lib))
	s.offline[int(lib)] = true
	if s.onOffline != nil {
		s.onOffline(int(lib))
	}
	return nil
}

func (s *fakeSystem) Online(lib int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("online %d", lib)
	if s.offline[int(lib)] {
		s.pendingOnline[int(lib)] = true
	}
	return nil
}
func (s *fakeSystem) ListMap(lib int64) (string, error) {
	s.log("list map %d", lib)
	s.mu.Lock()
	if s.pendingOnline[int(lib)] {
		delete(s.pendingOnline, int(lib))
		delete(s.offline, int(lib))
	}
	deaf := s.deaf[int(lib)]
	s.mu.Unlock()
	if deaf || s.unitState(libraryUnit(int(lib))) != "active" {
		return "", errors.New("no reply")
	}
	if s.onListMap != nil {
		return s.onListMap(int(lib)), nil
	}
	return "", nil
}

// LocateLibrary finds a changer only while its vtllibrary@ is active: an
// activating one has not registered it yet.
func (s *fakeSystem) LocateLibrary(lib config.Library) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.unitState(libraryUnit(lib.ID)); st != "active" {
		return "", fmt.Errorf("%w: no medium changer with serial %q (vtllibrary@%d is %s)", changer.ErrUnavailable, lib.Serial, lib.ID, st)
	}
	return "hctl-" + lib.Serial, nil
}

// LocateDrive finds a drive only while its vtltape@ is active: a stopped
// vtltape has removed its device.
func (s *fakeSystem) LocateDrive(d config.Drive) (string, error) {
	s.mu.Lock()
	st := s.unitState(driveUnit(d.ID))
	s.mu.Unlock()
	if s.lost[d.Serial] || st == "inactive" || st == "failed" {
		return "", fmt.Errorf("%w: no tape drive with serial %q", changer.ErrUnavailable, d.Serial)
	}
	return "hctl-" + d.Serial, nil
}

func (s *fakeSystem) Inventory(lib config.Library, hctl string) (changer.Inventory, error) {
	s.log("RES %d", lib.ID)
	s.mu.Lock()
	off := s.offline[lib.ID]
	s.mu.Unlock()
	if off {
		return changer.Inventory{}, &scsi.SenseError{Key: 0x02, ASC: 0x04, ASCQ: 0x12}
	}
	if inv, ok := s.live[lib.ID]; ok {
		return inv, nil
	}
	if inv, ok := s.started[lib.ID]; ok {
		return inv, nil
	}
	return s.fromDisk(lib.ID), nil
}

// Loaded is TEST UNIT READY: a drive whose vtltape@ is not running does
// not answer.
func (s *fakeSystem) Loaded(hctl string) (bool, error) {
	serial := strings.TrimPrefix(hctl, "hctl-")
	inv := s.inventory()
	drives := inv.Standalone
	for _, l := range inv.Libraries {
		drives = append(drives, l.Drives...)
	}
	for _, d := range drives {
		if d.Serial == serial && s.unitState(driveUnit(d.ID)) != "active" {
			return false, fmt.Errorf("%s: no response", hctl)
		}
	}
	return s.loaded[serial], nil
}

// steps reduces the call log to the kinds of call, in order, collapsing
// repeats: the order the plan asserts.
func (s *fakeSystem) steps() []string {
	var out []string
	for _, c := range s.calls {
		k := c
		for _, p := range []string{"stop ", "start ", "offline ", "online ", "list map ", "RES ", "restart ", "enable ", "disable "} {
			if strings.HasPrefix(c, p) {
				k = strings.TrimSpace(p)
			}
		}
		if len(out) == 0 || out[len(out)-1] != k {
			out = append(out, k)
		}
	}
	return out
}

func (s *fakeSystem) has(call string) bool {
	for _, c := range s.calls {
		if c == call {
			return true
		}
	}
	return false
}

// index is the position of call in the log at or after from, -1 when
// absent.
func (s *fakeSystem) index(call string, from int) int {
	for i := from; i < len(s.calls); i++ {
		if s.calls[i] == call {
			return i
		}
	}
	return -1
}

// before reports whether a is logged, and b after it.
func (s *fakeSystem) before(a, b string) bool {
	i := s.index(a, 0)
	return i >= 0 && s.index(b, i) >= 0
}

// compression edits drive 11 so that it and library 10 restart.
func compression(e *env) Request {
	return Request{Op: OpApply, Actor: "admin", Files: []File{e.edit("device.conf", " Compression type: lzo", " Compression type: zlib")},
		ExpectUnits: []string{"vtllibrary@10.service", "vtltape@11.service"}}
}

// TestExecOrder asserts the call order of plan 3.3 (exec_test.go): plan
// checks (R1) → guardedStop (offline, barrier, stop) → write → reload →
// start → hold offline → online → R2, with the library stopped before its
// drive and started after it (amendment 1).
func TestExecOrder(t *testing.T) {
	e, s := newExecEnv(t)
	gen := filepath.Join(e.dir, "generator")
	os.WriteFile(gen, nil, 0o755)
	e.cfg.Generators = []string{gen}
	e.rebuild()
	before := e.sha("device.conf")
	resp := e.do(compression(e))
	if resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	if e.sha("device.conf") == before || e.intentExists() {
		t.Error("device.conf not written, or intent left")
	}
	got := strings.Join(s.steps(), " > ")
	order := "RES > list map > offline > list map > stop > daemon-reload > start > list map > offline > online > list map > RES"
	if got != order {
		t.Errorf("order\n got %s\nwant %s", got, order)
	}
	for _, pair := range [][2]string{
		{"stop vtllibrary@10.service", "stop vtltape@11.service"},
		{"start vtltape@11.service", "start vtllibrary@10.service"},
		{"start vtllibrary@10.service", "offline 10"},
	} {
		if !s.before(pair[0], pair[1]) {
			t.Errorf("%s not before %s", pair[0], pair[1])
		}
	}
	if s.has("stop vtllibrary@30.service") {
		t.Error("library 30 restarted")
	}
	if len(resp.PlacementDrift) != 0 || len(resp.Warnings) != 0 {
		t.Errorf("drift %v, warnings %v", resp.PlacementDrift, resp.Warnings)
	}
}

// addLibrary50 adds library 50, with no drives and two empty slots.
func addLibrary50(e *env, units ...string) Request {
	dc := e.read("device.conf")
	add := fmt.Sprintf("\nLibrary: 50 CHANNEL: 00 TARGET: 13 LUN: 00\n Vendor identification: STK\n Product identification: L80\n Unit serial number: XYZZY_C\n NAA: 50:22:33:44:ab:00:13:00\n Home directory: %s\n PERSIST: False\n", e.home)
	return Request{Op: OpApply, Actor: "admin",
		Files: []File{
			{Name: "device.conf", BaseSHA256: e.sha("device.conf"), Content: append(append([]byte{}, dc...), add...)},
			{Name: "library_contents.50", Content: []byte("VERSION: 2\n\nPicker 1:\n\nMAP 1:\n\nSlot 1:\nSlot 2:\n")},
		},
		ExpectUnits: append([]string{"vtllibrary@50.service"}, units...)}
}

// TestExecAddLibrary: an added library's units are started, and without
// the generator enabled (AC 11); nothing else restarts.
func TestExecAddLibrary(t *testing.T) {
	for _, gen := range []bool{false, true} {
		t.Run(fmt.Sprint("generator ", gen), func(t *testing.T) {
			e, s := newExecEnv(t)
			if gen {
				p := filepath.Join(e.dir, "generator")
				os.WriteFile(p, nil, 0o755)
				e.cfg.Generators = []string{p}
				e.rebuild()
			}
			resp := e.do(addLibrary50(e))
			if resp.Result != ResultApplied {
				t.Fatalf("apply: %+v", resp)
			}
			if s.has("daemon-reload") != gen || s.has("enable vtllibrary@50.service") == gen {
				t.Errorf("generator %v: calls %v", gen, s.calls)
			}
			if !s.before("start vtllibrary@50.service", "offline 50") || !s.before("offline 50", "online 50") {
				t.Errorf("not held offline: %v", s.calls)
			}
			for _, c := range s.calls {
				if strings.HasPrefix(c, "stop ") {
					t.Errorf("stopped a unit: %s", c)
				}
			}
		})
	}
}

// TestExecCreateRollback: a start failure after a library was added rolls
// back: the new library_contents is deleted, device.conf restored, the
// enabled unit disabled again, daemon-reload runs before any start
// (amendment 6), and the response names the unit with its journal tail.
func TestExecCreateRollback(t *testing.T) {
	e, s := newExecEnv(t)
	dc := e.read("device.conf")
	s.failStart["vtllibrary@50.service"] = true
	resp := e.do(addLibrary50(e))
	if resp.Result != ResultRolledBack || resp.Error != ErrStart || resp.FailedUnit != "vtllibrary@50.service" || resp.JournalTail != "tail of vtllibrary@50.service" {
		t.Fatalf("apply: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.50")); !os.IsNotExist(err) {
		t.Errorf("library_contents.50 left: %v", err)
	}
	if string(e.read("device.conf")) != string(dc) || e.intentExists() {
		t.Error("device.conf not restored, or intent left")
	}
	if !s.before("enable vtllibrary@50.service", "disable vtllibrary@50.service") {
		t.Errorf("not disabled again: %v", s.calls)
	}
	last := len(s.calls) - 1
	if i := s.index("daemon-reload", s.index("start vtllibrary@50.service", 0)); i < 0 || i > last {
		t.Errorf("no daemon-reload in the rollback: %v", s.calls)
	}
}

// TestExecStartFailure: a restarted library that fails to start is
// stopped again through guardedStop in rollback mode, the files are
// restored, and after daemon-reload the units that ran come back, held
// offline, then online.
func TestExecStartFailure(t *testing.T) {
	e, s := newExecEnv(t)
	dc, lc := e.read("device.conf"), e.read("library_contents.10")
	s.failStart["vtllibrary@10.service"] = true
	resp := e.do(compression(e))
	if resp.Result != ResultRolledBack || resp.FailedUnit != "vtllibrary@10.service" {
		t.Fatalf("apply: %+v", resp)
	}
	if string(e.read("device.conf")) != string(dc) || e.intentExists() {
		t.Error("device.conf not restored, or intent left")
	}
	if got := string(e.read("library_contents.10")); got != string(lc) {
		t.Errorf("library_contents.10 not restored:\n%s", diffLines(string(lc), got))
	}
	first := s.index("start vtllibrary@10.service", 0)
	rest := s.calls[first+1:]
	want := []string{"stop vtllibrary@10.service", "stop vtltape@11.service", "daemon-reload",
		"start vtltape@11.service", "start vtllibrary@10.service", "list map 10", "offline 10", "online 10", "list map 10"}
	var got []string
	for _, c := range rest {
		for _, w := range want {
			if c == w && (len(got) == 0 || got[len(got)-1] != c) {
				got = append(got, c)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rollback calls\n got %v\nwant %v", got, want)
	}
	if s.unitState("vtllibrary@10.service") != "active" {
		t.Error("library 10 not running after the rollback")
	}
}

// TestExecRaced: the MAP changing between R1′ and the offline is raced:
// nothing is stopped, the library is online again, nothing is written.
func TestExecRaced(t *testing.T) {
	e, s := newExecEnv(t)
	before := e.sha("device.conf")
	n := 0
	s.onListMap = func(lib int) string {
		n++
		if n == 2 {
			return "E01001L8"
		}
		return ""
	}
	resp := e.do(compression(e))
	if resp.Result != ResultRefused || resp.Error != ErrRaced {
		t.Fatalf("apply: %+v", resp)
	}
	if e.sha("device.conf") != before || e.intentExists() {
		t.Error("written, or intent left")
	}
	if !s.before("offline 10", "online 10") {
		t.Errorf("not online again: %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "stop ") {
			t.Errorf("stopped %s", c)
		}
	}
}

// TestExecBusy: apply.lock held by the daemon is busy; a move-home journal
// planned or in an unsettled step refuses too, a settled one does not.
func TestExecBusy(t *testing.T) {
	e, _ := newExecEnv(t)
	f, err := openApplyLock(e.cfg.ApplyLockDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(compression(e)); resp.Result != ResultRefused || resp.Error != ErrBusy {
		t.Errorf("held: %+v", resp)
	}
	f.Close()

	for step, refused := range map[string]bool{"copying": true, "switched": true, "planned": true, "done": false, "cleaned": false, "rolled_back": false} {
		dir := filepath.Join(e.cfg.MoveHomeDir, "m1")
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "journal.json"), []byte(`{"step":"`+step+`"}`), 0o600)
		resp := e.do(compression(e))
		if got := resp.Error == ErrMoveHome; got != refused {
			t.Errorf("move-home %s: %+v", step, resp)
		}
		if step == "planned" && (!strings.Contains(resp.Message, "m1") || !strings.Contains(resp.Message, "run or cancel")) {
			t.Errorf("move-home planned: %q", resp.Message)
		}
		if resp.Result == ResultApplied {
			e, _ = newExecEnv(t) // start over from the shipped files
		}
	}
}

// diffLines lists the lines of b that differ from a.
func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("%d: %q -> %q", i+1, x, y))
		}
	}
	return strings.Join(out, "\n")
}

// replaceFile rewrites name on disk, replacing old with new once.
func (e *env) replaceFile(name, old, new string) {
	e.t.Helper()
	f := e.edit(name, old, new)
	if err := os.WriteFile(filepath.Join(e.dir, name), f.Content, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// liveWith is library 10's inventory from disk with change applied.
func (s *fakeSystem) liveWith(id int, change func(*changer.Inventory)) {
	inv := s.fromDisk(id)
	change(&inv)
	s.live[id] = inv
}

// TestExecPersistMismatch is Q7: with PERSIST on, a trusted .persist
// written at the stop that disagrees with R1 refuses and rolls back,
// keeping the .persist placements in library_contents.
func TestExecPersistMismatch(t *testing.T) {
	e, s := newExecEnv(t)
	e.replaceFile("device.conf", " PERSIST: False", " PERSIST: True")
	lc := string(e.read("library_contents.10"))
	s.onStop = func(u string) {
		if u == "vtllibrary@10.service" {
			p := strings.Replace(strings.Replace(lc, "Slot 1: E01001L8", "Slot 1: ", 1), "Slot 21: \n", "Slot 21: E01001L8        \n", 1)
			os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(p), 0o644)
		}
	}
	resp := e.do(compression(e))
	if resp.Result != ResultRolledBack || resp.Error != ErrPersistMismatch {
		t.Fatalf("apply: %+v", resp)
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nSlot 1:\n") || !strings.Contains(got, "\nSlot 21: E01001L8\n") {
		t.Errorf("the .persist placements were not kept:\n%s", diffLines(lc, got))
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); !os.IsNotExist(err) {
		t.Errorf(".persist left in place: %v", err)
	}
	if !s.before("daemon-reload", "start vtllibrary@10.service") || s.unitState("vtllibrary@10.service") != "active" {
		t.Errorf("library 10 not back: %v", s.calls)
	}
}

// TestExecPersistToggled: PERSIST turned off or on restarts the library;
// a trusted .persist written at the stop is the placement source and is
// moved aside, so the merged library_contents is what the library loads.
func TestExecPersistToggled(t *testing.T) {
	for _, from := range []string{"False", "True"} {
		t.Run("from "+from, func(t *testing.T) {
			e, s := newExecEnv(t)
			to := map[string]string{"False": "True", "True": "False"}[from]
			e.replaceFile("device.conf", " PERSIST: False", " PERSIST: "+from)
			lc := string(e.read("library_contents.10"))
			s.onStop = func(u string) {
				if u == "vtllibrary@10.service" && from == "True" {
					os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte(lc), 0o644)
				}
			}
			resp := e.do(Request{Op: OpApply, Files: []File{e.edit("device.conf", " PERSIST: "+from, " PERSIST: "+to)}, ExpectUnits: []string{"vtllibrary@10.service"}})
			if resp.Result != ResultApplied {
				t.Fatalf("apply: %+v", resp)
			}
			if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); !os.IsNotExist(err) {
				t.Errorf(".persist left in place: %v", err)
			}
			if got := string(e.read("library_contents.10")); got != lc {
				t.Errorf("library_contents.10 changed:\n%s", diffLines(lc, got))
			}
		})
	}
}

// TestExecLoaded: a loaded drive of a library the apply restarts is
// refused with drive_loaded, and the plan returns a force token; applied
// with it, the drive's vtltape@ is restarted with the library and its
// cartridge written to the slot it came from, never on a Drive line.
func TestExecLoaded(t *testing.T) {
	e, s := newExecEnv(t)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
	})
	s.loaded["XYZZY_A2"] = true
	// The client builds next from the live view: slot 2 is empty.
	req := compression(e)
	req.Files = append(req.Files, e.edit("library_contents.10", "Slot 2: E01002L8\n", "Slot 2:\n"))
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrDriveLoaded || resp.ForceToken != "" {
		t.Fatalf("apply: %+v", resp)
	}
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Error != ErrDriveLoaded || pr.ForceToken == "" || pr.ForceExpires == nil {
		t.Fatalf("plan: %+v", pr)
	}
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied {
		t.Fatalf("forced apply: %+v", resp)
	}
	if len(resp.ForcedUnloads) != 1 || resp.ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 12, Barcode: "E01002L8", SourceSlot: 2}) {
		t.Errorf("forced unloads %+v", resp.ForcedUnloads)
	}
	if !s.before("stop vtllibrary@10.service", "stop vtltape@12.service") || !s.before("start vtltape@12.service", "start vtllibrary@10.service") {
		t.Errorf("drive 12 not restarted with its library: %v", s.calls)
	}
	got := string(e.read("library_contents.10"))
	if !strings.Contains(got, "\nSlot 2: E01002L8\n") || strings.Contains(got, "Drive 2: E") {
		t.Errorf("library_contents.10:\n%s", got)
	}
}

// TestExecMedia: new pre-filled barcodes restart mhvtl-media.service, and
// the stat of each decides: a missing one rolls back even with exit 0,
// exit 1 with every one present does not.
// TestExecDriveNotFound: the apply's prepare locates every drive of a
// library it stops, to read it with TEST UNIT READY, so the plan does too:
// a drive it cannot find is a live_state refusal at plan time, not an
// internal error after the plan passed, and nothing is stopped.
func TestExecDriveNotFound(t *testing.T) {
	e, s := newExecEnv(t)
	s.lost["XYZZY_A2"] = true
	req := compression(e)
	plan := req
	plan.Op = OpPlan
	if pr := e.do(plan); pr.Result != ResultRefused || pr.Error != ErrLive || !strings.Contains(pr.Message, "drive 12") {
		t.Fatalf("plan: %+v", pr)
	}
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrLive {
		t.Fatalf("apply: %+v", resp)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "stop ") {
			t.Errorf("stopped %q", c)
		}
	}
}

// TestExecDriveLostInPrepare: a drive the prepare cannot locate after it
// took the library offline is a live_state refusal, not an internal error,
// and the library is put back online.
func TestExecDriveLostInPrepare(t *testing.T) {
	e, s := newExecEnv(t)
	s.onOffline = func(int) { s.lost["XYZZY_A2"] = true }
	resp := e.do(compression(e))
	if resp.Result != ResultRefused || resp.Error != ErrLive || !strings.Contains(resp.Message, "drive 12") {
		t.Fatalf("apply: %+v", resp)
	}
	if !s.before("offline 10", "online 10") || s.offline[10] && !s.pendingOnline[10] {
		t.Errorf("library 10 not put back online: %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "stop ") {
			t.Errorf("stopped %q", c)
		}
	}
	if e.intentExists() {
		t.Error("intent left")
	}
}

// TestExecRollbackStartKilled: SIGTERM kills the systemctl client of a
// start, but the start job goes on and the library stays activating, its
// changer not registered yet (VM run2b). The rollback stops it as a
// library that is not running and builds on the apply's source.
func TestExecRollbackStartKilled(t *testing.T) {
	e, s := newExecEnv(t)
	dc, lc := e.read("device.conf"), e.read("library_contents.10")
	s.killStart["vtllibrary@10.service"] = "activating"
	resp := e.do(compression(e))
	if resp.Result != ResultRolledBack {
		t.Fatalf("apply: %+v", resp)
	}
	if string(e.read("device.conf")) != string(dc) || e.intentExists() {
		t.Error("device.conf not restored, or intent left")
	}
	if got := string(e.read("library_contents.10")); got != string(lc) {
		t.Errorf("library_contents.10 not restored:\n%s", diffLines(string(lc), got))
	}
	if !s.before("start vtllibrary@10.service", "stop vtllibrary@10.service") {
		t.Errorf("the activating library was not stopped: %v", s.calls)
	}
	if st := s.unitState("vtllibrary@10.service"); st != "active" {
		t.Errorf("vtllibrary@10 is %s after the rollback", st)
	}
}

// TestExecRollbackDeactivating: a library deactivating as the rollback
// begins may still run, and in recover hold moves made since R1; it is not
// taken for one that is not running, and the rollback fails closed.
func TestExecRollbackDeactivating(t *testing.T) {
	e, s := newExecEnv(t)
	s.killStart["vtllibrary@10.service"] = "deactivating"
	resp := e.do(compression(e))
	if resp.Result != ResultError || !strings.Contains(resp.Message, "and so did its rollback") || !e.intentExists() {
		t.Fatalf("apply: %+v", resp)
	}
}

// TestExecDriveDown: a running library's drive whose vtltape@ is down
// has no device to locate; the apply that would fix it is not refused
// (#53). Its cartridge, if any, is the one READ ELEMENT STATUS shows.
func TestExecDriveDown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive string // the drive down
	}{
		// Drive 11 is the one the change edits, so the apply restarts it.
		{"in the plan", "vtltape@11.service"},
		// Drive 12 is not: it stays down.
		{"not in the plan", "vtltape@12.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newExecEnv(t)
			s.state[tc.drive] = "failed"
			req := compression(e)
			plan := req
			plan.Op = OpPlan
			if pr := e.do(plan); pr.Result != ResultValidated {
				t.Fatalf("plan: %+v", pr)
			}
			if resp := e.do(req); resp.Result != ResultApplied {
				t.Fatalf("apply: %+v", resp)
			}
			if tc.drive == "vtltape@11.service" {
				if !s.before("stop vtltape@11.service", "start vtltape@11.service") || s.unitState(tc.drive) != "active" {
					t.Errorf("drive 11 not restarted: %v", s.calls)
				}
			} else if s.has("stop vtltape@12.service") || s.has("start vtltape@12.service") || s.unitState(tc.drive) != "failed" {
				t.Errorf("drive 12 touched: %v", s.calls)
			}
		})
	}
}

// TestExecStandaloneDown: the same for a standalone drive, which no
// changer reports: whose vtltape@ is down holds no cartridge, since
// vtltape keeps no mount state, and its edit applies.
func TestExecStandaloneDown(t *testing.T) {
	e, s := newExecEnv(t)
	e.replaceFile("device.conf", " Library ID: 30 Slot: 04", " Library ID: 0 Slot: 00")
	e.replaceFile("library_contents.30", "Drive 4:\n", "")
	s.state["vtltape@34.service"] = "failed"
	dc := string(e.read("device.conf"))
	i := strings.Index(dc, "Drive: 34 ")
	req := Request{Op: OpApply, Actor: "admin", Files: []File{{Name: "device.conf", BaseSHA256: e.sha("device.conf"),
		Content: []byte(dc[:i] + strings.Replace(dc[i:], " Compression type: lzo", " Compression type: zlib", 1))}},
		ExpectUnits: []string{"vtltape@34.service"}}
	plan := req
	plan.Op = OpPlan
	if pr := e.do(plan); pr.Result != ResultValidated {
		t.Fatalf("plan: %+v", pr)
	}
	if resp := e.do(req); resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	if !s.before("stop vtltape@34.service", "start vtltape@34.service") {
		t.Errorf("drive 34 not restarted: %v", s.calls)
	}
}

// TestExecDriveDownLoaded: a down drive that READ ELEMENT STATUS shows
// full cannot be asked, so the cartridge counts as loaded: drive_loaded,
// and forced, it goes back to its slot. vtltape keeps no mount state, so
// the unload is the library's placement alone: the drive's vtltape@ is
// left down, outside the units.
func TestExecDriveDownLoaded(t *testing.T) {
	e, s := newExecEnv(t)
	s.state["vtltape@12.service"] = "failed"
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[1] = changer.SlotState{Number: 2}
		inv.Drives[1] = changer.DriveState{Number: 2, Full: true, Barcode: "E01002L8", SourceSlot: 2}
	})
	req := compression(e)
	req.Files = append(req.Files, e.edit("library_contents.10", "Slot 2: E01002L8\n", "Slot 2:\n"))
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Error != ErrDriveLoaded || pr.ForceToken == "" || !strings.Contains(pr.Message, "drive slot 2 (E01002L8)") {
		t.Fatalf("plan: %+v", pr)
	}
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied {
		t.Fatalf("forced apply: %+v", resp)
	}
	if len(resp.ForcedUnloads) != 1 || resp.ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 12, Barcode: "E01002L8", SourceSlot: 2}) {
		t.Errorf("forced unloads %+v", resp.ForcedUnloads)
	}
	if got := string(e.read("library_contents.10")); !strings.Contains(got, "\nSlot 2: E01002L8\n") {
		t.Errorf("library_contents.10:\n%s", got)
	}
	for _, u := range append(pr.Units, resp.Units...) {
		if u == "vtltape@12.service" {
			t.Errorf("units %v, %v name vtltape@12", pr.Units, resp.Units)
		}
	}
	if s.has("stop vtltape@12.service") || s.has("start vtltape@12.service") || s.unitState("vtltape@12.service") != "failed" {
		t.Errorf("vtltape@12 touched: %v", s.calls)
	}
}

func TestExecMedia(t *testing.T) {
	newSlot := func(e *env) Request {
		return Request{Op: OpApply, Files: []File{e.edit("library_contents.10", "Slot 24:\n", "Slot 24: E01024L8\n")}, ExpectUnits: []string{"vtllibrary@10.service"}}
	}
	e, s := newExecEnv(t)
	s.onMedia = func() error {
		os.Mkdir(filepath.Join(e.home, "E01024L8"), 0o755)
		return errors.New("exit status 1") // another library's mktape failed
	}
	if resp := e.do(newSlot(e)); resp.Result != ResultApplied {
		t.Fatalf("exit 1, all present: %+v", resp)
	}
	if !s.before("restart mhvtl-media.service", "start vtllibrary@10.service") {
		t.Errorf("media not before the start: %v", s.calls)
	}

	e, s = newExecEnv(t)
	lc := e.read("library_contents.10")
	resp := e.do(newSlot(e))
	if resp.Result != ResultRolledBack || resp.Error != ErrMedia || !strings.Contains(resp.Message, "E01024L8") {
		t.Fatalf("exit 0, missing: %+v", resp)
	}
	if string(e.read("library_contents.10")) != string(lc) {
		t.Error("library_contents.10 not restored")
	}
}

// TestExecMediaRefusals: without make_vtl_media's --per-library-home, new
// barcodes are refused while any library has a home of its own and
// pre-filled slots; with it they are not. A MAP pre-fill is refused by
// validation.
func TestExecMediaRefusals(t *testing.T) {
	e, _ := newExecEnv(t)
	own := filepath.Join(filepath.Dir(e.home), "home30")
	os.MkdirAll(own, 0o755)
	e.replaceFile("device.conf", " Home directory: "+e.home+"\n PERSIST: False\n Backoff: 400\n# fifo: /run/mhvtl/fifo\n\nDrive: 31", " Home directory: "+own+"\n PERSIST: False\n Backoff: 400\n# fifo: /run/mhvtl/fifo\n\nDrive: 31")
	req := Request{Op: OpPlan, Files: []File{e.edit("library_contents.10", "Slot 24:\n", "Slot 24: E01024L8\n")}, ExpectUnits: []string{"vtllibrary@10.service"}}
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrMedia {
		t.Errorf("own home, no marker: %+v", resp)
	}
	script := filepath.Join(e.dir, "make_vtl_media")
	os.WriteFile(script, []byte("#!/bin/sh\n# --per-library-home\n"), 0o755)
	e.cfg.MakeVTLMedia = script
	e.rebuild()
	if resp := e.do(req); resp.Result != ResultValidated {
		t.Errorf("own home, marker: %+v", resp)
	}

	resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("library_contents.10", "MAP 1:\n", "MAP 1: E01099L8\n")}, ExpectUnits: []string{"vtllibrary@10.service"}})
	if resp.Error != ErrInvalid || resp.Findings == nil || resp.Findings.Errors[0].Code != "media.not_in_home" {
		t.Errorf("MAP pre-fill: %+v", resp)
	}
}

// TestExecBay: a bay-monitor cartridge in the MAP of an affected bay
// target is left out of the merge; the bay monitor is stopped before
// guardedStop and started after the libraries are online.
func TestExecBay(t *testing.T) {
	e, s := newExecEnv(t)
	os.WriteFile(filepath.Join(e.dir, baysConf), []byte("# bays\n10 1 /dev/disk/by-path/pci-0000:01:00.0-sas-phy0-lun-0\n"), 0o644)
	os.Symlink("/dev/null", filepath.Join(e.home, "R00001L8"))
	s.state[bayMonitor] = "active"
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.MAPs[0] = changer.SlotState{Number: 1, Full: true, Barcode: "R00001L8"}
	})
	lc := e.read("library_contents.10")
	resp := e.do(compression(e))
	if resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	if string(e.read("library_contents.10")) != string(lc) {
		t.Errorf("the bay cartridge was merged:\n%s", diffLines(string(lc), string(e.read("library_contents.10"))))
	}
	if !s.before("stop "+bayMonitor, "offline 10") || !s.before("online 10", "start "+bayMonitor) {
		t.Errorf("bay monitor order: %v", s.calls)
	}
}

// TestExecDeleteLibrary is AC 12: deleting library 30 stops its units,
// library first, disables those that were enabled, removes its
// library_contents and keeps its home; nothing starts.
func TestExecDeleteLibrary(t *testing.T) {
	e, s := newExecEnv(t)
	for _, u := range []string{"vtllibrary@30.service", "vtltape@31.service"} {
		s.enabled[u] = true
	}
	dc := string(e.read("device.conf"))
	cut := strings.Index(dc, "\nLibrary: 30")
	resp := e.do(Request{Op: OpApply,
		Files:       []File{{Name: "device.conf", BaseSHA256: e.sha("device.conf"), Content: []byte(dc[:cut+1])}},
		Delete:      []Delete{{Name: "library_contents.30", BaseSHA256: e.sha("library_contents.30")}},
		ExpectUnits: []string{"vtllibrary@30.service", "vtltape@31.service", "vtltape@32.service", "vtltape@33.service", "vtltape@34.service"}})
	if resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "library_contents.30")); !os.IsNotExist(err) {
		t.Errorf("library_contents.30: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, "G03001TA")); err != nil {
		t.Errorf("media removed: %v", err)
	}
	if !s.before("stop vtllibrary@30.service", "stop vtltape@31.service") || !s.has("disable vtllibrary@30.service") || !s.has("disable vtltape@34.service") {
		t.Errorf("calls %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "start ") || c == "stop vtllibrary@10.service" {
			t.Errorf("unexpected %s", c)
		}
	}
}

// TestExecZeroLibraries: deleting every library is valid.
func TestExecZeroLibraries(t *testing.T) {
	e, s := newExecEnv(t)
	resp := e.do(Request{Op: OpApply,
		Files: []File{{Name: "device.conf", BaseSHA256: e.sha("device.conf"), Content: []byte("VERSION: 5\n")}},
		Delete: []Delete{{Name: "library_contents.10", BaseSHA256: e.sha("library_contents.10")},
			{Name: "library_contents.30", BaseSHA256: e.sha("library_contents.30")}},
		ExpectUnits: shippedUnits})
	if resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	for _, u := range shippedUnits {
		if s.unitState(u) != "inactive" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
}

// TestExecWarnsMissing: a Slot cartridge missing from its home is warned
// about before the restart (mhvtl-control#43); R2 differing from the
// placement written is reported as advisory drift, not rolled back.
func TestExecWarnsMissing(t *testing.T) {
	e, s := newExecEnv(t)
	os.RemoveAll(filepath.Join(e.home, "E01002L8"))
	s.onStart = func(u string) {
		if u == "vtllibrary@10.service" {
			inv := s.fromDisk(10)
			inv.Slots[2] = changer.SlotState{Number: 3}
			s.live[10] = inv
		}
	}
	resp := e.do(compression(e))
	if resp.Result != ResultApplied {
		t.Fatalf("apply: %+v", resp)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "slot 2: E01002L8") {
		t.Errorf("warnings %v", resp.Warnings)
	}
	if len(resp.PlacementDrift) != 1 || !strings.Contains(resp.PlacementDrift[0], "slot 3") {
		t.Errorf("drift %v", resp.PlacementDrift)
	}
}

// TestExecLivePlacement is AC 13: a cartridge a host exported to the MAP
// at run time stays there when the client builds next from the live view.
func TestExecLivePlacement(t *testing.T) {
	e, s := newExecEnv(t)
	s.liveWith(10, func(inv *changer.Inventory) {
		inv.Slots[0] = changer.SlotState{Number: 1}
		inv.MAPs[0] = changer.SlotState{Number: 1, Full: true, Barcode: "E01001L8"}
	})
	s.onListMap = func(int) string { return "E01001L8" }
	req := compression(e)
	lc := e.edit("library_contents.10", "MAP 1:\n", "MAP 1: E01001L8\n")
	lc.Content = []byte(strings.Replace(string(lc.Content), "Slot 1: E01001L8\n", "Slot 1:\n", 1))
	req.Files = append(req.Files, lc)
	resp := e.do(req)
	if resp.Result != ResultApplied || len(resp.PlacementDrift) != 0 {
		t.Fatalf("apply: %+v", resp)
	}
	if got := string(e.read("library_contents.10")); !strings.Contains(got, "\nMAP 1: E01001L8\n") || !strings.Contains(got, "\nSlot 1:\n") {
		t.Errorf("library_contents.10:\n%s", got)
	}
}

// TestExecStopFailure: a stop that fails after another library was
// stopped rolls back: the stopped library comes back, the other one,
// still running, is put back online.
func TestExecStopFailure(t *testing.T) {
	e, s := newExecEnv(t)
	conf := e.read("mhvtl.conf")
	s.failStop["vtllibrary@30.service"] = true
	resp := e.do(Request{Op: OpApply, Files: []File{e.edit("mhvtl.conf", "VERBOSE=1", "VERBOSE=2")}, ExpectUnits: shippedUnits})
	if resp.Result != ResultRolledBack {
		t.Fatalf("apply: %+v", resp)
	}
	if string(e.read("mhvtl.conf")) != string(conf) || e.intentExists() {
		t.Error("mhvtl.conf not restored, or intent left")
	}
	if !s.before("stop vtllibrary@30.service", "online 30") || !s.before("stop vtllibrary@30.service", "start vtllibrary@10.service") {
		t.Errorf("calls %v", s.calls)
	}
	for _, u := range shippedUnits {
		if s.unitState(u) != "active" {
			t.Errorf("%s is %s", u, s.unitState(u))
		}
	}
}

// TestExecInactiveLibraryLoaded: a library that is not running, whose
// drive's vtltape@ runs with a cartridge, is drive_loaded; forced, the
// drive's vtltape@ stops and starts with the library, and its cartridge,
// whose barcode no READ ELEMENT STATUS showed, is reported unknown.
func TestExecInactiveLibraryLoaded(t *testing.T) {
	e, s := newExecEnv(t)
	s.state["vtllibrary@10.service"] = "inactive"
	s.loaded["XYZZY_A2"] = true
	req := compression(e)
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Error != ErrDriveLoaded || pr.ForceToken == "" || !strings.Contains(pr.Message, "drive slot 2") {
		t.Fatalf("plan: %+v", pr)
	}
	req.ForceToken, req.ExpectUnits = pr.ForceToken, pr.Units
	resp := e.do(req)
	if resp.Result != ResultApplied {
		t.Fatalf("forced apply: %+v", resp)
	}
	if len(resp.ForcedUnloads) != 1 || resp.ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 12, Unknown: true}) {
		t.Errorf("forced unloads %+v", resp.ForcedUnloads)
	}
	// Its cartridge has no slot: reported unplaced, in the audit too.
	if fmt.Sprint(resp.Unplaced) != "["+unknownCartridge(12)+"]" {
		t.Errorf("unplaced %v", resp.Unplaced)
	}
	lines := e.auditLines()
	var r AuditRecord
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &r) != nil || fmt.Sprint(r.Unplaced) != fmt.Sprint(resp.Unplaced) {
		t.Errorf("audit %v", lines)
	}
	if !s.before("stop vtltape@12.service", "start vtltape@12.service") {
		t.Errorf("drive 12 not restarted: %v", s.calls)
	}
}

// TestExecRollbackUnknownUnload: the rollback finds a drive of the failed
// library loaded with a cartridge no READ ELEMENT STATUS showed; it is
// stopped and reported unplaced rather than dropped.
func TestExecRollbackUnknownUnload(t *testing.T) {
	e, s := newExecEnv(t)
	s.failStart["vtllibrary@10.service"] = true
	s.onStart = func(u string) {
		if u == "vtltape@11.service" {
			s.loaded["XYZZY_A2"] = true
		}
	}
	resp := e.do(compression(e))
	if resp.Result != ResultRolledBack {
		t.Fatalf("apply: %+v", resp)
	}
	found := false
	for _, u := range resp.Unplaced {
		found = found || strings.Contains(u, "drive 12")
	}
	if !found {
		t.Errorf("unplaced %v", resp.Unplaced)
	}
	if s.index("stop vtltape@12.service", 0) < 0 {
		t.Errorf("drive 12 not stopped: %v", s.calls)
	}
}

// TestWithForcedDown: a forced drive whose vtltape@ is down is not
// stopped or started for the unload, unless the edit restarts it anyway.
func TestWithForcedDown(t *testing.T) {
	lib := config.Library{ID: 10, Drives: []config.Drive{{ID: 11, LibraryID: 10, Slot: 1}, {ID: 12, LibraryID: 10, Slot: 2}}}
	inv := config.Inventory{Libraries: []config.Library{lib}}
	c := &checked{cur: inv, next: inv, down: map[int]bool{11: true, 12: true}}
	forced := map[int][]changer.DriveState{10: {{Number: 1, Full: true}, {Number: 2, Full: true}}}
	p := withForced(UnitPlan{Restart: []int{10}, StopDrives: map[int][]int{10: {11}}, StartDrives: map[int][]int{10: {11}}}, c, forced)
	if fmt.Sprint(p.StopDrives[10], p.StartDrives[10]) != "[11] [11]" {
		t.Errorf("stop %v, start %v", p.StopDrives, p.StartDrives)
	}
}

// TestWithForcedStandalone: a forced drive whose library the change
// deletes, its stanza kept, starts again standalone.
func TestWithForcedStandalone(t *testing.T) {
	cur := config.Inventory{Libraries: []config.Library{{ID: 30, Drives: []config.Drive{{ID: 31, LibraryID: 30, Slot: 1}}}}}
	next := config.Inventory{Standalone: []config.Drive{{ID: 31, LibraryID: 30}}}
	p := withForced(UnitPlan{Stop: []int{30}, StopDrives: map[int][]int{}, StartDrives: map[int][]int{}}, &checked{cur: cur, next: next},
		map[int][]changer.DriveState{30: {{Number: 1, Full: true, Barcode: "G03001L8"}}})
	if fmt.Sprint(p.StopDrives[30], p.StartStandalone) != "[31] [31]" {
		t.Errorf("stop %v, start standalone %v", p.StopDrives, p.StartStandalone)
	}
}

// TestExecRollbackCrashedDrive: drive 11 dies after library 10 started,
// with a cartridge mounted; the rollback does not ask it, takes its
// cartridge from READ ELEMENT STATUS and puts it back in its slot.
func TestExecRollbackCrashedDrive(t *testing.T) {
	e, s := newExecEnv(t)
	lc := e.read("library_contents.10")
	once := false
	s.onStart = func(u string) {
		if u != "vtllibrary@10.service" || once {
			return
		}
		once = true
		s.state["vtltape@11.service"] = "failed"
		inv := s.fromDisk(10)
		inv.Slots[0] = changer.SlotState{Number: 1}
		inv.Drives[0] = changer.DriveState{Number: 1, Full: true, Barcode: "E01001L8", SourceSlot: 1}
		s.live[10] = inv
	}
	resp := e.do(compression(e))
	if resp.Result != ResultRolledBack || resp.FailedUnit != "vtltape@11.service" {
		t.Fatalf("apply: %+v", resp)
	}
	if len(resp.ForcedUnloads) != 1 || resp.ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 11, Barcode: "E01001L8", SourceSlot: 1}) {
		t.Errorf("forced unloads %+v", resp.ForcedUnloads)
	}
	if got := string(e.read("library_contents.10")); got != string(lc) {
		t.Errorf("library_contents.10:\n%s", diffLines(string(lc), got))
	}
}

// TestExecMediaWarnsMissing: with new barcodes mhvtl-media.service creates
// every missing Slot barcode of every library, so the missing-cartridge
// warning covers library 30 too, which does not restart; it is computed
// before media runs, so it still names cartridges media then created blank.
func TestExecMediaWarnsMissing(t *testing.T) {
	e, s := newExecEnv(t)
	os.RemoveAll(filepath.Join(e.home, "E01002L8"))
	os.RemoveAll(filepath.Join(e.home, "G03001TA"))
	s.onMedia = func() error {
		for _, b := range []string{"E01024L8", "E01002L8", "G03001TA"} {
			os.Mkdir(filepath.Join(e.home, b), 0o755)
		}
		return nil
	}
	req := Request{Op: OpApply, Files: []File{e.edit("library_contents.10", "Slot 24:\n", "Slot 24: E01024L8\n")}, ExpectUnits: []string{"vtllibrary@10.service"}}
	check := func(what string, w []string) {
		t.Helper()
		all := strings.Join(w, "\n")
		if len(w) != 2 || !strings.Contains(all, "library 10 slot 2: E01002L8") || !strings.Contains(all, "library 30 slot 1: G03001TA") || strings.Contains(all, "E01024L8") {
			t.Errorf("%s: warnings %q", what, w)
		}
	}
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Result != ResultValidated {
		t.Fatalf("plan: %+v", pr)
	}
	check("plan", pr.Warnings)
	resp := e.do(req)
	if resp.Result != ResultApplied || s.has("restart mhvtl-media.service") == false {
		t.Fatalf("apply: %+v", resp)
	}
	check("apply", resp.Warnings)
}

// TestExecRollbackNewMedia: a rollback after media created a new
// pre-filled cartridge restores the original layout without it, whether
// library 10 never came up (the apply's source) or ran (its own R1′ lists
// the cartridge), and reports its media as orphaned, in the response and
// the audit alike.
func TestExecRollbackNewMedia(t *testing.T) {
	for _, fail := range []string{"library start", "drive dies after the library started"} {
		t.Run(fail, func(t *testing.T) {
			e, s := newExecEnv(t)
			lc := e.read("library_contents.10")
			s.onMedia = func() error { return os.Mkdir(filepath.Join(e.home, "E01024L8"), 0o755) }
			if fail == "library start" {
				s.failStart["vtllibrary@10.service"] = true
			} else {
				once := false
				s.onStart = func(u string) {
					if u == "vtllibrary@10.service" && !once {
						once = true
						s.state["vtltape@11.service"] = "failed"
					}
				}
			}
			req := compression(e)
			req.Files = append(req.Files, e.edit("library_contents.10", "Slot 24:\n", "Slot 24: E01024L8\n"))
			resp := e.do(req)
			if resp.Result != ResultRolledBack {
				t.Fatalf("apply: %+v", resp)
			}
			if got := string(e.read("library_contents.10")); got != string(lc) {
				t.Errorf("library_contents.10:\n%s", diffLines(string(lc), got))
			}
			want := filepath.Join(e.home, "E01024L8")
			if fmt.Sprint(resp.OrphanedMedia) != "["+want+"]" || len(resp.Unplaced) != 0 {
				t.Errorf("orphaned %v, unplaced %v", resp.OrphanedMedia, resp.Unplaced)
			}
			if _, err := os.Stat(want); err != nil {
				t.Errorf("media not kept: %v", err)
			}
			lines := e.auditLines()
			var r AuditRecord
			if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &r) != nil || fmt.Sprint(r.OrphanedMedia) != fmt.Sprint(resp.OrphanedMedia) {
				t.Errorf("audit %v", lines)
			}
		})
	}
}

// TestExecRollbackNeverCameUp: the request moves E01001L8 from slot 1 to
// slot 25 and library 10 fails to start. Its file then is the new
// layout's, so the rollback takes the apply's source: E01001L8 goes back
// to slot 1, not to slot 25 of the original layout.
func TestExecRollbackNeverCameUp(t *testing.T) {
	e, s := newExecEnv(t)
	lc := e.read("library_contents.10")
	s.failStart["vtllibrary@10.service"] = true
	f := e.edit("library_contents.10", "Slot 25:\n", "Slot 25: E01001L8\n")
	f.Content = []byte(strings.Replace(string(f.Content), "Slot 1: E01001L8\n", "Slot 1:\n", 1))
	resp := e.do(Request{Op: OpApply, Actor: "admin", Files: []File{f}, ExpectUnits: []string{"vtllibrary@10.service"}})
	if resp.Result != ResultRolledBack {
		t.Fatalf("apply: %+v", resp)
	}
	if got := string(e.read("library_contents.10")); got != string(lc) {
		t.Errorf("library_contents.10:\n%s", diffLines(string(lc), got))
	}
}

// TestForceLoadedSince: a drive loaded after the plan voids its force
// token, though the change and the live placement are the same: here a
// drive of a library that is not running, found only by TEST UNIT READY.
func TestForceLoadedSince(t *testing.T) {
	e, s := newExecEnv(t)
	s.state["vtllibrary@10.service"] = "inactive"
	s.loaded["XYZZY_A2"] = true
	req := compression(e)
	plan := req
	plan.Op = OpPlan
	pr := e.do(plan)
	if pr.Error != ErrDriveLoaded || pr.ForceToken == "" {
		t.Fatalf("plan: %+v", pr)
	}
	s.loaded["XYZZY_A1"] = true
	req.ForceToken = pr.ForceToken
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrForce {
		t.Errorf("apply: %+v", resp)
	}
	if s.has("stop vtllibrary@10.service") || s.has("stop vtltape@11.service") {
		t.Errorf("stopped: %v", s.calls)
	}
}

// TestExecRollbackHeld: the start fails with libraries 10 and 30 both
// held offline. Nothing puts them online before the rollback, whose
// guardedStop takes them over; when it fails at library 10, library 30,
// which it never reached, is put back online.
func TestExecRollbackHeld(t *testing.T) {
	e, s := newExecEnv(t)
	dc := string(e.read("device.conf"))
	i := strings.Index(dc, "Drive: 31 ")
	next := strings.Replace(dc[:i], " Compression type: lzo", " Compression type: zlib", 1) +
		strings.Replace(dc[i:], " Compression type: lzo", " Compression type: zlib", 1)
	req := Request{Op: OpApply, Actor: "admin", Files: []File{{Name: "device.conf", BaseSHA256: e.sha("device.conf"), Content: []byte(next)}},
		ExpectUnits: []string{"vtllibrary@10.service", "vtllibrary@30.service", "vtltape@11.service", "vtltape@31.service"}}
	s.onStart = func(u string) {
		if u == "vtllibrary@30.service" {
			s.state["vtltape@31.service"] = "failed" // fails the start
		}
	}
	// As the rollback begins, library 10 stops answering the queue, which
	// fails the rollback's guardedStop at it.
	e.cfg.JournalTail = func(context.Context, string) ([]byte, error) {
		s.deaf[10] = true
		return nil, nil
	}
	e.rebuild()
	resp := e.do(req)
	if resp.Result != ResultError {
		t.Fatalf("apply: %+v", resp)
	}
	first := s.index("offline 30", s.index("start vtllibrary@30.service", 0))
	if first < 0 {
		t.Fatalf("library 30 not held offline: %v", s.calls)
	}
	// After the hold, the rollback's online of 10 comes first, then 30's.
	onl10, onl30 := s.index("online 10", first), s.index("online 30", first)
	if onl10 < 0 || onl30 < onl10 || s.offline[30] {
		t.Errorf("held libraries: %v (offline %v)", s.calls[first:], s.offline)
	}
	if strings.Count(strings.Join(s.calls[first:], ","), "online 30") != 1 {
		t.Errorf("library 30 put online more than once: %v", s.calls[first:])
	}
}

// TestExecBayCartridgeInSlot: a bay cartridge (a symlink in the home) a
// host moved to a storage slot is no new barcode: written to a Slot line
// it applies without restarting mhvtl-media.service. With a real new
// barcode in the same apply, make_vtl_media would run mktape on it, so the
// apply is refused.
func TestExecBayCartridgeInSlot(t *testing.T) {
	e, s := newExecEnv(t)
	if err := os.Symlink("/dev/null", filepath.Join(e.home, "R00001L8")); err != nil {
		t.Fatal(err)
	}
	req := Request{Op: OpApply, Files: []File{e.edit("library_contents.10", "Slot 24:\n", "Slot 24: R00001L8\n")}, ExpectUnits: []string{"vtllibrary@10.service"}}
	if resp := e.do(req); resp.Result != ResultApplied || s.has("restart mhvtl-media.service") {
		t.Fatalf("bay cartridge: %+v", resp)
	}

	e, s = newExecEnv(t)
	os.Symlink("/dev/null", filepath.Join(e.home, "R00001L8"))
	f := e.edit("library_contents.10", "Slot 24:\n", "Slot 24: R00001L8\n")
	f.Content = []byte(strings.Replace(string(f.Content), "Slot 25:\n", "Slot 25: E01025L8\n", 1))
	req = Request{Op: OpApply, Files: []File{f}, ExpectUnits: []string{"vtllibrary@10.service"}}
	if resp := e.do(req); resp.Result != ResultRefused || resp.Error != ErrMedia || !strings.Contains(resp.Message, "R00001L8") {
		t.Errorf("with a new barcode: %+v", resp)
	}
	if s.has("restart mhvtl-media.service") || s.has("stop vtllibrary@10.service") {
		t.Errorf("ran: %v", s.calls)
	}
}
