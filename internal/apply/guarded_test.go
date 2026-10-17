package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// stopEnv is a fake system for guardedStop: two libraries, 10 (drives 11,
// 12) and 30 (drive 31), a message queue, changers and drives, and
// systemctl.
type stopEnv struct {
	t      *testing.T
	dir    string
	calls  []string
	maps   map[int][]string // successive "list map" replies
	state  map[string]string
	result map[string]string
	inv    map[int]changer.Inventory
	loaded map[string]bool // by drive serial
	onStop func(unit string)
	failOn string // systemctl stop of this unit fails, after onStop
	onRES  func()
	// seq are successive ActiveState replies of a unit, before state.
	seq map[string][]string
}

func newStopEnv(t *testing.T) *stopEnv {
	return &stopEnv{
		t: t, dir: t.TempDir(),
		maps:   map[int][]string{},
		state:  map[string]string{},
		result: map[string]string{},
		inv: map[int]changer.Inventory{
			10: {Drives: []changer.DriveState{{Number: 1}, {Number: 2}}, Slots: []changer.SlotState{{Number: 1, Full: true, Barcode: "E01001L8"}, {Number: 2}}, MAPs: []changer.SlotState{{Number: 1}}},
			30: {Drives: []changer.DriveState{{Number: 1}}, Slots: []changer.SlotState{{Number: 1, Full: true, Barcode: "G03001L8"}}},
		},
		loaded: map[string]bool{},
		onStop: func(string) {},
	}
}

func (e *stopEnv) lib(id int) StopLib {
	switch id {
	case 10:
		return StopLib{Lib: config.Library{ID: 10, Serial: "LIB10",
			Drives: []config.Drive{{ID: 11, Slot: 1, Serial: "DRV11"}, {ID: 12, Slot: 2, Serial: "DRV12"}},
			Slots:  []config.Slot{{Number: 1, Barcode: "E01001L8"}, {Number: 2, Barcode: "E01002L8"}}}}
	case 30:
		return StopLib{Lib: config.Library{ID: 30, Serial: "LIB30",
			Drives: []config.Drive{{ID: 31, Slot: 1, Serial: "DRV31"}},
			Slots:  []config.Slot{{Number: 1, Barcode: "G03001L8"}}}}
	}
	e.t.Fatalf("no library %d", id)
	return StopLib{}
}

func (e *stopEnv) Offline(lib int64) error {
	e.calls = append(e.calls, fmt.Sprint("offline ", lib))
	return nil
}
func (e *stopEnv) Online(lib int64) error {
	e.calls = append(e.calls, fmt.Sprint("online ", lib))
	return nil
}
func (e *stopEnv) ListMap(lib int64) (string, error) {
	e.calls = append(e.calls, fmt.Sprint("list map ", lib))
	r := e.maps[int(lib)]
	if len(r) == 0 {
		return "", nil
	}
	e.maps[int(lib)] = r[1:]
	return r[0], nil
}

func (e *stopEnv) Locate(serial string, typ uint8) (string, error) {
	return "hctl-" + serial, nil
}

func (e *stopEnv) Inventory(lib config.Library, hctl string) (changer.Inventory, error) {
	e.calls = append(e.calls, fmt.Sprint("RES ", lib.ID))
	if e.onRES != nil {
		e.onRES()
	}
	return e.inv[lib.ID], nil
}

func (e *stopEnv) Loaded(hctl string) (bool, error) {
	e.calls = append(e.calls, "TUR "+strings.TrimPrefix(hctl, "hctl-"))
	return e.loaded[strings.TrimPrefix(hctl, "hctl-")], nil
}

func (e *stopEnv) systemctl(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] {
	case "stop":
		e.calls = append(e.calls, "stop "+args[1])
		e.onStop(args[1])
		if args[1] == e.failOn {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	case "show":
		unit := args[3]
		switch args[1] {
		case "--property=ActiveState":
			if q := e.seq[unit]; len(q) > 0 {
				e.seq[unit] = q[1:]
				return []byte(q[0] + "\n"), nil
			}
			if s, ok := e.state[unit]; ok {
				return []byte(s + "\n"), nil
			}
			return []byte("active\n"), nil
		case "--property=Result":
			if r, ok := e.result[unit]; ok {
				return []byte(r + "\n"), nil
			}
			return []byte("success\n"), nil
		}
	}
	e.t.Fatalf("systemctl %v", args)
	return nil, nil
}

func (e *stopEnv) stopper(mode StopMode) *stopper {
	cfg := &Config{Queue: e, Devices: e, Systemctl: e.systemctl, Logf: e.t.Logf}
	aside := filepath.Join(e.dir, "aside")
	os.MkdirAll(aside, 0o700)
	return &stopper{cfg: cfg, configDir: e.dir, aside: aside, mode: mode, journal: func(*LibStop) error { return nil }}
}

func (e *stopEnv) write(name, data string, mtime time.Time) {
	e.t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		e.t.Fatal(err)
	}
}

// onlines lists the libraries put back online, in order.
func (e *stopEnv) onlines() string {
	var out []string
	for _, c := range e.calls {
		if strings.HasPrefix(c, "online ") {
			out = append(out, strings.TrimPrefix(c, "online "))
		}
	}
	return strings.Join(out, ",")
}

func TestGuardedInactiveFileRule(t *testing.T) {
	old, newer := time.Unix(1_700_000_000, 0), time.Unix(1_700_000_100, 0)
	for _, tc := range []struct {
		name    string
		persist bool
		lcTime  time.Time
		pTime   *time.Time
		want    string // slot 1 barcode
		kind    string
	}{
		{"PERSIST off", false, old, &newer, "E01001L8", SourceFile},
		{"PERSIST on, no .persist", true, old, nil, "E01001L8", SourceFile},
		{"PERSIST on, .persist newer", true, old, &newer, "P00001L8", SourcePersist},
		{"PERSIST on, same second", true, old, &old, "P00001L8", SourcePersist},
		{"PERSIST on, library_contents newer", true, newer, &old, "E01001L8", SourceFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []StopMode{ModeApply, ModeRollback, ModeRecover} {
				e := newStopEnv(t)
				e.state["vtllibrary@10.service"] = "failed"
				e.state["vtltape@11.service"], e.state["vtltape@12.service"] = "inactive", "inactive"
				e.write("library_contents.10", "VERSION: 2\nSlot 1: E01001L8\n", tc.lcTime)
				if tc.pTime != nil {
					e.write("library_contents.10.persist", "\nSlot 1: P00001L8   \nSlot 2: \n", *tc.pTime)
				}
				sl := e.lib(10)
				sl.Persist = tc.persist
				out, err := e.stopper(mode).guardedStop(context.Background(), []StopLib{sl})
				if err != nil {
					t.Fatal(err)
				}
				ls := out[0]
				if ls.Active || ls.R1 != nil || ls.Offline || ls.Source.Slots[1] != tc.want || ls.SourceKind != tc.kind || ls.Trusted {
					t.Errorf("%s: %+v", mode, ls)
				}
				for _, c := range e.calls {
					if strings.HasPrefix(c, "RES") || strings.HasPrefix(c, "TUR") || strings.HasPrefix(c, "offline") || strings.HasPrefix(c, "list map") {
						t.Errorf("%s: inactive library got %q", mode, c)
					}
				}
				if _, err := os.Stat(filepath.Join(e.dir, "library_contents.10.persist")); err == nil {
					t.Errorf("%s: .persist left in place", mode)
				}
			}
		})
	}
}

func TestGuardedVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   bool   // a .persist exists before the stop
		write    string // what the stop writes to .persist, "" for nothing
		result   string
		fresh    bool
		trusted  bool
		wantKind string
	}{
		{"no .persist, PERSIST off", false, "", "success", false, false, SourceLive},
		{"an old .persist the stop leaves", true, "", "success", false, false, SourceLive},
		{"a fresh .persist, Result=success", true, "\nSlot 1: P00001L8\nSlot 2: P00002L8 \n", "success", true, true, SourcePersist},
		{"a truncated .persist, Result=timeout", true, "\nSlot 1: P00", "timeout", true, false, SourceLive},
		{"a fresh .persist, Result=signal", false, "\nSlot 1: P00001L8\n", "signal", true, false, SourceLive},
		{"a fresh .persist, Result=success, unreadable", true, "\nSlot 1: P00001L8\nSlot 1: P00002L8\n", "success", true, false, SourceLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newStopEnv(t)
			p := filepath.Join(e.dir, "library_contents.10.persist")
			if tc.before {
				e.write("library_contents.10.persist", "\nSlot 1: OLD001L8\n", time.Unix(1_700_000_000, 0))
			}
			e.onStop = func(unit string) {
				if unit == "vtllibrary@10.service" && tc.write != "" {
					os.WriteFile(p, []byte(tc.write), 0o644)
				}
			}
			e.result["vtllibrary@10.service"] = tc.result
			sp := e.stopper(ModeApply)
			journaled := false
			sp.journal = func(ls *LibStop) error {
				// The verdict is journaled while .persist is still in place.
				_, err := os.Stat(p)
				journaled = err == nil || !ls.After.Exists
				return nil
			}
			out, err := sp.guardedStop(context.Background(), []StopLib{e.lib(10)})
			if err != nil {
				t.Fatal(err)
			}
			ls := out[0]
			if ls.Fresh != tc.fresh || ls.Trusted != tc.trusted || ls.SourceKind != tc.wantKind || !journaled {
				t.Errorf("fresh %v trusted %v source %s journaled %v", ls.Fresh, ls.Trusted, ls.SourceKind, journaled)
			}
			// A fresh .persist not trusted says why.
			if (ls.Fresh && !ls.Trusted) != (ls.PersistReason != "") {
				t.Errorf("persist reason %q", ls.PersistReason)
			}
			switch tc.wantKind {
			case SourcePersist:
				if ls.Source.Slots[2] != "P00002L8" || len(ls.PersistData) == 0 {
					t.Errorf("source %+v", ls.Source)
				}
			case SourceLive:
				if ls.Source.Slots[1] != "E01001L8" || ls.Source.Slots[2] != "" {
					t.Errorf("source %+v, want R1′", ls.Source)
				}
			}
			// Any .persist is moved aside, trusted or not.
			if _, err := os.Stat(p); err == nil {
				t.Error(".persist left in place")
			}
			if ls.After.Exists && ls.Aside == "" {
				t.Error("no aside path")
			}
			if want := "RES 10,list map 10,offline 10,list map 10,TUR DRV11,TUR DRV12,stop vtllibrary@10.service"; strings.Join(e.calls, ",") != want {
				t.Errorf("calls %q\nwant  %q", strings.Join(e.calls, ","), want)
			}
		})
	}
}

// TestGuardedStopOrder: the library goes down before its drives
// (amendment 1), and steps 1-5 run for every library before any stop.
func TestGuardedStopOrder(t *testing.T) {
	e := newStopEnv(t)
	l10, l30 := e.lib(10), e.lib(30)
	l10.Drives, l30.Drives = []int{12, 11}, []int{31}
	if _, err := e.stopper(ModeApply).guardedStop(context.Background(), []StopLib{l10, l30}); err != nil {
		t.Fatal(err)
	}
	var stops []string
	firstStop := -1
	for i, c := range e.calls {
		if strings.HasPrefix(c, "stop ") {
			stops = append(stops, short([]string{strings.TrimPrefix(c, "stop ")}))
			if firstStop < 0 {
				firstStop = i
			}
		} else if firstStop >= 0 {
			t.Errorf("%q after the first stop", c)
		}
	}
	if strings.Join(stops, " ") != "L10 T11 T12 L30 T31" {
		t.Errorf("stops %v", stops)
	}
	if e.onlines() != "" {
		t.Errorf("stopped libraries put online: %s", e.onlines())
	}
}

func TestGuardedLoadedDrive(t *testing.T) {
	// R1′ shows drive 1 empty, TUR finds a cartridge.
	for _, mode := range []StopMode{ModeApply, ModeRollback, ModeRecover} {
		e := newStopEnv(t)
		e.loaded["DRV11"] = true
		out, err := e.stopper(mode).guardedStop(context.Background(), []StopLib{e.lib(10)})
		switch mode {
		case ModeApply:
			if !errors.Is(err, errRaced) {
				t.Errorf("apply: %v", err)
			}
			if e.onlines() != "10" {
				t.Errorf("apply: online %q", e.onlines())
			}
		default:
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			// Its barcode is not known: flagged, so the merge reports it.
			if len(out[0].ForcedUnloads) != 1 || out[0].ForcedUnloads[0] != (ForcedUnload{Library: 10, Drive: 11, Unknown: true}) ||
				strings.Join(out[0].Stopped, " ") != "vtllibrary@10.service vtltape@11.service" {
				t.Errorf("%s: %+v", mode, out[0])
			}
		}
	}

	// R1′ shows drive 1 loaded from slot 2: consistent in apply mode; a
	// forced unload back to slot 2 in rollback mode.
	for _, mode := range []StopMode{ModeApply, ModeRollback} {
		e := newStopEnv(t)
		e.loaded["DRV11"] = true
		inv := e.inv[10]
		inv.Drives[0] = changer.DriveState{Number: 1, Full: true, Barcode: "E01002L8", SourceSlot: 2}
		e.inv[10] = inv
		out, err := e.stopper(mode).guardedStop(context.Background(), []StopLib{e.lib(10)})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		fu, ld := out[0].ForcedUnloads, out[0].LoadedDrives
		want := ForcedUnload{Library: 10, Drive: 11, Barcode: "E01002L8", SourceSlot: 2}
		if mode == ModeApply && (len(fu) != 0 || len(ld) != 1 || ld[0] != want) || mode == ModeRollback && (len(fu) != 1 || fu[0] != want || len(ld) != 0) {
			t.Errorf("%s: forced unloads %+v, loaded drives %+v", mode, fu, ld)
		}
	}
}

// TestGuardedReonline: every exit path after an offline puts the
// libraries taken offline and not stopped back online.
func TestGuardedReonline(t *testing.T) {
	ctx := context.Background()

	// The MAP barrier disagrees on library 30 after 10 was offlined.
	e := newStopEnv(t)
	e.maps[30] = []string{"", "M00001L8"}
	if _, err := e.stopper(ModeApply).guardedStop(ctx, []StopLib{e.lib(10), e.lib(30)}); !errors.Is(err, errRaced) {
		t.Errorf("barrier: %v", err)
	}
	if e.onlines() != "10,30" {
		t.Errorf("barrier: online %q", e.onlines())
	}

	// The stop of library 30 fails: 10 is down, 30 still up and offline.
	e = newStopEnv(t)
	e.failOn = "vtllibrary@30.service"
	if _, err := e.stopper(ModeRollback).guardedStop(ctx, []StopLib{e.lib(10), e.lib(30)}); err == nil {
		t.Error("stop failure: no error")
	}
	// Rollback puts each library online before its R1′ (10, 30), then
	// 30 again after the failure.
	if e.onlines() != "10,30,30" {
		t.Errorf("stop failure: online %q", e.onlines())
	}

	// A panic in the journal of library 30, stopped first: 10 is offline
	// and still running.
	e = newStopEnv(t)
	sp := e.stopper(ModeRecover)
	sp.journal = func(*LibStop) error { panic("journal") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		sp.guardedStop(ctx, []StopLib{e.lib(30), e.lib(10)})
	}()
	if e.onlines() != "30,10,10" {
		t.Errorf("panic: online %q", e.onlines())
	}

	// A journal error after the first library stopped.
	e = newStopEnv(t)
	sp = e.stopper(ModeApply)
	n := 0
	sp.journal = func(*LibStop) error {
		if n++; n == 1 {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := sp.guardedStop(ctx, []StopLib{e.lib(10), e.lib(30)}); err == nil {
		t.Error("journal failure: no error")
	}
	if e.onlines() != "30" {
		t.Errorf("journal failure: online %q", e.onlines())
	}
}

// TestGuardedExpect: in apply mode R1′ must equal the placement the change
// was validated against; a cartridge moved since is raced before the
// library is taken offline or anything stopped.
func TestGuardedExpect(t *testing.T) {
	e := newStopEnv(t)
	st := e.stopper(ModeApply)
	st.expect = &validate.Live{Libraries: map[int]validate.Placement{10: {Slots: map[int]string{2: "E01001L8"}}}}
	if _, err := st.guardedStop(context.Background(), []StopLib{e.lib(10)}); !errors.Is(err, errRaced) {
		t.Fatalf("err %v", err)
	}
	if got := strings.Join(e.calls, ","); got != "RES 10" {
		t.Errorf("calls %s", got)
	}
	st = e.stopper(ModeApply)
	st.expect = &validate.Live{Libraries: map[int]validate.Placement{10: placementOf(e.inv[10])}}
	if _, err := st.guardedStop(context.Background(), []StopLib{e.lib(10)}); err != nil {
		t.Errorf("same placement: %v", err)
	}
}

func TestPersistPlacement(t *testing.T) {
	// As save_config writes it: blank lines between element types, no
	// VERSION line, barcodes padded, Drive lines empty.
	p, err := persistPlacement([]byte("\nDrive 1:\nDrive 2:\n\nPicker 1: \n\nMAP 1: M00001L8  \n\nSlot 1: E01001L8    \nSlot 2: \nSlot 3: E01003L8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.Slots) != "map[1:E01001L8 3:E01003L8]" || fmt.Sprint(p.MAPs) != "map[1:M00001L8]" || len(p.Drives) != 0 || len(p.Pickers) != 0 {
		t.Errorf("%+v", p)
	}
}

// TestGuardedPrepared: once every library is offline and before the first
// stop, the prepared hook journals R1′, the offline set and the MAP. A
// helper killed in the hook (the kill point) has stopped nothing, and what
// it journaled names every library to put back online.
func TestGuardedPrepared(t *testing.T) {
	e := newStopEnv(t)
	e.maps[10] = []string{"M00001L8", "M00001L8"}
	sp := e.stopper(ModeApply)
	killed := errors.New("SIGKILL")
	var journaled []LibStop
	sp.prepared = func(out []*LibStop) error {
		for _, c := range e.calls {
			if strings.HasPrefix(c, "stop ") {
				t.Errorf("%q before the prepared journal", c)
			}
		}
		for _, ls := range out {
			journaled = append(journaled, *ls)
		}
		return killed
	}
	if _, err := sp.guardedStop(context.Background(), []StopLib{e.lib(10), e.lib(30)}); !errors.Is(err, killed) {
		t.Fatalf("err %v", err)
	}
	if len(journaled) != 2 {
		t.Fatalf("journaled %+v", journaled)
	}
	for _, ls := range journaled {
		if !ls.Offline || ls.R1 == nil || len(ls.DriveHCTL) == 0 || len(ls.Stopped) != 0 {
			t.Errorf("library %d: %+v", ls.ID, ls)
		}
	}
	if journaled[0].Map != "M00001L8" {
		t.Errorf("map %q", journaled[0].Map)
	}
}

// TestGuardedStopError: a unit whose stop fails but that is no longer
// active counts as stopped; one still active does not.
func TestGuardedStopError(t *testing.T) {
	e := newStopEnv(t)
	e.failOn = "vtltape@11.service"
	e.state["vtltape@11.service"] = "failed"
	l10 := e.lib(10)
	l10.Drives = []int{11, 12}
	out, err := e.stopper(ModeRollback).guardedStop(context.Background(), []StopLib{l10})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out[0].Stopped, " "); got != "vtllibrary@10.service vtltape@11.service vtltape@12.service" {
		t.Errorf("stopped %s", got)
	}

	// A cancel during the stop: the unit went down, the stop reports the
	// cancel, and the unit is recorded as stopped for the rollback.
	e = newStopEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.failOn = "vtllibrary@10.service"
	e.state["vtllibrary@10.service"] = "inactive"
	sp := e.stopper(ModeApply)
	orig := sp.cfg.Systemctl
	sp.cfg.Systemctl = func(c context.Context, args ...string) ([]byte, error) {
		if args[0] == "stop" {
			cancel()
		}
		return orig(c, args...)
	}
	out, err = sp.guardedStop(ctx, []StopLib{e.lib(10)})
	if err == nil {
		t.Error("cancel: no error")
	}
	if got := strings.Join(out[0].Stopped, " "); got != "vtllibrary@10.service" {
		t.Errorf("cancel: stopped %q", got)
	}
	if e.onlines() != "" {
		t.Errorf("cancel: a stopped library put online: %s", e.onlines())
	}
}

// TestGuardedCancel: a cancel between READ ELEMENT STATUS and the offline
// takes nothing offline.
func TestGuardedCancel(t *testing.T) {
	e := newStopEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.onRES = cancel
	if _, err := e.stopper(ModeApply).guardedStop(ctx, []StopLib{e.lib(10)}); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v", err)
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c, "offline") || strings.HasPrefix(c, "stop") {
			t.Errorf("%q after the cancel", c)
		}
	}
}

// TestGuardedInactiveLibraryLoadedDrive: an inactive library whose
// vtltape@ still runs with a cartridge. Apply mode refuses it unless the
// plan stops the drive; rollback and recover unload it as unknown and stop
// its vtltape@.
func TestGuardedInactiveLibraryLoadedDrive(t *testing.T) {
	setup := func() *stopEnv {
		e := newStopEnv(t)
		e.state["vtllibrary@10.service"] = "inactive"
		e.state["vtltape@12.service"] = "inactive"
		e.loaded["DRV11"] = true
		return e
	}
	e := setup()
	if _, err := e.stopper(ModeApply).guardedStop(context.Background(), []StopLib{e.lib(10)}); !errors.Is(err, errRaced) {
		t.Errorf("apply, drive not planned: %v", err)
	}
	e = setup()
	sl := e.lib(10)
	sl.Drives = []int{11}
	out, err := e.stopper(ModeApply).guardedStop(context.Background(), []StopLib{sl})
	if err != nil || len(out[0].LoadedDrives) != 1 || strings.Join(out[0].Stopped, " ") != "vtllibrary@10.service vtltape@11.service" {
		t.Errorf("apply, drive planned: %v %+v", err, out)
	}
	for _, mode := range []StopMode{ModeRollback, ModeRecover} {
		e = setup()
		out, err := e.stopper(mode).guardedStop(context.Background(), []StopLib{e.lib(10)})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if fu := out[0].ForcedUnloads; len(fu) != 1 || fu[0] != (ForcedUnload{Library: 10, Drive: 11, Unknown: true}) ||
			strings.Join(out[0].Stopped, " ") != "vtllibrary@10.service vtltape@11.service" {
			t.Errorf("%s: %+v", mode, out[0])
		}
		for _, c := range e.calls {
			if c == "TUR DRV12" {
				t.Errorf("%s: TUR of an inactive vtltape", mode)
			}
		}
	}
}

// TestGuardedStopSettles: after a failed stop only inactive or failed
// count as stopped. A unit deactivating is polled until it leaves; one
// still deactivating after the bound counts as stopped, and the library's
// .persist is not trusted; any other state is a failed stop.
func TestGuardedStopSettles(t *testing.T) {
	const lib = "vtllibrary@10.service"
	run := func(seq []string, state string) (*LibStop, error) {
		e := newStopEnv(t)
		e.failOn = lib
		// The first ActiveState is prepare's; the rest answer settled.
		e.seq = map[string][]string{lib: append([]string{"active"}, seq...)}
		if state != "" {
			e.state[lib] = state
		}
		e.onStop = func(u string) {
			if u == lib {
				os.WriteFile(filepath.Join(e.dir, "library_contents.10.persist"), []byte("\nSlot 1: P00001L8\n"), 0o644)
			}
		}
		sp := e.stopper(ModeApply)
		sp.poll, sp.wait = time.Millisecond, 20*time.Millisecond
		out, err := sp.guardedStop(context.Background(), []StopLib{e.lib(10)})
		return out[0], err
	}

	ls, err := run([]string{"deactivating", "deactivating"}, "inactive")
	if err != nil || len(ls.Stopped) != 1 || len(ls.Deactivating) != 0 || !ls.Trusted {
		t.Errorf("left deactivating: %v %+v", err, ls)
	}
	ls, err = run(nil, "deactivating")
	if err != nil || len(ls.Stopped) != 1 || fmt.Sprint(ls.Deactivating) != "["+lib+"]" || ls.Trusted || !strings.Contains(ls.PersistReason, "deactivating") {
		t.Errorf("stuck deactivating: %v %+v", err, ls)
	}
	if ls.SourceKind != SourceLive {
		t.Errorf("stuck deactivating: source %s", ls.SourceKind)
	}
	for _, state := range []string{"active", "activating", "reloading"} {
		ls, err = run(nil, state)
		if err == nil || len(ls.Stopped) != 0 {
			t.Errorf("%s: %v %+v", state, err, ls)
		}
	}
}

// TestGuardedOfflineLibrary: in rollback and recover an active library
// that the apply left offline answers READ ELEMENT STATUS only after it is
// put online; a MAP that keeps changing is read again barrierRetries times
// and recorded as drift, not raced.
func TestGuardedOfflineLibrary(t *testing.T) {
	for _, mode := range []StopMode{ModeRollback, ModeRecover} {
		e := newStopEnv(t)
		var maps []string
		for i := 0; i <= barrierRetries; i++ {
			// The online barrier's reply, R1′'s, the offline barrier's.
			maps = append(maps, "", fmt.Sprint("M", i), fmt.Sprint("M", i, "x"))
		}
		e.maps[10] = maps
		out, err := e.stopper(mode).guardedStop(context.Background(), []StopLib{e.lib(10)})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		ls := out[0]
		if !strings.HasPrefix(e.calls[0], "online 10") || !strings.Contains(ls.Drift, "M3x") || len(ls.Stopped) == 0 {
			t.Errorf("%s: calls %v, %+v", mode, e.calls, ls)
		}
		if n := strings.Count(strings.Join(e.calls, ","), "RES 10"); n != barrierRetries+1 {
			t.Errorf("%s: %d reads", mode, n)
		}
	}
	// A MAP that settles on the second read: no drift.
	e := newStopEnv(t)
	e.maps[10] = []string{"", "A", "B", "", "B", "B"}
	out, err := e.stopper(ModeRecover).guardedStop(context.Background(), []StopLib{e.lib(10)})
	if err != nil || out[0].Drift != "" || out[0].Map != "B" {
		t.Errorf("settled: %v %+v", err, out[0])
	}
}
