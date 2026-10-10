// Package api implements the mhvtld REST API.
package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mhvtld/internal/ipc"
	"mhvtld/internal/rawdisk"
)

var startTime = time.Now()

// Config holds the injected dependencies for a Server.
type Config struct {
	// ConfigDir is the directory holding device.conf and library_contents.<id>.
	ConfigDir string
	// Controller drives library/drive load/unload over IPC. Nil means the mhvtl
	// control queue is unavailable; mount/unmount then return 503.
	Controller ipc.Controller
	// DiskOptions controls raw-disk enumeration for GET /storage/disks.
	DiskOptions rawdisk.Options
	// Formatter initializes raw disks. Nil means formatting is unavailable (503).
	Formatter rawdisk.Formatter
	// Token, when non-empty, requires a bearer token on /api/v1/* requests.
	Token string
	// CORSOrigin, when non-empty, enables CORS with this allowed origin.
	CORSOrigin string
	// StaticFS, when non-nil, serves the web UI at "/" and "/static/".
	StaticFS fs.FS
}

// Server is the dependency-injected API server.
type Server struct {
	cfg Config
}

// NewServer returns a Server configured with cfg.
func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Handler builds the route mux and wraps it in the middleware chain
// (recover -> log -> cors -> auth), returning the wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/system", handleSystem)
	mux.HandleFunc("GET /api/v1/libraries", s.handleLibraries)
	mux.HandleFunc("POST /api/v1/libraries/{id}/mount", s.handleMount)
	mux.HandleFunc("POST /api/v1/libraries/{id}/unmount", s.handleUnmount)
	mux.HandleFunc("GET /api/v1/storage/disks", s.handleDisks)
	mux.HandleFunc("POST /api/v1/storage/disks/format", s.handleFormat)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	if s.cfg.StaticFS != nil {
		mux.Handle("GET /static/", http.FileServerFS(s.cfg.StaticFS))
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, s.cfg.StaticFS, "index.html")
		})
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mhvtld"))
		})
	}

	var h http.Handler = mux
	h = s.auth(h)
	h = s.cors(h)
	h = logging(h)
	h = recoverMW(h)
	return h
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// System describes the host and daemon state.
type System struct {
	Hostname         string     `json:"hostname"`
	OS               string     `json:"os"`
	KernelVersion    string     `json:"kernel_version"`
	Architecture     string     `json:"architecture"`
	CPUCount         int        `json:"cpu_count"`
	MemTotalBytes    uint64     `json:"mem_total_bytes"`
	MemAvailBytes    uint64     `json:"mem_available_bytes"`
	LoadAverage      [3]float64 `json:"load_average"`
	UptimeSeconds    float64    `json:"uptime_seconds"`
	ActiveTransports []string   `json:"active_transports"`
}

func handleSystem(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(collectSystem())
}

func collectSystem() System {
	s := System{
		CPUCount:         runtime.NumCPU(),
		UptimeSeconds:    time.Since(startTime).Seconds(),
		ActiveTransports: []string{"local"},
	}
	s.Hostname, _ = os.Hostname()
	s.OS, s.KernelVersion, s.Architecture = uname()
	s.MemTotalBytes, s.MemAvailBytes = meminfo()
	s.LoadAverage = loadavg()
	return s
}

func utsString(a [65]int8) string {
	b := make([]byte, 0, len(a))
	for _, c := range a {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func uname() (osName, release, machine string) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return
	}
	return utsString(u.Sysname), utsString(u.Release), utsString(u.Machine)
}

func meminfo() (total, avail uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	return
}

func loadavg() (l [3]float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	f := strings.Fields(string(data))
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return
}
