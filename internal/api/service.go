package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/chleak"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/ipc"
)

// Values of serviceDTO.Manager.
const (
	managerSystemd = "systemd"
	// managerUnavailable: the apply helper, the daemon's only way to
	// systemd, is absent or did not answer. mhvtl-control has no SysV
	// support (mhvtl-control#14, D4).
	managerUnavailable = "unavailable"
)

// serviceTTL is how long a GET /service answer is reused: each answer
// starts a helper instance for its units op, as hello does (helloTTL).
const serviceTTL = 2 * time.Second

// serviceTimeout bounds one GET /service read, hello and units included:
// the helper's own systemctl call gives up after 5 s (apply.unitsTimeout),
// and the dashboard's poll must not wait on a wedged helper for longer.
var serviceTimeout = 6 * time.Second

// serviceCache holds the last GET /service answer, from when it ended, and
// the read in flight, which every caller that asks meanwhile shares. mu
// is never held across the read.
type serviceCache struct {
	mu     sync.Mutex
	at     time.Time
	dto    serviceDTO
	flight *serviceFlight
}

// serviceFlight is a read in progress; dto is set before done is closed.
type serviceFlight struct {
	done chan struct{}
	dto  serviceDTO
}

// queueExists reports whether mhVTL's message queue exists. It opens the
// queue without creating it, as the controller does
// (ipc.IPCController.open).
var queueExists = func() bool {
	return (&ipc.Client{}).OpenQueue(ipc.QKey(), false) == nil
}

// serviceDTO is GET /api/v1/service: mhVTL's units and backend.
type serviceDTO struct {
	// Manager is systemd when the helper reported the units, else
	// unavailable, with every unit left out.
	Manager string `json:"manager"`
	// Helper and HelperError are capabilitiesDTO's.
	Helper      string     `json:"helper"`
	HelperError string     `json:"helper_error,omitempty"`
	Backend     backendDTO `json:"backend"`
	Queue       queueDTO   `json:"queue"`
	// Target is mhvtl.target; Support the units besides the instances that
	// are PartOf= it.
	Target           *apply.UnitStatus   `json:"target,omitempty"`
	Support          []apply.UnitStatus  `json:"support,omitempty"`
	Libraries        []serviceLibraryDTO `json:"libraries"`
	StandaloneDrives []serviceDriveDTO   `json:"standalone_drives"`
	KernelCh         *chleak.Status      `json:"kernel_ch,omitempty"`
	// Errors say what could not be read; the rest is still reported.
	Errors []string `json:"errors"`
}

// backendDTO is the backend mhvtl.conf selects and what the kernel has
// loaded. mhvtl-load-modules.service loads mhvtl.ko unless MHVTL_BACKEND
// begins with tcmu, then target_core_user and tcm_loop
// (etc/mhvtl-load-modules.service.in:13-17 at ebbda22,
// https://github.com/markh794/mhvtl/blob/ebbda22646e72106b7f476a5672adc8be915bfc9/etc/mhvtl-load-modules.service.in#L13-L17).
// Both may be loaded at once, after a backend change without a reboot.
type backendDTO struct {
	// Configured is MHVTL_BACKEND, "mhvtl" when mhvtl.conf does not set it.
	Configured   string          `json:"configured"`
	KernelModule kernelModuleDTO `json:"kernel_module"`
	TCMU         tcmuDTO         `json:"tcmu"`
}

type kernelModuleDTO struct {
	Loaded bool `json:"loaded"`
	// Refcount is mhvtl.ko's reference count, nil when it is not loaded
	// or reports none.
	Refcount *int `json:"refcount,omitempty"`
}

type tcmuDTO struct {
	TargetCoreUser bool `json:"target_core_user"`
}

type queueDTO struct {
	Exists bool `json:"exists"`
}

type serviceLibraryDTO struct {
	ID     int               `json:"id"`
	Unit   *apply.UnitStatus `json:"unit,omitempty"`
	Drives []serviceDriveDTO `json:"drives"`
	// Persist is device.conf's PERSIST for the library (apply.PersistOn).
	Persist bool `json:"persist"`
	// ChExposed is whether starting or restarting the library may leak
	// references to its drives (chleak.Status.Exposed).
	ChExposed bool `json:"ch_exposed"`
}

type serviceDriveDTO struct {
	ID   int               `json:"id"`
	Unit *apply.UnitStatus `json:"unit,omitempty"`
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	c := &s.service
	c.mu.Lock()
	if !c.at.IsZero() && s.now().Sub(c.at) < serviceTTL {
		dto := c.dto
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, dto)
		return
	}
	f := c.flight
	if f == nil {
		f = &serviceFlight{done: make(chan struct{})}
		c.flight = f
		// Read apart from the request, so a client that goes away
		// neither cancels it for the others nor gets its cancellation
		// cached.
		go s.service1(context.WithoutCancel(r.Context()), f)
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		writeJSON(w, http.StatusOK, f.dto)
	case <-r.Context().Done():
	}
}

// service1 reads the service status once, within serviceTimeout, and
// caches it. A read that panics is not cached, as in hello1: it runs out of
// reach of the HTTP recover middleware, and the flight must end.
func (s *Server) service1(ctx context.Context, f *serviceFlight) {
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	c := &s.service
	defer func() {
		if r := recover(); r != nil {
			log.Printf("GET /service panicked: %v\n%s", r, debug.Stack())
			f.dto = serviceDTO{Manager: managerUnavailable, Libraries: []serviceLibraryDTO{}, StandaloneDrives: []serviceDriveDTO{},
				Errors: []string{"the service status read failed; the daemon log has the cause"}}
			c.mu.Lock()
			c.flight = nil
			c.mu.Unlock()
			close(f.done)
		}
	}()
	f.dto = s.serviceStatus(ctx)
	c.mu.Lock()
	c.dto, c.at, c.flight = f.dto, s.now(), nil
	c.mu.Unlock()
	close(f.done)
}

// serviceStatus reads device.conf, mhvtl.conf and sysfs, and asks the
// helper for the units (apply.OpUnits: lockless and not audited).
func (s *Server) serviceStatus(ctx context.Context) serviceDTO {
	dto := serviceDTO{Libraries: []serviceLibraryDTO{}, StandaloneDrives: []serviceDriveDTO{}, Errors: []string{}}
	fail := func(err error) { dto.Errors = append(dto.Errors, err.Error()) }
	dto.Helper, dto.HelperError = s.helperStatus(ctx, s.cfg.Capabilities.withDefaults())

	sysRoot := s.cfg.SysRoot
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	dto.Backend.Configured = "mhvtl"
	switch b, err := os.ReadFile(filepath.Join(s.cfg.ConfigDir, "mhvtl.conf")); {
	case err == nil:
		if v, ok := conffile.ParseMhvtlConf(b).Get("MHVTL_BACKEND"); ok && v != "" {
			dto.Backend.Configured = v
		}
	case !errors.Is(err, os.ErrNotExist):
		fail(err)
	}
	if _, err := os.Stat(filepath.Join(sysRoot, "module", hostids.MhvtlDriver)); err == nil {
		dto.Backend.KernelModule.Loaded = true
	}
	if _, err := os.Stat(filepath.Join(sysRoot, "module", "target_core_user")); err == nil {
		dto.Backend.TCMU.TargetCoreUser = true
	}
	dto.Queue.Exists = queueExists()

	units := map[string]*apply.UnitStatus{}
	if s.cfg.Helper != nil {
		ctx, cancel := context.WithTimeout(ctx, unitsWait)
		resp, err := s.cfg.Helper.Do(ctx, apply.Request{Op: apply.OpUnits})
		cancel()
		switch {
		case errors.Is(err, applyclient.ErrAbsent):
		case err != nil:
			fail(err)
		case resp.Result != apply.ResultOK:
			fail(errors.New("the apply helper answered units " + resp.Result + ": " + resp.Message))
		default:
			dto.Manager = managerSystemd
			for i := range resp.UnitStates {
				units[resp.UnitStates[i].Name] = &resp.UnitStates[i]
			}
		}
	}
	if dto.Manager == "" {
		dto.Manager = managerUnavailable
	}
	if u := units["mhvtl.target"]; u != nil {
		dto.Target = u
	}
	for _, name := range []string{"mhvtl-load-modules.service", "mhvtl-tcmu-handler.service", "mhvtl-media.service", "mhvtl-bay-monitor.service"} {
		if u := units[name]; u != nil {
			dto.Support = append(dto.Support, *u)
		}
	}

	snap, err := s.readConfig()
	if err != nil {
		fail(err)
	}
	_, release, _ := uname()
	host, hostErr := hostids.Read(sysRoot)
	ch, err := chleak.Check(sysRoot, release, snap.inv.Libraries, host)
	if err = errors.Join(hostErr, err); err != nil {
		fail(err)
	}
	dto.KernelCh = &ch
	dto.Backend.KernelModule.Refcount = ch.ModuleRefcount
	dc := conffile.ParseDeviceConf(snap.files["device.conf"])
	drive := func(id int) serviceDriveDTO {
		return serviceDriveDTO{ID: id, Unit: units[fmt.Sprintf("vtltape@%d.service", id)]}
	}
	for _, lib := range snap.inv.Libraries {
		l := serviceLibraryDTO{ID: lib.ID, Unit: units[fmt.Sprintf("vtllibrary@%d.service", lib.ID)], Drives: []serviceDriveDTO{}, Persist: apply.PersistOn(dc, lib.ID)}
		for _, d := range lib.Drives {
			l.Drives = append(l.Drives, drive(d.ID))
		}
		l.ChExposed = ch.Exposed(lib.ID)
		dto.Libraries = append(dto.Libraries, l)
	}
	for _, d := range snap.inv.Standalone {
		dto.StandaloneDrives = append(dto.StandaloneDrives, drive(d.ID))
	}
	return dto
}
