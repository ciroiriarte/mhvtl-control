package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/media"
)

// maxCreate bounds one media creation request.
const maxCreate = 500

// maxSizeMB bounds a requested capacity: mktape and edit_tape multiply it by
// 2^20 in 64 bits.
const maxSizeMB = 1 << 32

// defaultHome is mhVTL's compiled-in media home (MHVTL_HOME_PATH).
const defaultHome = "/var/lib/mhvtl"

// homeFor returns the directory holding lib's media, resolved the way mhVTL
// does (find_media_home_directory): the library's "Home directory" in
// device.conf, else MHVTL_HOME_PATH from mhvtl.conf as the shell reads it
// (conffile, as default_home_effective), else the default.
func (s *Server) homeFor(lib config.Library) string {
	if lib.HomeDir != "" {
		return lib.HomeDir
	}
	if data, err := os.ReadFile(filepath.Join(s.cfg.ConfigDir, mhvtlConf)); err == nil {
		if v, ok := conffile.ParseMhvtlConf(data).Get("MHVTL_HOME_PATH"); ok && v != "" {
			return v
		}
	}
	if s.cfg.DefaultHome != "" {
		return s.cfg.DefaultHome
	}
	return defaultHome
}

// locationDTO is where a cartridge is: a storage slot, MAP slot or drive
// position, or "shelf" when it is not in the library.
type locationDTO struct {
	Kind   string `json:"kind"`
	Number int    `json:"number,omitempty"`
}

type cartridgeDTO struct {
	media.Cartridge
	// Location is nil when the library's live state is unavailable.
	Location *locationDTO `json:"location,omitempty"`
}

func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	home := s.homeFor(lib)
	carts, err := media.List(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "media_error", err.Error())
		return
	}
	resp := map[string]any{
		"home_dir":  home,
		"densities": media.Densities,
		"types":     media.Types,
		"live":      false,
	}
	var inv *changer.Inventory
	if live, err := s.liveInventory(r.Context(), lib); err != nil {
		resp["live_error"] = err.Error()
	} else {
		inv, resp["live"] = &live, true
	}
	out := make([]cartridgeDTO, 0, len(carts))
	seen := map[string]bool{}
	for _, c := range carts {
		seen[c.Barcode] = true
		out = append(out, cartridgeDTO{Cartridge: c, Location: locate(inv, c.Barcode)})
	}
	// A barcode the library holds without media (library_contents names it,
	// mktape never ran) fails on load; list it so the operator sees why.
	if inv != nil {
		for _, bc := range inventoryBarcodes(*inv) {
			if !seen[bc] {
				seen[bc] = true
				out = append(out, cartridgeDTO{
					Cartridge: media.Cartridge{Barcode: bc, Error: "in the library but has no media in " + home},
					Location:  locate(inv, bc),
				})
			}
		}
	}
	resp["cartridges"] = out
	writeJSON(w, http.StatusOK, resp)
}

func locate(inv *changer.Inventory, barcode string) *locationDTO {
	if inv == nil {
		return nil
	}
	if loc, ok := inv.Locate(barcode); ok {
		return &locationDTO{Kind: loc.Kind, Number: loc.Number}
	}
	return &locationDTO{Kind: "shelf"}
}

func inventoryBarcodes(inv changer.Inventory) []string {
	var out []string
	for _, d := range inv.Drives {
		if d.Full && d.Barcode != "" {
			out = append(out, d.Barcode)
		}
	}
	for _, sl := range append(append([]changer.SlotState{}, inv.Slots...), inv.MAPs...) {
		if sl.Full && sl.Barcode != "" {
			out = append(out, sl.Barcode)
		}
	}
	return out
}

// mediaCreateRequest names one barcode, or a range prefix + number + suffix
// with the number zero-padded to Width digits (default: the digits of the
// last number). Place puts each new cartridge into the library: through the
// MAP into the first empty storage slot.
type mediaCreateRequest struct {
	Barcode string `json:"barcode"`
	Prefix  string `json:"prefix"`
	Start   int    `json:"start"`
	Count   int    `json:"count"`
	Width   int    `json:"width"`
	Suffix  string `json:"suffix"`
	Type    string `json:"type"`
	Density string `json:"density"`
	SizeMB  uint64 `json:"size_mb"`
	Place   bool   `json:"place"`
}

// barcodes expands the request into the barcodes to create.
func (req mediaCreateRequest) barcodes() ([]string, error) {
	if req.Barcode != "" {
		if req.Prefix != "" || req.Suffix != "" || req.Count != 0 {
			return nil, errors.New("give either barcode or a range (prefix, start, count, suffix), not both")
		}
		if !media.ValidBarcode(req.Barcode) {
			return nil, errors.New("barcode must be 1-16 letters or digits")
		}
		return []string{req.Barcode}, nil
	}
	switch {
	case req.Count < 1 || req.Count > maxCreate:
		return nil, fmt.Errorf("count must be 1-%d", maxCreate)
	case req.Start < 0:
		return nil, errors.New("start must not be negative")
	case req.Width < 0 || req.Width > 16:
		return nil, errors.New("width must be 0-16")
	}
	width := max(req.Width, len(strconv.Itoa(req.Start+req.Count-1)))
	out := make([]string, 0, req.Count)
	for i := range req.Count {
		bc := fmt.Sprintf("%s%0*d%s", req.Prefix, width, req.Start+i, req.Suffix)
		if !media.ValidBarcode(bc) {
			return nil, fmt.Errorf("barcode %q is not 1-16 letters or digits", bc)
		}
		out = append(out, bc)
	}
	return out, nil
}

// placeResult reports the cartridges placed into the library and, when
// placing stopped early, why.
type placeResult struct {
	Placed []changer.Move `json:"placed"`
	Error  string         `json:"error,omitempty"`
}

func (s *Server) handleMediaCreate(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	var req mediaCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Type == "" {
		req.Type = "data"
	}
	bcs, err := req.barcodes()
	switch {
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case !media.ValidType(req.Type):
		writeError(w, http.StatusBadRequest, "bad_request", "type must be one of "+strings.Join(media.Types, ", "))
		return
	case !media.ValidDensity(req.Density):
		writeError(w, http.StatusBadRequest, "bad_request", "density must be one of "+strings.Join(media.Densities, ", "))
		return
	case req.SizeMB > maxSizeMB:
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("size_mb must be at most %d", uint64(maxSizeMB)))
		return
	}
	if s.cfg.Media == nil {
		writeError(w, http.StatusServiceUnavailable, "media_unavailable", "media tools not configured")
		return
	}
	if req.Place && (s.cfg.Changers == nil || s.cfg.Controller == nil) {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable", "placing media needs changer and control queue access")
		return
	}
	home := s.homeFor(lib)
	defer s.homes.lock(home)()
	if req.Place {
		defer s.lockLibrary(lib.ID)()
	}
	var existing []string
	for _, bc := range bcs {
		if media.Exists(home, bc) {
			existing = append(existing, bc)
		}
	}
	if len(existing) > 0 {
		writeError(w, http.StatusConflict, "conflict", "media already exists for "+strings.Join(existing, ", "))
		return
	}
	created := make([]string, 0, len(bcs))
	for _, bc := range bcs {
		spec := media.Spec{Barcode: bc, Type: req.Type, Density: req.Density, SizeMB: req.SizeMB}
		if err := s.cfg.Media.Create(r.Context(), lib.ID, home, spec); err != nil {
			msg := "mktape " + bc + ": " + err.Error()
			if len(created) > 0 {
				msg += " (created before the failure: " + strings.Join(created, ", ") + ")"
			}
			writeError(w, http.StatusInternalServerError, "tool_error", msg)
			return
		}
		created = append(created, bc)
	}
	resp := map[string]any{"status": "ok", "created": created}
	if req.Place {
		resp["place"] = s.place(lib, created)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// place imports each barcode through the MAP and moves it on to the first
// empty storage slot, one at a time so a single free MAP slot suffices. It
// stops at the first failure.
func (s *Server) place(lib config.Library, barcodes []string) placeResult {
	res := placeResult{Placed: []changer.Move{}}
	for _, bc := range barcodes {
		if err := s.cfg.Controller.ImportMap(int64(lib.ID), bc); err != nil {
			res.Error = fmt.Sprintf("import %s into the MAP: %v", bc, err)
			return res
		}
		mv, err := s.cfg.Changers.Store(lib, bc)
		if err != nil {
			res.Error = fmt.Sprintf("move %s from the MAP to a slot: %v (it is left in the MAP)", bc, err)
			return res
		}
		res.Placed = append(res.Placed, mv)
	}
	return res
}

// mediaEditRequest changes an existing cartridge; absent fields are kept.
type mediaEditRequest struct {
	WriteProtect *bool   `json:"write_protect"`
	Type         *string `json:"type"`
	Density      *string `json:"density"`
	SizeMB       *uint64 `json:"size_mb"`
}

func (s *Server) handleMediaEdit(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	barcode := r.PathValue("barcode")
	var req mediaEditRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	change := media.Change(req)
	switch {
	case !media.ValidBarcode(barcode):
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	case change.Empty():
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: give write_protect, type, density or size_mb")
		return
	case change.Type != nil && !media.ValidType(*change.Type):
		writeError(w, http.StatusBadRequest, "bad_request", "type must be one of "+strings.Join(media.Types, ", "))
		return
	case change.Density != nil && !media.ValidDensity(*change.Density):
		writeError(w, http.StatusBadRequest, "bad_request", "density must be one of "+strings.Join(media.Densities, ", "))
		return
	case change.SizeMB != nil && *change.SizeMB > maxSizeMB:
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("size_mb must be at most %d", uint64(maxSizeMB)))
		return
	}
	home := s.homeFor(lib)
	if !media.Exists(home, barcode) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no media for %s in %s", barcode, home))
		return
	}
	if s.cfg.Media == nil {
		writeError(w, http.StatusServiceUnavailable, "media_unavailable", "media tools not configured")
		return
	}
	// mhVTL does not lock media: editing a loaded cartridge would race the
	// drive, which rewrites the MAM when it unloads. Refuse unless the live
	// inventory of every library keeping media in this home shows it is not
	// in a drive, and hold those libraries still until edit_tape is done.
	// Moves a backup application makes through the changer itself are not
	// held back by this.
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable",
			"cannot confirm the cartridge is not in a drive: changer access not configured")
		return
	}
	sharing, err := s.librariesWithHome(home)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	var here *locationDTO
	for _, l := range sharing {
		defer s.lockLibrary(l.ID)()
		inv, err := s.cfg.Changers.Inventory(l)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "changer_unavailable", fmt.Sprintf(
				"cannot confirm the cartridge is not in a drive of library %d: %v", l.ID, err))
			return
		}
		loc, in := inv.Locate(barcode)
		if in && loc.Kind == "drive" {
			writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
				"%s is loaded in drive position %d of library %d: unmount it first", barcode, loc.Number, l.ID))
			return
		}
		// The library reads a cartridge's type when the cartridge enters it
		// (vtllibrary get_cart_type on start and on MAP load), so a type
		// changed in place would not reach READ ELEMENT STATUS.
		if in && change.Type != nil {
			writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
				"%s is in %s %d of library %d: the library only reads the type when a cartridge is imported, "+
					"so export and eject it before changing its type", barcode, loc.Kind, loc.Number, l.ID))
			return
		}
		if l.ID == lib.ID {
			here = locate(&inv, barcode)
		}
	}
	if err := s.cfg.Media.Edit(r.Context(), lib.ID, home, barcode, change); err != nil {
		writeError(w, http.StatusInternalServerError, "tool_error", "edit_tape "+barcode+": "+err.Error())
		return
	}
	c, err := media.Read(home, barcode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "media_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cartridgeDTO{Cartridge: c, Location: here})
}

// librariesWithHome returns the libraries whose media live in home, in
// ascending id order (the order their locks are taken in).
func (s *Server) librariesWithHome(home string) ([]config.Library, error) {
	inv, err := config.LoadInventory(s.cfg.ConfigDir)
	if err != nil {
		return nil, err
	}
	var out []config.Library
	for _, l := range inv.Libraries {
		if s.homeFor(l) == home {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// writeIPCError maps a control queue failure to an HTTP response.
func writeIPCError(w http.ResponseWriter, op string, lib int, err error) {
	var re *ipc.ReplyError
	switch {
	case errors.Is(err, ipc.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ipc_unavailable", err.Error())
	case errors.Is(err, ipc.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, "ipc_timeout", fmt.Sprintf("library %d did not reply: %v", lib, err))
	case errors.As(err, &re):
		writeError(w, http.StatusConflict, "conflict", op+" refused: "+re.Error())
	default:
		writeError(w, http.StatusInternalServerError, "control_error", err.Error())
	}
}
