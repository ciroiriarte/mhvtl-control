package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
)

// unitsHelper is a HelperService that answers hello ready and units with
// resp or err, counting the units calls.
type unitsHelper struct {
	mu    sync.Mutex
	resp  apply.Response
	err   error
	units int
}

func (h *unitsHelper) Hello(context.Context) (string, error) { return applyclient.Present, nil }

func (h *unitsHelper) Do(_ context.Context, req apply.Request) (apply.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.Op != apply.OpUnits {
		return apply.Response{}, fmt.Errorf("unexpected op %s", req.Op)
	}
	h.units++
	return h.resp, h.err
}

func (h *unitsHelper) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.units
}

// serviceSys builds a sysfs with the given modules, mhvtl's with refcnt.
func serviceSys(t *testing.T, mhvtlRefcnt string, modules ...string) string {
	t.Helper()
	sys := t.TempDir()
	dirs := []string{filepath.Join(sys, "class", "scsi_device")}
	for _, m := range modules {
		dirs = append(dirs, filepath.Join(sys, "module", m))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if mhvtlRefcnt != "" {
		if err := os.WriteFile(filepath.Join(sys, "module", "mhvtl", "refcnt"), []byte(mhvtlRefcnt+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sys
}

func getService(t *testing.T, srv *Server) serviceDTO {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/service", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	return decodeBody[serviceDTO](t, rec)
}

func noQueue(t *testing.T, exists bool) {
	t.Helper()
	old := queueExists
	queueExists = func() bool { return exists }
	t.Cleanup(func() { queueExists = old })
}

// The units the helper reports land on the target, the support units, and
// each library and its drives; device.conf gives PERSIST and sysfs the ch
// exposure. The answer is reused for 2 s.
func TestServiceUnits(t *testing.T) {
	noQueue(t, true)
	dir := writeConfigDir(t)
	conf := filepath.Join(dir, "device.conf")
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// Library 30 persists its placement; 10 does not.
	i := strings.Index(string(b), "Library: 30")
	b = append(b[:i:i], strings.Replace(string(b[i:]), "PERSIST: False", "PERSIST: True", 1)...)
	if err := os.WriteFile(conf, b, 0o644); err != nil {
		t.Fatal(err)
	}
	unit := func(name, active string, pid int, up uint64) apply.UnitStatus {
		return apply.UnitStatus{Name: name, LoadState: "loaded", ActiveState: active, SubState: "running", MainPID: pid, UptimeUS: up}
	}
	resp := apply.Response{Result: apply.ResultOK, UnitStates: []apply.UnitStatus{
		unit("mhvtl.target", "active", 0, 90e6),
		unit("mhvtl-load-modules.service", "active", 0, 91e6),
		{Name: "mhvtl-tcmu-handler.service", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"},
		unit("mhvtl-media.service", "inactive", 0, 0),
		unit("mhvtl-bay-monitor.service", "active", 77, 80e6),
		unit("vtllibrary@10.service", "active", 100, 60e6),
		unit("vtllibrary@30.service", "failed", 0, 0),
	}}
	for _, d := range []int{11, 12, 13, 14, 31, 32, 33, 34} {
		resp.UnitStates = append(resp.UnitStates, unit(fmt.Sprintf("vtltape@%d.service", d), "active", 200+d, 50e6))
	}
	h := &unitsHelper{resp: resp}
	// mhvtl.ko loaded and ch loaded: every library is exposed unless this
	// kernel has the fix.
	sys := serviceSys(t, "9", "mhvtl")
	if err := os.MkdirAll(filepath.Join(sys, "bus", "scsi", "drivers", "ch"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{ConfigDir: dir, SysRoot: sys, Helper: h})
	clock := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return clock }

	got := getService(t, srv)
	if got.Manager != "systemd" || got.Helper != "present" || len(got.Errors) != 0 || !got.Queue.Exists {
		t.Errorf("service: %+v", got)
	}
	if got.Target == nil || *got.Target != resp.UnitStates[0] {
		t.Errorf("target %+v", got.Target)
	}
	if len(got.Support) != 4 || got.Support[1].LoadState != "not-found" || got.Support[3].MainPID != 77 {
		t.Errorf("support %+v", got.Support)
	}
	if len(got.Libraries) != 2 {
		t.Fatalf("libraries %+v", got.Libraries)
	}
	l10, l30 := got.Libraries[0], got.Libraries[1]
	if l10.ID != 10 || l10.Unit == nil || l10.Unit.MainPID != 100 || l10.Unit.UptimeUS != 60e6 || l10.Persist {
		t.Errorf("library 10: %+v", l10)
	}
	if l30.ID != 30 || l30.Unit == nil || l30.Unit.ActiveState != "failed" || !l30.Persist {
		t.Errorf("library 30: %+v", l30)
	}
	if len(l10.Drives) != 4 || l10.Drives[0].ID != 11 || l10.Drives[0].Unit == nil || l10.Drives[0].Unit.MainPID != 211 {
		t.Errorf("library 10 drives: %+v", l10.Drives)
	}
	if got.KernelCh == nil || !got.KernelCh.ChLoaded {
		t.Fatalf("kernel_ch %+v", got.KernelCh)
	}
	if fixed := got.KernelCh.KernelFixed; l10.ChExposed == fixed || l30.ChExposed == fixed {
		t.Errorf("ch_exposed %v %v with kernel_fixed %v", l10.ChExposed, l30.ChExposed, fixed)
	}
	if got.StandaloneDrives == nil || len(got.StandaloneDrives) != 0 {
		t.Errorf("standalone drives %+v", got.StandaloneDrives)
	}

	// Reused within 2 s, asked again after.
	clock = clock.Add(1999 * time.Millisecond)
	getService(t, srv)
	if h.calls() != 1 {
		t.Errorf("%d units calls within 2 s", h.calls())
	}
	clock = clock.Add(time.Millisecond)
	getService(t, srv)
	if h.calls() != 2 {
		t.Errorf("%d units calls after 2 s", h.calls())
	}
}

// Without a helper the units are unknown, not an error: manager is
// unavailable and the rest is still reported, with HTTP 200.
func TestServiceNoHelper(t *testing.T) {
	noQueue(t, false)
	dir := writeConfigDir(t)
	for name, h := range map[string]HelperService{
		"not configured": nil,
		"no socket":      &unitsHelper{err: fmt.Errorf("%w: connect: no such file", applyclient.ErrAbsent)},
	} {
		cfg := Config{ConfigDir: dir, SysRoot: serviceSys(t, "")}
		if h != nil {
			cfg.Helper = h
		}
		got := getService(t, NewServer(cfg))
		if got.Manager != "unavailable" || got.Target != nil || got.Support != nil || len(got.Errors) != 0 || got.Queue.Exists {
			t.Errorf("%s: %+v", name, got)
		}
		if len(got.Libraries) != 2 || got.Libraries[0].Unit != nil || len(got.Libraries[0].Drives) != 4 || got.Libraries[0].Drives[0].Unit != nil {
			t.Errorf("%s: libraries %+v", name, got.Libraries)
		}
	}
	// A helper that fails is reported in errors.
	for name, h := range map[string]*unitsHelper{
		"transport": {err: errors.New("broken pipe")},
		"refused":   {resp: apply.Response{Result: apply.ResultRefused, Message: "no"}},
	} {
		got := getService(t, NewServer(Config{ConfigDir: dir, SysRoot: serviceSys(t, ""), Helper: h}))
		if got.Manager != "unavailable" || len(got.Errors) != 1 || got.Target != nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// The backend: what mhvtl.conf configures, next to what the kernel has
// loaded, each reported as found.
func TestServiceBackend(t *testing.T) {
	noQueue(t, true)
	for _, tc := range []struct {
		name, conf, refcnt string
		modules            []string
		configured         string
		loaded, tcmu       bool
		refcount           int
	}{
		{"kernel module", "", "5", []string{"mhvtl"}, "mhvtl", true, false, 5},
		{"tcmu", "MHVTL_BACKEND=tcmu\n", "", []string{"target_core_user"}, "tcmu", false, true, -1},
		{"tcmu with mhvtl.ko still loaded", "MHVTL_BACKEND=\"tcmu\"\n", "2", []string{"mhvtl", "target_core_user"}, "tcmu", true, true, 2},
		{"nothing loaded", "MHVTL_BACKEND=mhvtl\n", "", nil, "mhvtl", false, false, -1},
	} {
		dir := writeConfigDir(t)
		if tc.conf != "" {
			if err := os.WriteFile(filepath.Join(dir, "mhvtl.conf"), []byte("VERBOSE=1\n"+tc.conf), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := getService(t, NewServer(Config{ConfigDir: dir, SysRoot: serviceSys(t, tc.refcnt, tc.modules...)}))
		b := got.Backend
		if b.Configured != tc.configured || b.KernelModule.Loaded != tc.loaded || b.TCMU.TargetCoreUser != tc.tcmu {
			t.Errorf("%s: backend %+v", tc.name, b)
		}
		if (tc.refcount < 0) != (b.KernelModule.Refcount == nil) || (b.KernelModule.Refcount != nil && *b.KernelModule.Refcount != tc.refcount) {
			t.Errorf("%s: refcount %v, want %d", tc.name, b.KernelModule.Refcount, tc.refcount)
		}
		if len(got.Errors) != 0 {
			t.Errorf("%s: errors %v", tc.name, got.Errors)
		}
	}
}

// blockingHelper answers hello at once and units only when its context
// ends, as a wedged helper does.
type blockingHelper struct {
	unitsHelper
	started chan struct{}
}

func (h *blockingHelper) Do(ctx context.Context, req apply.Request) (apply.Response, error) {
	h.unitsHelper.Do(ctx, req)
	h.started <- struct{}{}
	<-ctx.Done()
	return apply.Response{}, ctx.Err()
}

// serviceWaits sets the hello and units bounds for a test.
func serviceWaits(t *testing.T, hello, units time.Duration) {
	t.Helper()
	oldHello, oldUnits := serviceHelloWait, serviceUnitsWait
	serviceHelloWait, serviceUnitsWait = hello, units
	t.Cleanup(func() { serviceHelloWait, serviceUnitsWait = oldHello, oldUnits })
}

// A wedged helper delays neither GET /system nor a GET /service past its
// bounds, and concurrent GET /service share the one read.
func TestServiceHelperWedged(t *testing.T) {
	noQueue(t, false)
	serviceWaits(t, 300*time.Millisecond, 300*time.Millisecond)
	h := &blockingHelper{started: make(chan struct{}, 4)}
	srv := NewServer(Config{ConfigDir: writeConfigDir(t), SysRoot: serviceSys(t, ""), Helper: h})
	handler := srv.Handler()

	type answer struct {
		dto serviceDTO
		d   time.Duration
	}
	answers := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		go func() {
			start := time.Now()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/service", nil))
			var dto serviceDTO
			json.Unmarshal(rec.Body.Bytes(), &dto)
			answers <- answer{dto, time.Since(start)}
		}()
	}
	<-h.started // the units call is in flight
	start := time.Now()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if rec.Code != http.StatusOK || time.Since(start) > 100*time.Millisecond {
		t.Errorf("GET /system with units in flight: %d after %v", rec.Code, time.Since(start))
	}
	for i := 0; i < 2; i++ {
		a := <-answers
		if a.d > 2*time.Second || a.dto.Manager != "unavailable" || len(a.dto.Errors) != 1 || len(a.dto.Libraries) != 2 {
			t.Errorf("GET /service with a wedged helper: %+v after %v", a.dto, a.d)
		}
	}
	if h.calls() != 1 {
		t.Errorf("%d units calls for two concurrent requests", h.calls())
	}
}

// slowUnitsHelper takes hello and units each most of its own allowance.
type slowUnitsHelper struct {
	unitsHelper
	hello, units time.Duration
}

func (h *slowUnitsHelper) Hello(ctx context.Context) (string, error) {
	select {
	case <-time.After(h.hello):
		return applyclient.Present, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (h *slowUnitsHelper) Do(ctx context.Context, req apply.Request) (apply.Response, error) {
	select {
	case <-time.After(h.units):
		return h.unitsHelper.Do(ctx, req)
	case <-ctx.Done():
		return apply.Response{}, ctx.Err()
	}
}

// A slow hello does not eat into units' deadline: each has its own, and
// the read ends within the longer.
func TestServiceSlowHello(t *testing.T) {
	noQueue(t, false)
	serviceWaits(t, 400*time.Millisecond, 400*time.Millisecond)
	h := &slowUnitsHelper{unitsHelper: unitsHelper{resp: apply.Response{Result: apply.ResultOK, UnitStates: []apply.UnitStatus{
		{Name: "mhvtl.target", LoadState: "loaded", ActiveState: "active"},
	}}}, hello: 300 * time.Millisecond, units: 300 * time.Millisecond}
	start := time.Now()
	got := getService(t, NewServer(Config{ConfigDir: writeConfigDir(t), SysRoot: serviceSys(t, ""), Helper: h}))
	if got.Manager != "systemd" || got.Helper != "present" || got.Target == nil || len(got.Errors) != 0 {
		t.Errorf("slow hello: %+v", got)
	}
	if d := time.Since(start); d > 550*time.Millisecond {
		t.Errorf("GET /service took %v: hello and units did not run side by side", d)
	}
}
