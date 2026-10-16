package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/drivestats"
)

func u64p(v uint64) *uint64 { return &v }

func get(h http.Handler, path, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func TestMetrics(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "A00002L8")
	fc := &fakeChangers{
		inv: changer.Inventory{
			Drives: []changer.DriveState{{Number: 1, Full: true, Barcode: "A00002L8"}, {Number: 2}},
			Slots:  []changer.SlotState{{Number: 1, Full: true, Barcode: "A00001L8"}, {Number: 2}, {Number: 3}},
			MAPs:   []changer.SlotState{{Number: 1}},
		},
		invByLib: map[int]changer.Inventory{},
	}
	ratio := 2.5
	fd := &fakeDrives{st: drivestats.Stats{
		Sequential:  &drivestats.Sequential{HostBytesWritten: u64p(1000), MediumBytesWritten: u64p(400), WriteCompressionRatio: &ratio},
		Device14:    &drivestats.DeviceStatistics{VolumeLoads: u64p(9)},
		WriteErrors: &drivestats.ErrorCounters{Corrected: u64p(1), Uncorrected: u64p(0)},
	}}
	h := NewServer(Config{ConfigDir: dir, Changers: fc, Drives: fd, Version: "1.2.3"}).Handler()
	get(h, "/api/v1/libraries", "") // counted below
	get(h, "/no/such/path", "")

	rec := get(h, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	home := homeOf(t, dir)
	for _, want := range []string{
		"# TYPE mhvtl_library_up gauge",
		`mhvtl_config_ok 1`,
		`mhvtl_library_up{library="10"} 1`,
		`mhvtl_library_slots{library="10"} 3`,
		`mhvtl_library_slots_full{library="10"} 1`,
		`mhvtl_library_map_slots{library="10"} 1`,
		`mhvtl_library_drives_loaded{library="10"} 1`,
		`mhvtl_library_drives{library="10"} 4`,
		`mhvtl_drive_stats_up{library="10",drive="11"} 1`,
		`mhvtl_drive_host_written_bytes{library="10",drive="11"} 1000`,
		`mhvtl_drive_write_compression_ratio{library="10",drive="11"} 2.5`,
		`mhvtl_drive_volume_loads_total{library="10",drive="11"} 9`,
		`mhvtl_drive_write_errors_total{library="10",drive="11",kind="corrected"} 1`,
		`mhvtl_home_ok{home="` + home + `"} 1`,
		`mhvtl_media_cartridges{home="` + home + `",backend="file",type="unknown"} 2`,
		`mhvtl_media_unreadable_cartridges{home="` + home + `"} 2`,
		`mhvtl_home_filesystem_size_bytes{home="` + home + `"} `,
		`mhvtl_control_build_info{version="1.2.3"} 1`,
		`mhvtl_control_http_requests_total{route="GET /api/v1/libraries",code="200"} 1`,
		`mhvtl_control_http_requests_total{route="unmatched",code="404"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(body, "mhvtl_drive_read_errors_total") {
		t.Error("metrics report read errors the drive did not return")
	}
	// Both libraries in the fixture share the home: its media are counted
	// once, not once per library.
	if n := strings.Count(body, "mhvtl_home_ok{"); n != 1 {
		t.Errorf("home reported %d times", n)
	}
}

func TestMetricsLibraryDown(t *testing.T) {
	fc := &fakeChangers{invErr: errors.New("no changer")}
	fd := &fakeDrives{err: drivestats.ErrReserved}
	body := get(NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc, Drives: fd}).Handler(), "/metrics", "").Body.String()
	for _, want := range []string{`mhvtl_library_up{library="10"} 0`, `mhvtl_drive_stats_up{library="10",drive="11"} 0`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, unwanted := range []string{"mhvtl_library_slots{", "mhvtl_drive_host_written_bytes"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("metrics report %q for a library that is down", unwanted)
		}
	}
}

func TestMetricsNoConfig(t *testing.T) {
	body := get(NewServer(Config{ConfigDir: t.TempDir()}).Handler(), "/metrics", "").Body.String()
	if !strings.Contains(body, "mhvtl_config_ok 0\n") {
		t.Errorf("got %s", body)
	}
}

// countingChangers counts Inventory calls, safely across goroutines.
type countingChangers struct {
	fakeChangers
	mu sync.Mutex
	n  int
}

func (c *countingChangers) Inventory(lib config.Library) (changer.Inventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return changer.Inventory{}, nil
}

func (c *countingChangers) TryInventory(lib config.Library) (changer.Inventory, error) {
	return c.Inventory(lib)
}

func TestMetricsCached(t *testing.T) {
	cc := &countingChangers{}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: cc}).Handler()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := get(h, "/metrics", ""); rec.Code != http.StatusOK {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	get(h, "/metrics", "")
	// The fixture has two libraries: one collection reads each changer once.
	if cc.n != 2 {
		t.Errorf("Inventory called %d times for 9 scrapes, want 2", cc.n)
	}
}

func TestMetricsAndReadyzNeedToken(t *testing.T) {
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Token: "s3cret"}).Handler()
	for _, path := range []string{"/metrics", "/readyz"} {
		if rec := get(h, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want 401", path, rec.Code)
		}
		if rec := get(h, path, "s3cret"); rec.Code == http.StatusUnauthorized {
			t.Errorf("%s with token: status = %d", path, rec.Code)
		}
	}
	if rec := get(h, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz without token: status = %d, want 200", rec.Code)
	}
}

type readyResp struct {
	Status string       `json:"status"`
	Checks []readyCheck `json:"checks"`
}

func readyz(t *testing.T, cfg Config) (int, readyResp) {
	t.Helper()
	rec := get(NewServer(cfg).Handler(), "/readyz", "")
	var r readyResp
	if err := json.NewDecoder(rec.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rec.Code, r
}

func failed(r readyResp) []string {
	var out []string
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestReadyz(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8")
	home := homeOf(t, dir)
	// A detached raw disk is not a failure.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(home, "R00001L8")); err != nil {
		t.Fatal(err)
	}
	code, r := readyz(t, Config{ConfigDir: dir, Changers: &fakeChangers{}})
	if code != http.StatusOK || r.Status != "ok" || len(failed(r)) != 0 {
		t.Fatalf("status %d %+v", code, r)
	}
	names := map[string]bool{}
	for _, c := range r.Checks {
		names[c.Name] = true
	}
	for _, n := range []string{"config", "changer:10", "changer:30", "media:" + home} {
		if !names[n] {
			t.Errorf("no %q check in %+v", n, r.Checks)
		}
	}

	// An attached disk without a valid superblock fails the media check.
	disk := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(disk, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disk, filepath.Join(home, "R00002L8")); err != nil {
		t.Fatal(err)
	}
	code, r = readyz(t, Config{ConfigDir: dir, Changers: &fakeChangers{invErr: errors.New("gone")}})
	if code != http.StatusServiceUnavailable || r.Status != "fail" {
		t.Fatalf("status %d %+v", code, r)
	}
	if got := strings.Join(failed(r), ","); got != "changer:10,changer:30,media:"+home {
		t.Errorf("failed checks = %s", got)
	}
	for _, c := range r.Checks {
		if c.Name == "media:"+home && !strings.Contains(c.Error, "R00002L8") {
			t.Errorf("media check error %q does not name the cartridge", c.Error)
		}
	}
}

func TestReadyzNoConfig(t *testing.T) {
	code, r := readyz(t, Config{ConfigDir: t.TempDir()})
	if code != http.StatusServiceUnavailable || strings.Join(failed(r), ",") != "config" {
		t.Fatalf("status %d %+v", code, r)
	}
}

// movingChangers is a changer whose reads block while a "move" holds it, as
// changer.Manager.Inventory waits for a Mount in progress. TryInventory
// reports ErrBusy when busy is set, like the Manager.
type movingChangers struct {
	fakeChangers
	mu      sync.Mutex
	inv     changer.Inventory
	busy    bool
	release chan struct{} // reads block until it is closed, when non-nil
}

func (m *movingChangers) Inventory(lib config.Library) (changer.Inventory, error) {
	m.mu.Lock()
	release, inv := m.release, m.inv
	m.mu.Unlock()
	if release != nil {
		<-release
	}
	return inv, nil
}

func (m *movingChangers) TryInventory(lib config.Library) (changer.Inventory, error) {
	m.mu.Lock()
	busy := m.busy
	m.mu.Unlock()
	if busy {
		return changer.Inventory{}, changer.ErrBusy
	}
	return m.Inventory(lib)
}

// within fails the test unless fn returns within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

func TestMetricsBusyKeepsLastState(t *testing.T) {
	mc := &movingChangers{inv: changer.Inventory{Slots: []changer.SlotState{{Number: 1, Full: true}, {Number: 2}}}}
	srv := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: mc})
	srv.metrics.ttl = time.Nanosecond // every request collects
	h := srv.Handler()
	get(h, "/metrics", "")

	mc.mu.Lock()
	mc.busy = true
	mc.mu.Unlock()
	time.Sleep(time.Millisecond)
	body := get(h, "/metrics", "").Body.String()
	for _, want := range []string{
		`mhvtl_library_busy{library="10"} 1`,
		`mhvtl_library_up{library="10"} 1`,
		`mhvtl_library_slots{library="10"} 2`,
		`mhvtl_library_slots_full{library="10"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("busy library: metrics lack %q", want)
		}
	}
	rec := get(h, "/readyz", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "busy with another operation") {
		t.Errorf("readyz on a busy library: %d %s", rec.Code, rec.Body.String())
	}
}

// TestScrapeDuringMove pins that a scrape and a probe never wait for a
// changer read stuck behind a move: they serve the previous collection, and
// mhvtl_control_metrics_age_seconds shows how old it is.
func TestScrapeDuringMove(t *testing.T) {
	mc := &movingChangers{inv: changer.Inventory{Slots: []changer.SlotState{{Number: 1, Full: true}}}}
	srv := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: mc})
	srv.metrics.ttl = time.Nanosecond
	srv.metrics.wait = 50 * time.Millisecond
	h := srv.Handler()
	get(h, "/metrics", "")

	release := make(chan struct{})
	mc.mu.Lock()
	mc.release = release
	mc.mu.Unlock()
	defer close(release)
	time.Sleep(time.Millisecond)

	var rec *httptest.ResponseRecorder
	within(t, 2*time.Second, "a scrape during a move", func() { rec = get(h, "/metrics", "") })
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `mhvtl_library_slots_full{library="10"} 1`) ||
		!strings.Contains(body, "mhvtl_control_metrics_age_seconds ") {
		t.Fatalf("scrape during a move: %d\n%s", rec.Code, body)
	}
	// The stuck collection is still running: these don't wait at all.
	within(t, 2*time.Second, "a second scrape", func() { get(h, "/metrics", "") })
	within(t, 2*time.Second, "/readyz during a move", func() { rec = get(h, "/readyz", "") })
	if rec.Code != http.StatusOK {
		t.Errorf("readyz during a move: %d %s", rec.Code, rec.Body.String())
	}
}

// TestReadyzHonoursContext pins that a probe arriving before any collection
// has finished gives up when its context ends.
func TestReadyzHonoursContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mc := &movingChangers{release: release}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: mc}).Handler()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	within(t, 2*time.Second, "/readyz with an expiring context", func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no collection has finished yet") {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
}
