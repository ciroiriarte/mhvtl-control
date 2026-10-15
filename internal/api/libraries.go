package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
	"mhvtld/internal/ipc"
	"mhvtld/internal/scsi"
)

// slotDTO is a storage slot or MAP (mail) slot. Without live changer state,
// Barcode is the configured initial content from library_contents.<id> and
// Full is omitted; with it, both come from READ ELEMENT STATUS.
type slotDTO struct {
	Number  int    `json:"number"`
	Barcode string `json:"barcode"`
	Full    *bool  `json:"full,omitempty"`
}

// driveDTO is a virtual tape drive within a library. Slot is the drive's
// position in the library (its data transfer element number, from
// "Library ID: <lib> Slot: <n>"), not a storage slot. Loaded, Barcode,
// SourceSlot and SourceMAP are present only when the library is live.
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
	Loaded             *bool  `json:"loaded,omitempty"`
	Barcode            string `json:"barcode,omitempty"`
	SourceSlot         int    `json:"source_slot,omitempty"`
	SourceMAP          int    `json:"source_map,omitempty"`
}

// libraryDTO is a virtual medium changer with its drives, slots, and maps.
// Live reports whether drives/slots/maps reflect the changer's live element
// status; when false, LiveError says why and the view is config-derived.
type libraryDTO struct {
	ID        int        `json:"id"`
	Vendor    string     `json:"vendor"`
	Product   string     `json:"product"`
	Serial    string     `json:"serial"`
	Channel   int        `json:"channel"`
	Target    int        `json:"target"`
	LUN       int        `json:"lun"`
	NAA       string     `json:"naa"`
	HomeDir   string     `json:"home_dir"`
	Live      bool       `json:"live"`
	LiveError string     `json:"live_error,omitempty"`
	Drives    []driveDTO `json:"drives"`
	Slots     []slotDTO  `json:"slots"`
	MAPs      []slotDTO  `json:"maps"`
	Pickers   []int      `json:"pickers"`
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

// applyLive overlays a live changer inventory: drive load state by drive
// position, and slots/MAP replaced by the changer's view.
func applyLive(dto *libraryDTO, inv changer.Inventory) {
	dto.Live = true
	for i := range dto.Drives {
		d := &dto.Drives[i]
		if d.Slot < 1 || d.Slot > len(inv.Drives) {
			continue
		}
		st := inv.Drives[d.Slot-1]
		loaded := st.Full
		d.Loaded = &loaded
		d.Barcode, d.SourceSlot, d.SourceMAP = st.Barcode, st.SourceSlot, st.SourceMAP
	}
	dto.Slots = liveSlots(inv.Slots)
	dto.MAPs = liveSlots(inv.MAPs)
}

func liveSlots(in []changer.SlotState) []slotDTO {
	out := make([]slotDTO, 0, len(in))
	for _, sl := range in {
		full := sl.Full
		out = append(out, slotDTO{Number: sl.Number, Barcode: sl.Barcode, Full: &full})
	}
	return out
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
		dto := toLibraryDTO(l)
		if s.cfg.Changers == nil {
			dto.LiveError = "changer access not configured"
		} else if live, err := s.cfg.Changers.Inventory(l); err != nil {
			dto.LiveError = err.Error()
		} else {
			applyLive(&dto, live)
		}
		libs = append(libs, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

// lookupLibrary resolves the {id} path value against device.conf, writing the
// error response itself when it fails.
func (s *Server) lookupLibrary(w http.ResponseWriter, r *http.Request) (config.Library, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "library id must be an integer")
		return config.Library{}, false
	}
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("library %d not found (no device.conf)", id))
		} else {
			writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		}
		return config.Library{}, false
	}
	for _, l := range inv.Libraries {
		if l.ID == id {
			return l, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("library %d not found", id))
	return config.Library{}, false
}

// hasDrive reports whether drive id belongs to lib.
func hasDrive(lib config.Library, id int) bool {
	for _, d := range lib.Drives {
		if d.ID == id {
			return true
		}
	}
	return false
}

// writeChangerError maps a changer failure to an HTTP response: an
// unreachable changer is 503, a request the changer's state refuses (sense
// NOT READY, or ILLEGAL REQUEST for an empty source, a full destination or an
// invalid element) is 409, a drive that refused the cartridge is 409 too,
// anything else is 500.
func writeChangerError(w http.ResponseWriter, op string, err error) {
	var se *scsi.SenseError
	var te *scsi.TransportError
	switch {
	case errors.Is(err, changer.ErrUnavailable), errors.As(err, &te):
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", err.Error())
	case errors.Is(err, changer.ErrUnknownDrive), errors.Is(err, changer.ErrOutOfRange):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, changer.ErrDriveEmpty), errors.Is(err, changer.ErrNoEmptySlot):
		writeError(w, http.StatusConflict, "conflict", op+": "+err.Error())
	case errors.As(err, &se) && conflictSense(se):
		writeError(w, http.StatusConflict, "conflict", op+": "+se.Error())
	case errors.As(err, &se) && se.Key == scsi.KeyHardwareError && se.Code() == scsi.ASCManualIntervention:
		// mhVTL's changer reports a drive that failed to load or unload the
		// cartridge (wrong media type for the drive, missing or damaged
		// media) as HARDWARE ERROR, manual intervention required.
		writeError(w, http.StatusConflict, "conflict", op+": the drive refused the cartridge "+
			"(check that the media type suits the drive and that the media exists): "+se.Error())
	default:
		writeError(w, http.StatusInternalServerError, "scsi_error", op+": "+err.Error())
	}
}

func conflictSense(se *scsi.SenseError) bool {
	if se.Key == scsi.KeyNotReady {
		return true
	}
	if se.Key != scsi.KeyIllegalRequest {
		return false
	}
	switch se.Code() {
	case scsi.ASCMediumSrcEmpty, scsi.ASCMediumDestFull, scsi.ASCInvalidElement, scsi.ASCMediumRemovalPrevnt:
		return true
	}
	return false
}

// mountRequest selects the cartridge for a drive: a storage slot or a MAP
// slot (both 1-based). The pre-changer shape {drive, slot} is still valid.
type mountRequest struct {
	Drive int `json:"drive"`
	Slot  int `json:"slot"`
	MAP   int `json:"map"`
}

func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	var req mountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch {
	case req.Slot > 0 && req.MAP > 0:
		writeError(w, http.StatusBadRequest, "bad_request", "give either slot or map, not both")
		return
	case req.Slot <= 0 && req.MAP <= 0:
		writeError(w, http.StatusBadRequest, "bad_request", "a positive slot or map number is required")
		return
	case !hasDrive(lib, req.Drive):
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("drive %d is not in library %d", req.Drive, lib.ID))
		return
	}
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "changer access not configured")
		return
	}
	mv, err := s.cfg.Changers.Mount(lib, req.Drive, changer.Source{Slot: req.Slot, MAP: req.MAP})
	if err != nil {
		writeChangerError(w, "mount", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "move": mv})
}

// unmountRequest names the drive to empty and, optionally, the storage slot
// to put its cartridge in (default: where it came from, else the first empty
// slot).
type unmountRequest struct {
	Drive int `json:"drive"`
	Slot  int `json:"slot"`
}

func (s *Server) handleUnmount(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	var req unmountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch {
	case req.Slot < 0:
		writeError(w, http.StatusBadRequest, "bad_request", "slot must be positive when given")
		return
	case !hasDrive(lib, req.Drive):
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("drive %d is not in library %d", req.Drive, lib.ID))
		return
	}
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "changer access not configured")
		return
	}
	mv, err := s.cfg.Changers.Unmount(lib, req.Drive, req.Slot)
	if err != nil {
		writeChangerError(w, "unmount", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "move": mv})
}

// barcodeRE bounds an imported barcode to MAX_BARCODE_LEN (16) alphanumerics.
// vtllibrary takes everything from the first alphanumeric to the end of the
// message as the barcode, so spaces or other text must never reach it.
var barcodeRE = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

type mapImportRequest struct {
	Barcode string `json:"barcode"`
}

// handleMapImport places a cartridge into an empty MAP slot of the library
// (vtllibrary "open map" / "load map <barcode>" / "close map"). The
// cartridge can then be mounted with {drive, map}.
func (s *Server) handleMapImport(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	var req mapImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !barcodeRE.MatchString(req.Barcode) {
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	}
	if s.cfg.Controller == nil {
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", "mhvtl control queue not configured")
		return
	}
	// The library accepts any barcode into the MAP; a cartridge with no media
	// only fails once a drive tries to load it. Refuse it up front, as vtlcmd
	// does. A cartridge is <home>/<barcode>: a directory of stream files, or a
	// symlink to a raw block device.
	if lib.HomeDir != "" {
		if _, err := os.Stat(filepath.Join(lib.HomeDir, req.Barcode)); err != nil {
			writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
				"no media for %s in %s (create it with mktape first)", req.Barcode, lib.HomeDir))
			return
		}
	}
	err := s.cfg.Controller.ImportMap(int64(lib.ID), req.Barcode)
	var re *ipc.ReplyError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case errors.Is(err, ipc.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", err.Error())
	case errors.Is(err, ipc.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, "ipc_timeout", fmt.Sprintf("library %d did not reply: %v", lib.ID, err))
	case errors.As(err, &re):
		writeError(w, http.StatusConflict, "conflict", "MAP import refused: "+re.Error())
	default:
		writeError(w, http.StatusInternalServerError, "control_error", err.Error())
	}
}
