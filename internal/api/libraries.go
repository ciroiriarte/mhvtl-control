package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"

	"mhvtld/internal/config"
)

// slotDTO is a storage slot or MAP (mail) slot.
type slotDTO struct {
	Number  int    `json:"number"`
	Barcode string `json:"barcode"`
}

// driveDTO is a virtual tape drive within a library.
type driveDTO struct {
	ID                 int    `json:"id"`
	Vendor             string `json:"vendor"`
	Product            string `json:"product"`
	Revision           string `json:"revision"`
	Serial             string `json:"serial"`
	Slot               int    `json:"slot"`
	CompressionEnabled bool   `json:"compression_enabled"`
	CompressionType    string `json:"compression_type"`
	CompressionFactor  int    `json:"compression_factor"`
}

// libraryDTO is a virtual medium changer with its drives, slots, and maps.
type libraryDTO struct {
	ID      int        `json:"id"`
	Vendor  string     `json:"vendor"`
	Product string     `json:"product"`
	Serial  string     `json:"serial"`
	Channel int        `json:"channel"`
	Target  int        `json:"target"`
	LUN     int        `json:"lun"`
	NAA     string     `json:"naa"`
	HomeDir string     `json:"home_dir"`
	Drives  []driveDTO `json:"drives"`
	Slots   []slotDTO  `json:"slots"`
	MAPs    []slotDTO  `json:"maps"`
	Pickers []int      `json:"pickers"`
}

func toLibraryDTO(l config.Library) libraryDTO {
	drives := make([]driveDTO, 0, len(l.Drives))
	for _, d := range l.Drives {
		drives = append(drives, driveDTO{
			ID:                 d.ID,
			Vendor:             d.Vendor,
			Product:            d.Product,
			Revision:           d.Revision,
			Serial:             d.Serial,
			Slot:               d.Slot,
			CompressionEnabled: d.CompressionEnabled,
			CompressionType:    d.CompressionType,
			CompressionFactor:  d.CompressionFactor,
		})
	}
	slots := make([]slotDTO, 0, len(l.Slots))
	for _, sl := range l.Slots {
		slots = append(slots, slotDTO{Number: sl.Number, Barcode: sl.Barcode})
	}
	maps := make([]slotDTO, 0, len(l.MAPs))
	for _, m := range l.MAPs {
		maps = append(maps, slotDTO{Number: m.Number, Barcode: m.Barcode})
	}
	pickers := l.Pickers
	if pickers == nil {
		pickers = []int{}
	}
	return libraryDTO{
		ID:      l.ID,
		Vendor:  l.Vendor,
		Product: l.Product,
		Serial:  l.Serial,
		Channel: l.Channel,
		Target:  l.Target,
		LUN:     l.LUN,
		NAA:     l.NAA,
		HomeDir: l.HomeDir,
		Drives:  drives,
		Slots:   slots,
		MAPs:    maps,
		Pickers: pickers,
	}
}

func (s *Server) handleLibraries(w http.ResponseWriter, _ *http.Request) {
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	if err != nil {
		// A missing device.conf is expected before the daemon is configured:
		// serve an empty list so the UI stays usable. Other parse errors are 500.
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("libraries: device.conf not found under %q; returning empty list", s.cfg.ConfigDir)
			writeJSON(w, http.StatusOK, map[string]any{"libraries": []libraryDTO{}})
			return
		}
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	libs := make([]libraryDTO, 0, len(inv.Libraries))
	for _, l := range inv.Libraries {
		libs = append(libs, toLibraryDTO(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

type mountRequest struct {
	Drive   int    `json:"drive"`
	Slot    int    `json:"slot"`
	Barcode string `json:"barcode"`
}

func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Controller == nil {
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", "mhvtl control queue unavailable")
		return
	}
	var req mountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var err error
	if req.Slot > 0 {
		err = s.cfg.Controller.Load(int64(req.Drive), req.Slot)
	} else {
		err = s.cfg.Controller.LoadMap(int64(req.Drive))
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "control_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type unmountRequest struct {
	Drive int `json:"drive"`
}

func (s *Server) handleUnmount(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Controller == nil {
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", "mhvtl control queue unavailable")
		return
	}
	var req unmountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.cfg.Controller.Unload(int64(req.Drive)); err != nil {
		writeError(w, http.StatusInternalServerError, "control_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
