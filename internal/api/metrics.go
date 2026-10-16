package api

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/drivestats"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/promtext"
)

// metricsTTL is how long a collection is served before the next scrape
// collects again. It bounds how often scrapes reach the changers and drives,
// however many Prometheus servers scrape and however often.
const metricsTTL = 10 * time.Second

// metricsCache holds the last collection. mu is held while collecting, so
// concurrent scrapes wait for one collection instead of each starting one.
type metricsCache struct {
	mu   sync.Mutex
	at   time.Time
	body []byte
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
// cached collection of library, media, home and drive state, then the
// daemon's own counters, which are always current.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	body := s.collectCached()
	var own promtext.Set
	s.ownMetrics(&own)
	w.Header().Set("Content-Type", promtext.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	_, _ = own.WriteTo(w)
}

func (s *Server) collectCached() []byte {
	c := &s.metrics
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Since(c.at) < metricsTTL {
		return c.body
	}
	var set promtext.Set
	start := time.Now()
	s.collect(&set)
	set.Family("mhvtl_control_collect_duration_seconds",
		"Time the last collection of library, media and drive state took.", promtext.Gauge).
		Set(time.Since(start).Seconds())
	var b bytes.Buffer
	_, _ = set.WriteTo(&b)
	c.body, c.at = b.Bytes(), time.Now()
	return c.body
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

// collect adds library, media, home filesystem and drive metrics. A library
// whose changer cannot be read reports mhvtl_library_up 0 and no live
// gauges; a drive whose statistics cannot be read reports
// mhvtl_drive_stats_up 0.
func (s *Server) collect(set *promtext.Set) {
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	set.Family("mhvtl_config_ok", "Whether device.conf could be read and parsed.", promtext.Gauge).
		Set(boolValue(err == nil))
	if err != nil {
		return
	}
	homes := map[string]bool{}
	for _, lib := range inv.Libraries {
		s.collectLibrary(set, lib)
		homes[s.homeFor(lib)] = true
	}
	// Libraries may share a home directory; count its media once.
	for _, home := range sortedKeys(homes) {
		collectHome(set, home)
	}
}

func (s *Server) collectLibrary(set *promtext.Set, lib config.Library) {
	id := strconv.Itoa(lib.ID)
	set.Family("mhvtl_library_info", "Library identity from device.conf; always 1.", promtext.Gauge).
		Set(1, "library", id, "vendor", lib.Vendor, "product", lib.Product, "serial", lib.Serial, "home", s.homeFor(lib))
	set.Family("mhvtl_library_drives", "Drives configured in the library.", promtext.Gauge).
		Set(float64(len(lib.Drives)), "library", id)

	up := set.Family("mhvtl_library_up", "Whether the library's changer answered READ ELEMENT STATUS.", promtext.Gauge)
	if s.cfg.Changers == nil {
		up.Set(0, "library", id)
	} else if live, err := s.cfg.Changers.Inventory(lib); err != nil {
		up.Set(0, "library", id)
	} else {
		up.Set(1, "library", id)
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

	if s.cfg.Drives == nil {
		return
	}
	for _, d := range lib.Drives {
		collectDrive(set, id, d, s.cfg.Drives)
	}
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

// collectHome adds the media and filesystem metrics of one home directory.
func collectHome(set *promtext.Set, home string) {
	ok := set.Family("mhvtl_home_ok", "Whether the media home directory could be listed.", promtext.Gauge)
	carts, err := media.List(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		ok.Set(0, "home", home)
		return
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
}

// readyCheck is one check of GET /readyz.
type readyCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleReadyz serves GET /readyz: 200 when device.conf parses, every
// library's changer answers and no attached raw cartridge has an unreadable
// superblock; 503 otherwise. The body lists each check. The message queue
// is not checked: the queue client has no request that is free of side
// effects.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	var checks []readyCheck
	add := func(name string, err error) {
		c := readyCheck{Name: name, OK: err == nil}
		if err != nil {
			c.Error = err.Error()
		}
		checks = append(checks, c)
	}
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	add("config", err)
	if err == nil {
		homes := map[string]bool{}
		for _, lib := range inv.Libraries {
			homes[s.homeFor(lib)] = true
			if s.cfg.Changers != nil {
				_, err := s.cfg.Changers.Inventory(lib)
				add("changer:"+strconv.Itoa(lib.ID), err)
			}
		}
		for _, home := range sortedKeys(homes) {
			add("media:"+home, checkRawMedia(home))
		}
	}
	status, code := "ok", http.StatusOK
	for _, c := range checks {
		if !c.OK {
			status, code = "fail", http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

// checkRawMedia fails when home cannot be listed, or when a raw cartridge
// whose disk is attached has an unreadable superblock or MAM. A detached
// disk is not a failure: bays are removable.
func checkRawMedia(home string) error {
	carts, err := media.List(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
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
