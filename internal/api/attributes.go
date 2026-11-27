package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/drivestats"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/scsi"
)

// DriveAttributeService writes MAM attributes to the cartridge in a drive;
// implemented by *drivestats.Reader.
type DriveAttributeService interface {
	WriteAttributes(d config.Drive, serial string, attrs []scsi.Attribute) error
}

var _ DriveAttributeService = (*drivestats.Reader)(nil)

// attributesNote tells what the files the attributes come from reflect.
const attributesNote = "Values as of the cartridge's last MAM write: vtltape rewrites the MAM after every WRITE ATTRIBUTE " +
	"and on unload, and while the cartridge is loaded its counters may be ahead of these. " +
	"Editing needs the cartridge loaded in a drive."

// loadedInDTO is the drive a cartridge is loaded in: its library, its
// position in the changer and its device.conf id (0 when no drive of
// device.conf has that position).
type loadedInDTO struct {
	Library  int `json:"library"`
	Position int `json:"position"`
	Drive    int `json:"drive,omitempty"`
}

type attributesDTO struct {
	media.AttributeSet
	LoadedIn *loadedInDTO `json:"loaded_in,omitempty"`
	// LiveError says why it is unknown whether the cartridge is loaded.
	LiveError string `json:"live_error,omitempty"`
	Note      string `json:"note"`
}

// loadedIn returns where the inventory inv of library l has barcode in a
// drive, nil when it is not in one.
func loadedIn(l config.Library, inv changer.Inventory, barcode string) *loadedInDTO {
	loc, ok := inv.Locate(barcode)
	if !ok || loc.Kind != "drive" {
		return nil
	}
	in := &loadedInDTO{Library: l.ID, Position: loc.Number}
	if d := driveAt(l, loc.Number); d != nil {
		in.Drive = d.ID
	}
	return in
}

// driveAt returns the drive of l at changer position pos (its device.conf
// Slot), nil when there is none.
func driveAt(l config.Library, pos int) *config.Drive {
	for i := range l.Drives {
		if l.Drives[i].Slot == pos {
			return &l.Drives[i]
		}
	}
	return nil
}

func (s *Server) handleMediaAttributes(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	barcode := r.PathValue("barcode")
	if !media.ValidBarcode(barcode) {
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	}
	home := s.homeFor(lib)
	set, err := media.Attributes(home, barcode)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no media for %s in %s", barcode, home))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "media_error", err.Error())
		return
	}
	resp := attributesDTO{AttributeSet: set, Note: attributesNote}
	// The cartridge may be loaded in any library keeping media in this
	// home.
	sharing, err := s.librariesWithHome(home)
	if err != nil {
		resp.LiveError = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	invs, errs := s.liveInventories(r.Context(), sharing)
	var unknown []string
	for _, l := range sharing {
		inv, ok := invs[l.ID]
		if !ok {
			unknown = append(unknown, fmt.Sprintf("library %d: %s", l.ID, errs[l.ID]))
			continue
		}
		if in := loadedIn(l, inv, barcode); in != nil && resp.LoadedIn == nil {
			resp.LoadedIn = in
		}
	}
	if resp.LoadedIn == nil && len(unknown) > 0 {
		resp.LiveError = strings.Join(unknown, "; ")
	}
	writeJSON(w, http.StatusOK, resp)
}

// attributesEditRequest maps attribute ids ("0x0801") to new values.
type attributesEditRequest struct {
	Attributes map[string]string `json:"attributes"`
}

// editableList names the editable attributes for error messages.
func editableList() string {
	var ids []string
	for _, d := range media.EditableAttributes() {
		ids = append(ids, fmt.Sprintf("0x%04x %s", d.ID, d.Name))
	}
	return strings.Join(ids, ", ")
}

// attributesToWrite validates req and returns the attributes to send, in
// ascending id order, each padded to its field width (media.AttributeDef.Pad).
// utf8Text is media.AttributeSet.TextIsUTF8 of the cartridge.
func (req attributesEditRequest) attributesToWrite(utf8Text bool) ([]scsi.Attribute, error) {
	if len(req.Attributes) == 0 {
		return nil, errors.New(`nothing to change: give "attributes": {"0x0801": "..."}`)
	}
	seen := map[uint16]string{}
	var out []scsi.Attribute
	for key, value := range req.Attributes {
		id, err := media.ParseAttributeID(key)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("attribute 0x%04x given twice (%s and %s)", id, prev, key)
		}
		seen[id] = key
		def, ok := media.EditableAttribute(id)
		if !ok {
			return nil, fmt.Errorf("attribute 0x%04x is not editable; these are: %s", id, editableList())
		}
		v, err := def.Pad(value, utf8Text)
		if err != nil {
			return nil, err
		}
		out = append(out, scsi.Attribute{ID: id, Format: def.Format, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// handleMediaAttributesEdit writes MAM attributes with WRITE ATTRIBUTE
// through the drive the cartridge is loaded in. mhVTL has no way to change
// them for a cartridge that is not loaded: vtltape refuses WRITE
// ATTRIBUTE without one (usr/ssc.c:1433 at 2c40a93) and edit_tape has no
// option for them.
func (s *Server) handleMediaAttributesEdit(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	barcode := r.PathValue("barcode")
	var req attributesEditRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !media.ValidBarcode(barcode) {
		writeError(w, http.StatusBadRequest, "bad_request", "barcode must be 1-16 letters or digits")
		return
	}
	home := s.homeFor(lib)
	if !media.Exists(home, barcode) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no media for %s in %s", barcode, home))
		return
	}
	// The character set of TEXT attributes is the cartridge's own; an
	// unreadable MAM leaves it ASCII.
	cur, _ := media.Attributes(home, barcode)
	attrs, err := req.attributesToWrite(cur.TextIsUTF8())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// The medium serial number tells the cartridge in the drive is this
	// one; the barcode attribute (0806h) cannot, as any host may write it.
	serial, _ := cur.Value("0x0401")
	if serial == "" {
		writeError(w, http.StatusInternalServerError, "media_error", fmt.Sprintf(
			"the MAM of %s in %s has no medium serial number (0x0401), so the cartridge in the drive cannot be checked to be this one: %s",
			barcode, home, cur.Error))
		return
	}
	if s.cfg.DriveAttributes == nil {
		writeError(w, http.StatusServiceUnavailable, "drive_unavailable", "drive access not configured")
		return
	}
	if s.cfg.Changers == nil {
		writeError(w, http.StatusServiceUnavailable, "changer_unavailable",
			"cannot find the drive the cartridge is in: changer access not configured")
		return
	}
	sharing, err := s.librariesWithHome(home)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	// Find the drive with the libraries held still, as handleMediaEdit
	// does, so the cartridge is not moved by this daemon in between.
	var (
		in    *loadedInDTO
		drive *config.Drive
	)
	for _, l := range sharing {
		defer s.lockLibrary(l.ID)()
		inv, err := s.cfg.Changers.Inventory(l)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "changer_unavailable", fmt.Sprintf(
				"cannot find the drive %s is in: library %d: %v", barcode, l.ID, err))
			return
		}
		if in == nil {
			if in = loadedIn(l, inv, barcode); in != nil {
				drive = driveAt(l, in.Position)
			}
		}
	}
	if in == nil {
		writeError(w, http.StatusConflict, "not_loaded", fmt.Sprintf(
			"%s is not loaded in a drive: mount it first, as attributes are written through the drive (WRITE ATTRIBUTE)", barcode))
		return
	}
	if drive == nil {
		writeError(w, http.StatusServiceUnavailable, "drive_unavailable", fmt.Sprintf(
			"%s is in drive position %d of library %d, which no drive of device.conf has", barcode, in.Position, in.Library))
		return
	}
	err = s.cfg.DriveAttributes.WriteAttributes(*drive, serial, attrs)
	var se *scsi.SenseError
	var te *scsi.TransportError
	switch {
	case err == nil:
	case errors.Is(err, drivestats.ErrMediumChanged):
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
			"drive %d of library %d may no longer hold %s; nothing was written: %v", drive.ID, in.Library, barcode, err))
		return
	case errors.Is(err, drivestats.ErrWriteUnverified):
		writeError(w, http.StatusInternalServerError, "write_unverified", fmt.Sprintf(
			"WRITE ATTRIBUTE to drive %d of library %d succeeded, but the drive did not show %s right after (a backup application may have swapped it); check the attributes of the cartridges involved: %v",
			drive.ID, in.Library, barcode, err))
		return
	case errors.Is(err, drivestats.ErrReserved):
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf(
			"drive %d of library %d holding %s: %v", drive.ID, in.Library, barcode, err))
		return
	case errors.Is(err, drivestats.ErrUnavailable), errors.Is(err, changer.ErrUnavailable), errors.As(err, &te):
		writeError(w, http.StatusServiceUnavailable, "drive_unavailable", err.Error())
		return
	case errors.As(err, &se):
		writeError(w, http.StatusUnprocessableEntity, "scsi_error", fmt.Sprintf(
			"drive %d of library %d refused WRITE ATTRIBUTE: %v", drive.ID, in.Library, se))
		return
	default:
		writeError(w, http.StatusInternalServerError, "scsi_error", err.Error())
		return
	}
	set, err := media.Attributes(home, barcode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "media_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, attributesDTO{AttributeSet: set, LoadedIn: in, Note: attributesNote})
}
