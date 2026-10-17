package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
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
	NAA                string `json:"naa"`
	Channel            int    `json:"channel"`
	Target             int    `json:"target"`
	LUN                int    `json:"lun"`
	Slot               int    `json:"slot"`
	CompressionEnabled bool   `json:"compression_enabled"`
	CompressionType    string `json:"compression_type"`
	CompressionFactor  int    `json:"compression_factor"`
	Loaded             *bool  `json:"loaded,omitempty"`
	Barcode            string `json:"barcode,omitempty"`
	SourceSlot         int    `json:"source_slot,omitempty"`
	SourceMAP          int    `json:"source_map,omitempty"`
	// Nodes are the drive's device nodes, when it is found on this host.
	Nodes *changer.Nodes `json:"nodes,omitempty"`
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
	// MAPOpen is the MAP door state, live libraries with a MAP only.
	MAPOpen *bool `json:"map_open,omitempty"`
	// Nodes are the changer's device nodes, when it is found on this host.
	Nodes *changer.Nodes `json:"nodes,omitempty"`
}

func toDriveDTO(d config.Drive) driveDTO {
	return driveDTO{
		ID:                 d.ID,
		Vendor:             d.Vendor,
		Product:            d.Product,
		Revision:           d.Revision,
		Serial:             d.Serial,
		NAA:                d.NAA,
		Channel:            d.Channel,
		Target:             d.Target,
		LUN:                d.LUN,
		Slot:               d.Slot,
		CompressionEnabled: d.CompressionEnabled,
		CompressionType:    d.CompressionType,
		CompressionFactor:  d.CompressionFactor,
	}
}

func toLibraryDTO(l config.Library) libraryDTO {
	drives := make([]driveDTO, 0, len(l.Drives))
	for _, d := range l.Drives {
		drives = append(drives, toDriveDTO(d))
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
	dto.MAPOpen = inv.MAPOpen
}

// applyNodes adds the device nodes of lib's changer and drives. A device
// that isn't found (mhVTL stopped) is left without nodes; other failures,
// such as an override that isn't an sg node, are logged too.
func (s *Server) applyNodes(dto *libraryDTO, lib config.Library) {
	if s.cfg.Nodes == nil {
		return
	}
	if n, err := s.cfg.Nodes.LibraryNodes(lib); err == nil {
		dto.Nodes = &n
	} else if !errors.Is(err, changer.ErrUnavailable) {
		log.Printf("libraries: library %d nodes: %v", lib.ID, err)
	}
	for i, d := range lib.Drives {
		if n, err := s.cfg.Nodes.DriveNodes(d); err == nil {
			dto.Drives[i].Nodes = &n
		} else if !errors.Is(err, changer.ErrUnavailable) {
			log.Printf("libraries: drive %d nodes: %v", d.ID, err)
		}
	}
}

func liveSlots(in []changer.SlotState) []slotDTO {
	out := make([]slotDTO, 0, len(in))
	for _, sl := range in {
		full := sl.Full
		out = append(out, slotDTO{Number: sl.Number, Barcode: sl.Barcode, Full: &full})
	}
	return out
}

func (s *Server) handleLibraries(w http.ResponseWriter, r *http.Request) {
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
	live, liveErrs := s.liveInventories(r.Context(), inv.Libraries)
	libs := make([]libraryDTO, 0, len(inv.Libraries))
	for _, l := range inv.Libraries {
		dto := toLibraryDTO(l)
		if inv, ok := live[l.ID]; ok {
			applyLive(&dto, inv)
		} else {
			dto.LiveError = liveErrs[l.ID]
		}
		s.applyNodes(&dto, l)
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
// invalid element) is 409, a drive that refused the cartridge or cannot use
// its media type is 409 too, anything else is 500.
func writeChangerError(w http.ResponseWriter, op string, err error) {
	var se *scsi.SenseError
	var te *scsi.TransportError
	switch {
	case errors.Is(err, changer.ErrUnavailable), errors.As(err, &te):
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", err.Error())
	case errors.Is(err, changer.ErrUnknownDrive), errors.Is(err, changer.ErrOutOfRange), errors.Is(err, changer.ErrBadLocation),
		errors.Is(err, changer.ErrNoMAP):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, changer.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", op+": "+err.Error())
	case errors.Is(err, changer.ErrDriveEmpty), errors.Is(err, changer.ErrNoEmptySlot),
		errors.Is(err, changer.ErrMAPFull), errors.Is(err, changer.ErrMAPOpen):
		writeError(w, http.StatusConflict, "conflict", op+": "+err.Error())
	case errors.As(err, &se) && conflictSense(se):
		writeError(w, http.StatusConflict, "conflict", op+": "+se.Error())
	// mhVTL's changer reports a MOVE MEDIUM whose load into a drive failed
	// as 05/30/00 when the drive cannot use the media type and 04/53/00
	// otherwise, from markh794/mhvtl#285 on (load_failed_sense,
	// usr/smc.c:1182-1192 at 852b78a,
	// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/smc.c#L1182-L1192,
	// after GC27-2211-00 Table 68, p.58). Before #285 it reported every
	// failed load as 04/04/03, which it still reports for a failed unload
	// (usr/smc.c:1475-1481 at 852b78a).
	case errors.As(err, &se) && se.Key == scsi.KeyIllegalRequest && se.Code() == scsi.ASCIncompatibleMedium:
		writeError(w, http.StatusConflict, "conflict", op+": the drive cannot use this cartridge's media type: "+se.Error())
	case errors.As(err, &se) && se.Key == scsi.KeyHardwareError && se.Code() == scsi.ASCMediaLoadFailed:
		writeError(w, http.StatusConflict, "conflict", op+": the drive refused the load "+
			"(check that the media exists and is not damaged): "+se.Error())
	case errors.As(err, &se) && se.Key == scsi.KeyHardwareError && se.Code() == scsi.ASCManualIntervention:
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
	defer s.lockLibrary(lib.ID)()
	var req mountRequest
	if err := decodeJSON(w, r, &req); err != nil {
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
	defer s.lockLibrary(lib.ID)()
	var req unmountRequest
	if err := decodeJSON(w, r, &req); err != nil {
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
