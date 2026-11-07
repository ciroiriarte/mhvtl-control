package api

import (
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
			m.Warnings = append(m.Warnings, "device.conf: "+err.Error())
		} else {
			m.Resolve(inv)
		}
	}
	writeJSON(w, http.StatusOK, transportsDTO{Map: m, ActiveTransports: s.activeFrom(m)})
}

// activeTransports is GET /system's active_transports.
func (s *Server) activeTransports() []string {
	return s.activeFrom(transports.Read(s.cfg.TargetRoot))
}

// activeFrom is local while mhvtl.ko (the pseudo HBA) is loaded,
// then the transports m exports through.
func (s *Server) activeFrom(m transports.Map) []string {
	sysRoot := s.cfg.SysRoot
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	out := []string{}
	_, err := os.Stat(filepath.Join(sysRoot, "module", hostids.MhvtlDriver))
	kmod := err == nil
	if kmod {
		out = append(out, "local")
	}
	for _, t := range m.Active() {
		if t != "local" || !kmod {
			out = append(out, t)
		}
	}
	return out
}
