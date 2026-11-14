package api

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/transports"
)

// transportsDTO is GET /api/v1/transports: the LIO configuration
// (transports.Map) with each mhVTL backstore's device.conf device, and the
// transports that export something.
type transportsDTO struct {
	transports.Map
	ActiveTransports []string `json:"active_transports"`
}

// handleTransports reads configfs; it never fails. Without configfs the
// map is empty, and what could not be read is named in warnings.
func (s *Server) handleTransports(w http.ResponseWriter, _ *http.Request) {
	m := transports.Read(s.cfg.TargetRoot)
	if len(m.Backstores) > 0 {
		inv, err := config.LoadInventory(s.cfg.ConfigDir)
		if err != nil {
			// The error names the absolute path; the log keeps it.
			log.Printf("transports: %v", err)
			m.Warnings = append(m.Warnings, "device.conf: unreadable")
		} else {
			m.Resolve(inv)
		}
	}
	writeJSON(w, http.StatusOK, transportsDTO{Map: m, ActiveTransports: s.activeFrom(m)})
}

// activeTransports is GET /system's active_transports.
func (s *Server) activeTransports() []string {
	return s.activeWith(transports.Active(s.cfg.TargetRoot))
}

// activeFrom is activeWith for a map already read.
func (s *Server) activeFrom(m transports.Map) []string {
	return s.activeWith(m.Active())
}

// activeWith is local while mhvtl.ko (the pseudo HBA) is loaded, then the
// exporting transports (transports.Active).
func (s *Server) activeWith(exporting []string) []string {
	out := []string{}
	kmod := s.kmodLoaded()
	if kmod {
		out = append(out, "local")
	}
	for _, t := range exporting {
		if t != "local" || !kmod {
			out = append(out, t)
		}
	}
	return out
}

// sysRoot is the sysfs mount point: cfg.SysRoot, /sys when unset.
func (s *Server) sysRoot() string {
	if s.cfg.SysRoot != "" {
		return s.cfg.SysRoot
	}
	return "/sys"
}

// kmodLoaded is whether mhvtl.ko (the pseudo HBA) is loaded.
func (s *Server) kmodLoaded() bool {
	_, err := os.Stat(filepath.Join(s.sysRoot(), "module", hostids.MhvtlDriver))
	return err == nil
}
