package api

import (
	"fmt"
	"net/http"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/media"
)

// mapImportRequest names the cartridge to put into the MAP. Store moves it on
// to the first empty storage slot.
type mapImportRequest struct {
	Barcode string `json:"barcode"`
	Store   bool   `json:"store"`
}

// handleMapImport places a cartridge into an empty MAP slot of the library
// (vtllibrary "open map" / "load map <barcode>" / "close map"). The
// cartridge can then be mounted with {drive, map}, or with store=true it is
// moved to a storage slot straight away.
func (s *Server) handleMapImport(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	defer s.lockLibrary(lib.ID)()
	var req mapImportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// vtllibrary takes everything from the first alphanumeric to the end of
	// the message as the barcode, so spaces or other text must never reach it.
	if !media.ValidBarcode(req.Barcode) {
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	}
	if s.cfg.Controller == nil {
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", "mhvtl control queue not configured")
		return
	}
	if req.Store && s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "changer access not configured")
		return
	}
	// The library accepts any barcode into the MAP; a cartridge with no media
	// only fails once a drive tries to load it. Refuse it up front, as vtlcmd
	// does. A cartridge is <home>/<barcode>: a directory of stream files, or a
	// symlink to a raw block device.
	if home := s.homeFor(lib); !media.Exists(home, req.Barcode) {
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
			"no media for %s in %s (create it first)", req.Barcode, home))
		return
	}
	// vtllibrary refuses a barcode already in the library, but matches it as
	// a prefix of the barcodes it holds; check the live view for an exact
	// match first so the refusal can say where the cartridge is.
	if s.cfg.Changers != nil {
		if inv, err := s.cfg.Changers.Inventory(lib); err == nil {
			if loc, ok := inv.Locate(req.Barcode); ok {
				writeError(w, http.StatusConflict, "conflict", fmt.Sprintf("%s is already in the library (%s %d)", req.Barcode, loc.Kind, loc.Number))
				return
			}
		}
	}
	if err := s.cfg.Controller.ImportMap(int64(lib.ID), req.Barcode); err != nil {
		writeIPCError(w, "MAP import", lib.ID, err)
		return
	}
	if !req.Store {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "imported": true})
		return
	}
	// The import has happened whatever the store does next, so a failed
	// store is reported alongside it rather than as a failed request.
	resp := map[string]any{"status": "ok", "imported": true}
	if mv, err := s.cfg.Changers.Store(lib, req.Barcode); err != nil {
		resp["store_error"] = fmt.Sprintf("moving %s from the MAP to a slot: %v (it is left in the MAP)", req.Barcode, err)
	} else {
		resp["move"] = mv
	}
	writeJSON(w, http.StatusOK, resp)
}

// mapExportRequest names the cartridge to take to the MAP: the one in storage
// slot Slot, in drive Drive (a drive id), or wherever Barcode is.
type mapExportRequest struct {
	Slot    int    `json:"slot"`
	Drive   int    `json:"drive"`
	Barcode string `json:"barcode"`
}

// handleMapExport moves a cartridge to the first empty MAP slot.
func (s *Server) handleMapExport(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	defer s.lockLibrary(lib.ID)()
	var req mapExportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	given := 0
	for _, set := range []bool{req.Slot != 0, req.Drive != 0, req.Barcode != ""} {
		if set {
			given++
		}
	}
	switch {
	case given != 1:
		writeError(w, http.StatusBadRequest, "bad_request", "give exactly one of slot, drive or barcode")
		return
	case req.Slot < 0:
		writeError(w, http.StatusBadRequest, "bad_request", "slot must be positive")
		return
	case req.Drive != 0 && !hasDrive(lib, req.Drive):
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("drive %d is not in library %d", req.Drive, lib.ID))
		return
	case req.Barcode != "" && !media.ValidBarcode(req.Barcode):
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	}
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "changer access not configured")
		return
	}
	mv, err := s.cfg.Changers.Export(lib, changer.ExportSource{Slot: req.Slot, Drive: req.Drive, Barcode: req.Barcode})
	if err != nil {
		writeChangerError(w, "export", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "move": mv})
}

// handleMapAction opens or closes the MAP, or ejects everything in it to the
// shelf (the cartridges leave the library; their media stays).
func (s *Server) handleMapAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "open" && action != "close" && action != "eject" {
		writeError(w, http.StatusNotFound, "not_found", "unknown MAP action "+action)
		return
	}
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	if s.cfg.Controller == nil {
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", "mhvtl control queue not configured")
		return
	}
	defer s.lockLibrary(lib.ID)()
	var err error
	resp := map[string]any{"status": "ok"}
	switch action {
	case "open":
		err = s.cfg.Controller.OpenMap(int64(lib.ID))
	case "close":
		err = s.cfg.Controller.CloseMap(int64(lib.ID))
	case "eject":
		// The barcodes are read first so the operator learns what left; the
		// eject itself does not depend on them.
		resp["ejected"] = s.mapBarcodes(lib)
		err = s.cfg.Controller.EjectMap(int64(lib.ID))
	}
	if err != nil {
		writeIPCError(w, "MAP "+action, lib.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// mapBarcodes returns the barcodes in lib's MAP, or nil when the changer
// cannot say.
func (s *Server) mapBarcodes(lib config.Library) []string {
	if s.cfg.Changers == nil {
		return nil
	}
	inv, err := s.cfg.Changers.Inventory(lib)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, m := range inv.MAPs {
		if m.Full {
			out = append(out, m.Barcode)
		}
	}
	return out
}

// moveRequest moves a cartridge between storage and MAP slots.
type moveRequest struct {
	From changer.Location `json:"from"`
	To   changer.Location `json:"to"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	defer s.lockLibrary(lib.ID)()
	var req moveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for _, loc := range []changer.Location{req.From, req.To} {
		if (loc.Kind != "slot" && loc.Kind != "map") || loc.Number <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"from and to must be {kind: slot|map, number: n>0}; use mount and unmount for drives")
			return
		}
	}
	if req.From == req.To {
		writeError(w, http.StatusBadRequest, "bad_request", "from and to are the same element")
		return
	}
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "changer access not configured")
		return
	}
	mv, err := s.cfg.Changers.Transfer(lib, req.From, req.To)
	if err != nil {
		writeChangerError(w, "move", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "move": mv})
}
