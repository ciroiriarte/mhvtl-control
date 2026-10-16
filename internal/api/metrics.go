package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/drivestats"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/promtext"
)

// metricsTTL is how long a collection is served before a request starts
// the next one. It bounds how often scrapes and probes reach the changers,
// drives and raw disks, however many clients ask and however often.
const metricsTTL = 10 * time.Second

// metricsWait is how long a request waits for a collection it started before
// it serves the previous one instead.
const metricsWait = 2 * time.Second

// metricsCache holds the last collection: the metrics body and the readiness
// checks computed with it. Collections run one at a time, each in its own
// goroutine. A request that finds a previous collection never waits longer
// than wait for a new one, and not at all when one is already running, so a
// scrape or probe does not queue behind a cartridge move.
type metricsCache struct {
	ttl, wait time.Duration // metricsTTL and metricsWait when zero

	mu      sync.Mutex
	at      time.Time
	body    []byte
	checks  []readyCheck
	running chan struct{} // closed when the running collection ends

	// live is each library's last inventory, reported while its changer is
	// busy with a move. Only the running collection uses it.
	live map[int]changer.Inventory
}

// collection is one finished collection.
type collection struct {
	at     time.Time
	body   []byte
	checks []readyCheck
}

// latest returns the latest collection, starting a new one when the cached
// one is older than the TTL. It waits for the new one only when it started
// it (up to the wait) or when there is no collection yet; ctx ends the wait
// early. ok is false when no collection has finished.
func (s *Server) latest(ctx context.Context) (col collection, ok bool) {
	c := &s.metrics
	c.mu.Lock()
	ttl, wait := c.ttl, c.wait
	if ttl == 0 {
		ttl = metricsTTL
	}
	if wait == 0 {
		wait = metricsWait
	}
	have := c.body != nil
	fresh := have && time.Since(c.at) < ttl
	started := false
	if !fresh && c.running == nil {
		c.running, started = make(chan struct{}), true
		go s.refresh(c.running)
	}
	running := c.running
	c.mu.Unlock()

	if !fresh && (started || !have) {
		var timeout <-chan time.Time
		if have {
			t := time.NewTimer(wait)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-running:
		case <-timeout:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return collection{at: c.at, body: c.body, checks: c.checks}, c.body != nil
}

// refresh runs one collection, stores it and closes done.
func (s *Server) refresh(done chan struct{}) {
	// The collection runs outside any request, so recoverMW can't catch a
	// panic here: log it, keep the previous collection and let the next
	// request try again, rather than taking the daemon down.
	defer func() {
		if rv := recover(); rv != nil {
			log.Printf("panic collecting metrics: %v", rv)
			c := &s.metrics
			c.mu.Lock()
			c.running = nil
			c.mu.Unlock()
			close(done)
		}
	}()
	var set promtext.Set
	start := time.Now()
	checks := s.collect(&set)
	set.Family("mhvtl_control_collect_duration_seconds",
		"Time the last collection of library, media and drive state took.", promtext.Gauge).
		Set(time.Since(start).Seconds())
	var b bytes.Buffer
	_, _ = set.WriteTo(&b)

	c := &s.metrics
	c.mu.Lock()
	c.body, c.checks, c.at, c.running = b.Bytes(), checks, time.Now(), nil
	c.mu.Unlock()
	close(done)
}

// requestCounter counts handled requests by route pattern and status code.
type requestCounter struct {
	mu sync.Mutex
	n  map[[2]string]uint64
}

func (c *requestCounter) add(route string, code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[[2]string]uint64{}
	}
	if route == "" {
		route = "unmatched"
	}
	c.n[[2]string{route, strconv.Itoa(code)}]++
}

// handleMetrics serves GET /metrics in the Prometheus text format: the
// latest collection of library, media, home and drive state, then the
// daemon's own metrics, which are always current.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	col, ok := s.latest(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no collection has finished yet")
		return
	}
	var own promtext.Set
	s.ownMetrics(&own)
	own.Family("mhvtl_control_metrics_age_seconds",
		"Age of the collection served: above the 10s cache while a collection is still running.", promtext.Gauge).
		Set(time.Since(col.at).Seconds())
	w.Header().Set("Content-Type", promtext.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(col.body)
	_, _ = own.WriteTo(w)
}

// ownMetrics adds the daemon's build info and request counters.
func (s *Server) ownMetrics(set *promtext.Set) {
	set.Family("mhvtl_control_build_info", "mhvtl-control version; always 1.", promtext.Gauge).
		Set(1, "version", s.cfg.Version)
	req := set.Family("mhvtl_control_http_requests_total",
		"HTTP requests handled, by route pattern and status code.", promtext.Counter)
	s.requests.mu.Lock()
	defer s.requests.mu.Unlock()
	for k, n := range s.requests.n {
		req.Set(float64(n), "route", k[0], "code", k[1])
	}
}

// collect adds library, media, home filesystem and drive metrics to set and
// returns the readiness checks. A library whose changer cannot be read
// reports mhvtl_library_up 0 and no live gauges; one whose changer is busy
// with another operation reports mhvtl_library_busy 1 and its last known
// gauges. A drive whose statistics cannot be read reports
// mhvtl_drive_stats_up 0.
func (s *Server) collect(set *promtext.Set) []readyCheck {
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	set.Family("mhvtl_config_ok", "Whether device.conf could be read and parsed.", promtext.Gauge).
		Set(boolValue(err == nil))
	checks := []readyCheck{newCheck("config", err)}
	if err != nil {
		return checks
	}
	homes := map[string]bool{}
	for _, lib := range inv.Libraries {
		if c := s.collectLibrary(set, lib); c != nil {
			checks = append(checks, *c)
		}
		homes[s.homeFor(lib)] = true
	}
	// Libraries may share a home directory; count its media once.
	for _, home := range sortedKeys(homes) {
		checks = append(checks, collectHome(set, home))
	}
	return checks
}

// collectLibrary adds lib's metrics and returns its changer check, or nil
// when changer access is not configured.
func (s *Server) collectLibrary(set *promtext.Set, lib config.Library) *readyCheck {
	id := strconv.Itoa(lib.ID)
	set.Family("mhvtl_library_info", "Library identity from device.conf; always 1.", promtext.Gauge).
		Set(1, "library", id, "vendor", lib.Vendor, "product", lib.Product, "serial", lib.Serial, "home", s.homeFor(lib))
	set.Family("mhvtl_library_drives", "Drives configured in the library.", promtext.Gauge).
		Set(float64(len(lib.Drives)), "library", id)

	up := set.Family("mhvtl_library_up", "Whether the library's changer answered READ ELEMENT STATUS (last known while busy).", promtext.Gauge)
	busy := set.Family("mhvtl_library_busy",
		"Whether the changer was busy with another operation, such as a cartridge move; the library's gauges are then its last known state.", promtext.Gauge)
	var check *readyCheck
	if s.cfg.Changers == nil {
		up.Set(0, "library", id)
	} else {
		c := &s.metrics
		if c.live == nil {
			c.live = map[int]changer.Inventory{}
		}
		live, err := s.cfg.Changers.TryInventory(lib)
		name := "changer:" + id
		switch {
		case errors.Is(err, changer.ErrBusy):
			busy.Set(1, "library", id)
			if last, ok := c.live[lib.ID]; ok {
				check = &readyCheck{Name: name, OK: true, Note: "busy with another operation; last known state kept"}
				up.Set(1, "library", id)
				liveGauges(set, id, last)
			} else {
				// It has not answered since it was last down, or since start.
				check = &readyCheck{Name: name, Error: "busy with another operation; no state read yet"}
			}
		case err != nil:
			busy.Set(0, "library", id)
			up.Set(0, "library", id)
			delete(c.live, lib.ID)
			check = &readyCheck{Name: name, Error: err.Error()}
		default:
			busy.Set(0, "library", id)
			up.Set(1, "library", id)
			c.live[lib.ID] = live
			liveGauges(set, id, live)
			check = &readyCheck{Name: name, OK: true}
		}
	}

	if s.cfg.Drives != nil {
		for _, d := range lib.Drives {
			collectDrive(set, id, d, s.cfg.Drives)
		}
	}
	return check
}

// liveGauges adds the slot, MAP and drive gauges of library id's inventory.
func liveGauges(set *promtext.Set, id string, live changer.Inventory) {
	slots, full := 0, 0
	for _, sl := range live.Slots {
		slots++
		full += boolInt(sl.Full)
	}
	maps, mapsFull := 0, 0
	for _, m := range live.MAPs {
		maps++
		mapsFull += boolInt(m.Full)
	}
	loaded := 0
	for _, d := range live.Drives {
		loaded += boolInt(d.Full)
	}
	set.Family("mhvtl_library_slots", "Storage slots.", promtext.Gauge).Set(float64(slots), "library", id)
	set.Family("mhvtl_library_slots_full", "Storage slots holding a cartridge.", promtext.Gauge).Set(float64(full), "library", id)
	set.Family("mhvtl_library_map_slots", "Import/export (MAP) slots.", promtext.Gauge).Set(float64(maps), "library", id)
	set.Family("mhvtl_library_map_slots_full", "MAP slots holding a cartridge.", promtext.Gauge).Set(float64(mapsFull), "library", id)
	set.Family("mhvtl_library_drives_loaded", "Drives holding a cartridge.", promtext.Gauge).Set(float64(loaded), "library", id)
}

func collectDrive(set *promtext.Set, lib string, d config.Drive, r DriveStatsService) {
	id := strconv.Itoa(d.ID)
	up := set.Family("mhvtl_drive_stats_up",
		"Whether the drive's log pages could be read (0 also when another initiator holds a reservation).", promtext.Gauge)
	st, err := r.Read(d)
	if err != nil {
		up.Set(0, "library", lib, "drive", id)
		return
	}
	up.Set(1, "library", lib, "drive", id)
	labels := []string{"library", lib, "drive", id}
	gauge := func(name, help string, v *uint64) {
		if v != nil {
			set.Family(name, help, promtext.Gauge).Set(float64(*v), labels...)
		}
	}
	counter := func(name, help string, v *uint64, extra ...string) {
		if v != nil {
			set.Family(name, help, promtext.Counter).Set(float64(*v), append(labels[:4:4], extra...)...)
		}
	}
	if q := st.Sequential; q != nil {
		gauge("mhvtl_drive_host_written_bytes", "Bytes written by the host during the current load, before compression (LOG SENSE 0Ch).", q.HostBytesWritten)
		gauge("mhvtl_drive_medium_written_bytes", "Bytes written to the medium during the current load, after compression (LOG SENSE 0Ch).", q.MediumBytesWritten)
		gauge("mhvtl_drive_host_read_bytes", "Bytes read by the host during the current load (LOG SENSE 0Ch).", q.HostBytesRead)
		gauge("mhvtl_drive_medium_read_bytes", "Bytes read from the medium during the current load (LOG SENSE 0Ch).", q.MediumBytesRead)
		if q.WriteCompressionRatio != nil {
			set.Family("mhvtl_drive_write_compression_ratio",
				"Host bytes written over medium bytes written during the current load.", promtext.Gauge).
				Set(*q.WriteCompressionRatio, labels...)
		}
	}
	if v := st.Device14; v != nil {
		counter("mhvtl_drive_volume_loads_total", "Cartridge loads over the drive's life (LOG SENSE 14h).", v.VolumeLoads)
	}
	for _, e := range []struct {
		dir, page string
		c         *drivestats.ErrorCounters
	}{{"write", "02h", st.WriteErrors}, {"read", "03h", st.ReadErrors}} {
		if e.c == nil {
			continue
		}
		name := "mhvtl_drive_" + e.dir + "_errors_total"
		help := "Drive " + e.dir + " error counters over the drive's life (LOG SENSE " + e.page + "), by kind."
		counter(name, help, e.c.Corrected, "kind", "corrected")
		counter(name, help, e.c.Uncorrected, "kind", "uncorrected")
		counter(name, help, e.c.Retries, "kind", "retries")
	}
	if st.TemperatureC != nil {
		set.Family("mhvtl_drive_temperature_celsius", "Drive temperature (LOG SENSE 0Dh).", promtext.Gauge).
			Set(float64(*st.TemperatureC), labels...)
	}
}

// collectHome adds the media and filesystem metrics of one home directory
// and returns its readiness check: the home can be listed, and no raw
// cartridge whose disk is attached has an unreadable superblock or MAM. A
// detached disk is not a failure: bays are removable.
func collectHome(set *promtext.Set, home string) readyCheck {
	ok := set.Family("mhvtl_home_ok", "Whether the media home directory could be listed.", promtext.Gauge)
	carts, err := media.List(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		ok.Set(0, "home", home)
		return newCheck("media:"+home, err)
	}
	ok.Set(1, "home", home)
	if err == nil {
		var fs syscall.Statfs_t
		if syscall.Statfs(home, &fs) == nil {
			set.Family("mhvtl_home_filesystem_size_bytes", "Size of the filesystem holding the media home.", promtext.Gauge).
				Set(float64(fs.Blocks)*float64(fs.Bsize), "home", home)
			set.Family("mhvtl_home_filesystem_avail_bytes", "Bytes available to unprivileged users on the filesystem holding the media home.", promtext.Gauge).
				Set(float64(fs.Bavail)*float64(fs.Bsize), "home", home)
		}
	}

	count := set.Family("mhvtl_media_cartridges", "Cartridges in the media home, by backend and medium type.", promtext.Gauge)
	wp := set.Family("mhvtl_media_write_protected_cartridges", "Write-protected cartridges in the media home.", promtext.Gauge)
	capacity := set.Family("mhvtl_media_capacity_bytes", "Sum of the cartridges' native capacity (MAM).", promtext.Gauge)
	remaining := set.Family("mhvtl_media_remaining_bytes", "Sum of the cartridges' remaining capacity, as of each one's last MAM update.", promtext.Gauge)
	written := set.Family("mhvtl_media_lifetime_written_bytes", "Sum of the bytes written to the cartridges over their life (MAM).", promtext.Gauge)
	rawUsed := set.Family("mhvtl_media_raw_used_bytes", "Payload bytes on the disks of raw-backend cartridges.", promtext.Gauge)
	absent := set.Family("mhvtl_media_raw_absent_cartridges", "Raw-backend cartridges whose disk is not attached.", promtext.Gauge)
	errs := set.Family("mhvtl_media_unreadable_cartridges", "Cartridges whose metadata could not be read.", promtext.Gauge)
	for _, f := range []*promtext.Family{wp, capacity, remaining, written, rawUsed, absent, errs} {
		f.Add(0, "home", home)
	}
	for _, c := range carts {
		typ := c.Type
		if typ == "" {
			typ = "unknown"
		}
		count.Add(1, "home", home, "backend", c.Backend, "type", typ)
		wp.Add(float64(boolInt(c.WriteProtected)), "home", home)
		capacity.Add(float64(c.CapacityBytes), "home", home)
		remaining.Add(float64(c.RemainingBytes), "home", home)
		written.Add(float64(c.WrittenBytes), "home", home)
		rawUsed.Add(float64(c.UsedBytes), "home", home)
		absent.Add(float64(boolInt(c.MediaAbsent)), "home", home)
		errs.Add(float64(boolInt(c.Error != "")), "home", home)
	}
	return newCheck("media:"+home, rawMediaError(carts))
}

// readyCheck is one check of GET /readyz.
type readyCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

func newCheck(name string, err error) readyCheck {
	if err != nil {
		return readyCheck{Name: name, Error: err.Error()}
	}
	return readyCheck{Name: name, OK: true}
}

// handleReadyz serves GET /readyz from the latest collection, so a probe
// causes at most one collection per TTL and never waits behind a cartridge
// move: 200 when device.conf parses, every library's changer answers (or
// is busy with a move) and no attached raw cartridge is unreadable; 503
// otherwise, and while no collection has finished. The body lists each
// check and the collection's age. The message queue is not checked: the
// queue client has no request that is free of side effects.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	col, ok := s.latest(r.Context())
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "fail", "checks": []readyCheck{
			{Name: "collection", Error: "no collection has finished yet"},
		}})
		return
	}
	status, code := "ok", http.StatusOK
	for _, c := range col.checks {
		if !c.OK {
			status, code = "fail", http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, map[string]any{
		"status":      status,
		"checks":      col.checks,
		"age_seconds": time.Since(col.at).Seconds(),
	})
}

// rawMediaError names the raw cartridges whose disk is attached but whose
// superblock or MAM could not be read.
func rawMediaError(carts []media.Cartridge) error {
	var bad []string
	for _, c := range carts {
		if c.Backend == media.BackendRaw && !c.MediaAbsent && c.Error != "" {
			bad = append(bad, c.Barcode+": "+c.Error)
		}
	}
	if len(bad) > 0 {
		return errors.New("raw cartridges unreadable: " + joinMax(bad, 5))
	}
	return nil
}

// joinMax joins up to n items with "; " and says how many more there are.
func joinMax(items []string, n int) string {
	var b bytes.Buffer
	for i, it := range items {
		if i == n {
			b.WriteString("; and " + strconv.Itoa(len(items)-n) + " more")
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(it)
	}
	return b.String()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func boolValue(b bool) float64 { return float64(boolInt(b)) }
