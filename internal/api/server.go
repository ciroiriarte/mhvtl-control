// Package api implements the mhvtl-control REST API.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/rawdisk"
)

var startTime = time.Now()

// Config holds the injected dependencies for a Server.
type Config struct {
	// ConfigDir is the directory holding device.conf and library_contents.<id>.
	ConfigDir string
	// Controller sends library commands over the mhvtl message queue (MAP
	// import). Nil means queue access is not configured (503).
	Controller ipc.Controller
	// Changers moves cartridges and reads live library state through each
	// library's SCSI medium changer. Nil means changer access is not
	// configured: mount/unmount return 503 and libraries are not live.
	Changers ChangerService
	// Media creates and edits cartridges (mktape, edit_tape). Nil means media
	// changes are unavailable (503); listing still works.
	Media MediaService
	// DefaultHome is the media home directory of a library whose device.conf
	// entry has none and when mhvtl.conf sets no MHVTL_HOME_PATH.
	DefaultHome string
	// Drives reads tape drive statistics. Nil means statistics are
	// unavailable (503).
	Drives DriveStatsService
	// Nodes resolves the device nodes of changers and drives for GET
	// /libraries. Nil means no nodes are reported.
	Nodes NodeService
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
	// Version is the daemon's version, reported by GET /system.
	Version string
	// SysRoot is the sysfs mount point POST /config/validate reads the
	// host's SCSI identities from; empty means /sys.
	SysRoot string
	// Capabilities are the host paths GET /config/capabilities inspects.
	Capabilities CapabilityPaths
	// InsecureListen is whether the daemon listens on a non-loopback address
	// without TLS, where configuration writes are refused (plan 4.1).
	InsecureListen bool
}

// ChangerService is the medium-changer surface the API uses; implemented by
// *changer.Manager.
type ChangerService interface {
	Inventory(lib config.Library) (changer.Inventory, error)
	// TryInventory is Inventory, but returns changer.ErrBusy instead of
	// waiting for an operation in progress.
	TryInventory(lib config.Library) (changer.Inventory, error)
	Mount(lib config.Library, driveID int, from changer.Source) (changer.Move, error)
	Unmount(lib config.Library, driveID, toSlot int) (changer.Move, error)
	Transfer(lib config.Library, from, to changer.Location) (changer.Move, error)
	Export(lib config.Library, from changer.ExportSource) (changer.Move, error)
	Store(lib config.Library, barcode string) (changer.Move, error)
}

// NodeService resolves device nodes; implemented by changer.NodeResolver.
type NodeService interface {
	LibraryNodes(lib config.Library) (changer.Nodes, error)
	DriveNodes(d config.Drive) (changer.Nodes, error)
}

var _ NodeService = changer.NodeResolver{}

// MediaService creates and edits cartridges; implemented by media.Tools.
type MediaService interface {
	Create(ctx context.Context, lib int, home string, s media.Spec) error
	Edit(ctx context.Context, lib int, home, barcode string, c media.Change) error
}

var _ MediaService = media.Tools{}

var _ ChangerService = (*changer.Manager)(nil)

// Server is the dependency-injected API server.
type Server struct {
	cfg Config
	// libs serializes the operations that move cartridges or command a
	// library over the queue, per library id. vtllibrary waits for a drive's
	// load/unload reply by reading any message addressed to the library, so
	// a queue command sent while a move into or out of a drive is in flight
	// would be taken as the drive's reply.
	libs keyedMutex
	// homes serializes media creation per home directory, so the check that a
	// barcode is new and mktape cannot interleave with another creation.
	homes keyedMutex
	// metrics caches the last GET /metrics collection.
	metrics metricsCache
	// requests counts handled requests for GET /metrics.
	requests requestCounter
	// live shares each library's live placement among the configuration
	// routes.
	live liveCache
}

// keyedMutex is a set of mutexes created on first use.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock locks the mutex for key and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// lockLibrary serializes cartridge moves and queue commands for library id.
// Callers that need several libraries lock them in ascending id order.
// Unlocking drops the library's shared live read first, so the views read
// the placement the move left rather than the one before it; a read still
// in progress then serves only those already waiting for it.
func (s *Server) lockLibrary(id int) func() {
	unlock := s.libs.lock(strconv.Itoa(id))
	return func() {
		s.live.forget(id)
		unlock()
	}
}

// maxBody bounds a JSON request body.
const maxBody = 1 << 20

// decodeJSON decodes the request body into v, refusing bodies over maxBody.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v)
}

// NewServer returns a Server configured with cfg.
func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Handler builds the route mux and wraps it in the middleware chain
// (recover -> log -> cors -> auth), returning the wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := &routeMux{ServeMux: http.NewServeMux()}

	mux.HandleFunc("GET /api/v1/system", s.handleSystem)
	mux.HandleFunc("GET /api/v1/libraries", s.handleLibraries)
	mux.HandleFunc("POST /api/v1/libraries/{id}/mount", s.handleMount)
	mux.HandleFunc("POST /api/v1/libraries/{id}/unmount", s.handleUnmount)
	mux.HandleFunc("POST /api/v1/libraries/{id}/map/import", s.handleMapImport)
	mux.HandleFunc("POST /api/v1/libraries/{id}/map/export", s.handleMapExport)
	mux.HandleFunc("POST /api/v1/libraries/{id}/map/{action}", s.handleMapAction)
	mux.HandleFunc("POST /api/v1/libraries/{id}/move", s.handleMove)
	mux.HandleFunc("GET /api/v1/libraries/{id}/media", s.handleMediaList)
	mux.HandleFunc("POST /api/v1/libraries/{id}/media", s.handleMediaCreate)
	mux.HandleFunc("PATCH /api/v1/libraries/{id}/media/{barcode}", s.handleMediaEdit)
	mux.HandleFunc("GET /api/v1/libraries/{id}/drives/{drive}/stats", s.handleDriveStats)
	mux.HandleFunc("GET /api/v1/storage/disks", s.handleDisks)
	mux.HandleFunc("POST /api/v1/storage/disks/format", s.handleFormat)
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	mux.HandleFunc("GET /api/v1/config/capabilities", s.handleConfigCapabilities)
	mux.HandleFunc("GET /api/v1/config/personalities", s.handleConfigPersonalities)
	mux.HandleFunc("GET /api/v1/config/libraries", s.handleConfigLibraries)
	mux.HandleFunc("GET /api/v1/config/libraries/{id}", s.handleConfigLibrary)
	mux.HandleFunc("GET /api/v1/config/libraries/{id}/drives", s.handleConfigDrives)
	mux.HandleFunc("GET /api/v1/config/libraries/{id}/slots", s.handleConfigSlots)
	mux.HandleFunc("POST /api/v1/config/validate", s.handleConfigValidate)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	if s.cfg.StaticFS != nil {
		mux.Handle("GET /static/", http.FileServerFS(s.cfg.StaticFS))
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, s.cfg.StaticFS, "index.html")
		})
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mhvtl-control"))
		})
	}

	var h http.Handler = mux
	h = s.auth(h)
	h = s.cors(h)
	h = logging(h, s.requests.add)
	h = recoverMW(h)
	return h
}

// routeMux is a ServeMux that records each matched route's pattern on the
// logging middleware's statusRecorder, so request metrics are labelled by
// pattern ("POST /api/v1/libraries/{id}/mount"), not by path. (Go 1.23's
// Request.Pattern would do this, but go.mod targets 1.22.)
type routeMux struct {
	*http.ServeMux
}

func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.ServeMux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := w.(*statusRecorder); ok {
			rec.route = pattern
		}
		h.ServeHTTP(w, r)
	}))
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
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
	Version          string     `json:"version"`
}

func (s *Server) handleSystem(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	sys := collectSystem()
	sys.Version = s.cfg.Version
	_ = json.NewEncoder(w).Encode(sys)
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
