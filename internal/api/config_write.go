package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/personality"
	"mhvtl-control/internal/validate"
)

// helperEnable is the command that enables the apply helper.
const helperEnable = "systemctl enable --now mhvtl-control-apply.socket"

// envFile is the daemon's options file, which sets MHVTL_CONTROL_API_TOKEN
// (cmd/mhvtl-control).
const envFile = "/etc/mhvtl-control/mhvtl-control.env"

// applyTimeout bounds an apply: the helper's RuntimeMaxSec=1800
// (dist/mhvtl-control-apply@.service) and a margin for its answer.
const applyTimeout = 31 * time.Minute

// maxNewElements bounds the slots or MAP slots one request adds.
const maxNewElements = 4096

// writeAllowed is the gate of every configuration write (plan 4.1): not on
// a non-loopback listen without TLS, only with an API token configured,
// which the auth middleware has checked, and only with the helper there,
// as its hello says (cached for helloTTL).
func (s *Server) writeAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.InsecureListen {
		writeError(w, http.StatusForbidden, writesTLSRequired, "configuration writes are refused on a non-loopback listen address without TLS")
		return false
	}
	if s.cfg.Token == "" {
		writeError(w, http.StatusForbidden, writesNoToken, "configuration writes need an API token: set MHVTL_CONTROL_API_TOKEN in "+envFile+" and restart mhvtl-control")
		return false
	}
	if s.cfg.Helper == nil {
		writeHelperAbsent(w)
		return false
	}
	state, err := s.helperState(r.Context())
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, helperUnreachable, err.Error())
		return false
	case state == applyclient.Absent:
		writeHelperAbsent(w)
		return false
	}
	return true
}

func writeHelperAbsent(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, writesNoHelper, "the apply helper is not enabled; run: "+helperEnable)
}

// forgetHello drops the cached hello, after the helper was found gone or
// an apply changed its state.
func (s *Server) forgetHello() {
	s.hello.mu.Lock()
	s.hello.at = time.Time{}
	s.hello.mu.Unlock()
}

// decodeStrict decodes one JSON object into v, refusing unknown fields. An
// empty DELETE body is no fields.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	switch {
	case err == io.EOF && r.Method == http.MethodDelete:
		return true
	case err == nil:
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			return true
		} else if err == nil {
			err = errors.New("unexpected data after the JSON object")
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body over %d bytes", maxBody))
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
	return false
}

// editError is a staging request refused with status and code.
type editError struct {
	status int
	code   string
	msg    string
}

func (e *editError) Error() string { return e.msg }

func badEdit(format string, a ...any) error {
	return &editError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) error {
	return &editError{http.StatusNotFound, "not_found", fmt.Sprintf(format, a...)}
}

func exists(format string, a ...any) error {
	return &editError{http.StatusConflict, "exists", fmt.Sprintf(format, a...)}
}

// confErr maps an error of a conffile edit: an element or stanza that is not
// there is 404, a file mhVTL would not read as written 422, anything else
// (an invalid value) 400.
func confErr(err error) error {
	var ee *editError
	var p conffile.Problem
	switch {
	case err == nil, errors.As(err, &ee):
		return err
	case errors.Is(err, conffile.ErrNotFound):
		return &editError{http.StatusNotFound, "not_found", err.Error()}
	case errors.As(err, &p):
		return &editError{http.StatusUnprocessableEntity, apply.ErrNotWritable, err.Error()}
	}
	return badEdit("%v", err)
}

// writeStageError answers a request whose stage could not be opened.
func writeStageError(w http.ResponseWriter, err error) {
	var applied *stageAppliedError
	switch {
	case errors.Is(err, errStageNotFound):
		writeError(w, http.StatusNotFound, "stage_not_found", err.Error())
	case errors.Is(err, errTooManyStages):
		writeError(w, http.StatusTooManyRequests, "too_many_stages", err.Error())
	case errors.Is(err, errStageBusy):
		writeError(w, http.StatusConflict, "stage_busy", err.Error())
	case errors.As(err, &applied):
		writeError(w, http.StatusConflict, "stage_applied", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
	}
}

// stageEdit runs edit on the stage stageID names, or on a new one, and
// answers with the stage. edit changes the stage only when it succeeds and
// leaves the stage within its bounds (checkBounds) and the home rules
// (checkHomes).
func (s *Server) stageEdit(w http.ResponseWriter, r *http.Request, stageID string, edit func(context.Context, *stage) error) {
	st, expires, err := s.openStage(stageID)
	if err != nil {
		writeStageError(w, err)
		return
	}
	defer s.releaseStage(st)
	next, disp, notes := maps.Clone(st.next), maps.Clone(st.dispositions), maps.Clone(st.notes)
	err = edit(r.Context(), st)
	if err == nil {
		err = st.writable()
	}
	if err == nil {
		err = s.checkBounds(st)
	}
	if err == nil {
		err = s.checkHomes(st)
	}
	if err = confErr(err); err != nil {
		st.next, st.dispositions, st.notes = next, disp, notes
		if stageID == "" {
			s.dropStage(st.id)
		}
		var ee *editError
		if errors.As(err, &ee) {
			writeError(w, ee.status, ee.code, ee.msg)
		} else {
			writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		}
		return
	}
	st.edited()
	writeJSON(w, http.StatusOK, st.dto(expires))
}

// elementLimit is the highest Slot or MAP number lib's personality windows
// hold (personality.Model.Limits; maxNewElements if it gives none).
func elementLimit(lib config.Library, kind string) int {
	l := personality.Lookup(lib.Vendor, lib.Product).Model.Limits()
	n := l.Slots
	if kind == "MAP" {
		n = l.MAPs
	}
	if n <= 0 {
		n = maxNewElements
	}
	return n
}

// highestElement is lib's highest Slot or MAP number.
func highestElement(lib config.Library, kind string) int {
	n := 0
	if kind == "MAP" {
		for _, m := range lib.MAPs {
			n = max(n, m.Number)
		}
		return n
	}
	for _, sl := range lib.Slots {
		n = max(n, sl.Number)
	}
	return n
}

func slotsOutOfRange(lib config.Library, kind string, n, limit int) error {
	return &editError{http.StatusUnprocessableEntity, "slots_out_of_range", fmt.Sprintf("library %d: %s %d is past the %d the %s personality holds",
		lib.ID, kind, n, limit, personality.Lookup(lib.Vendor, lib.Product).Model.Name)}
}

// checkBounds refuses a stage over maxStageBytes, or with a library's
// highest Slot or MAP past its personality's window, unless the file on
// disk already went that far (#61 review M1): each edit parses and renders
// the stage, under its lock.
func (s *Server) checkBounds(st *stage) error {
	total := 0
	for _, b := range st.next {
		total += len(b)
	}
	if total > maxStageBytes {
		return &editError{http.StatusRequestEntityTooLarge, "stage_too_large",
			fmt.Sprintf("the stage would hold %d bytes of files, over %d: apply or discard it first", total, maxStageBytes)}
	}
	inv, err := st.inventory(s.cfg.ConfigDir)
	if err != nil {
		return err
	}
	for _, lib := range inv.Libraries {
		if st.next[contentsFile(lib.ID)] == nil {
			continue // not edited
		}
		was, _ := findConfigLibrary(st.cur, lib.ID)
		for _, kind := range []string{"Slot", "MAP"} {
			n, limit := highestElement(lib, kind), elementLimit(lib, kind)
			if n > limit && n > highestElement(was, kind) {
				return slotsOutOfRange(lib, kind, n, limit)
			}
		}
	}
	return nil
}

// pathInt reads an integer path value.
func pathInt(r *http.Request, name string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n < 0 {
		return 0, badEdit("%s must be a non-negative integer", name)
	}
	return n, nil
}

// stageLibrary returns library id as the stage has it.
func stageLibrary(dir string, st *stage, id int) (config.Library, error) {
	inv, err := st.inventory(dir)
	if err != nil {
		return config.Library{}, err
	}
	lib, ok := findConfigLibrary(inv, id)
	if !ok {
		return config.Library{}, notFound("library %d not found", id)
	}
	return lib, nil
}

type attrDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// setAttrs sets (or, for null, deletes) attrs in a stanza, in key order.
func setAttrs(dc *conffile.DeviceConf, kind string, id int, attrs map[string]*string) error {
	if len(attrs) == 0 {
		return badEdit("no attrs to change")
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "Library ID" {
			return badEdit("a drive's Library ID cannot change: delete the drive and add it again")
		}
		var err error
		if v := attrs[k]; v == nil {
			err = dc.DeleteAttr(kind, id, k)
		} else {
			err = dc.SetAttr(kind, id, k, *v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func toAttrs(in []attrDTO) []conffile.Attr {
	out := make([]conffile.Attr, len(in))
	for i, a := range in {
		out[i] = conffile.Attr{Key: a.Key, Value: a.Value}
	}
	return out
}

func checkAddress(channel, target, lun int) error {
	if channel < 0 || target < 0 || lun < 0 {
		return badEdit("channel, target and lun must not be negative")
	}
	return nil
}

type libraryCreateRequest struct {
	Stage   string    `json:"stage"`
	ID      int       `json:"id"`
	Channel int       `json:"channel"`
	Target  int       `json:"target"`
	LUN     int       `json:"lun"`
	Attrs   []attrDTO `json:"attrs"`
	// Slots and MAPs are the storage and MAP slots of its
	// library_contents, all empty.
	Slots int `json:"slots"`
	MAPs  int `json:"maps"`
}

func (s *Server) handleConfigLibraryCreate(w http.ResponseWriter, r *http.Request) {
	var req libraryCreateRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		switch {
		case !validContentsName(contentsFile(req.ID)):
			return badEdit("library id %d out of range", req.ID)
		case req.Slots < 0 || req.Slots > maxNewElements || req.MAPs < 0 || req.MAPs > maxNewElements:
			return badEdit("slots and maps must be 0 to %d", maxNewElements)
		}
		if err := checkAddress(req.Channel, req.Target, req.LUN); err != nil {
			return err
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if _, ok := dc.Stanza("Library", req.ID); ok {
			return exists("library %d already exists", req.ID)
		}
		h := conffile.Header{Kind: "Library", ID: req.ID, Channel: req.Channel, Target: req.Target, LUN: req.LUN}
		if err := dc.AddStanza(h, toAttrs(req.Attrs)); err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString("VERSION: 2\n\nPicker 1:\n")
		for _, part := range []struct {
			kind string
			n    int
		}{{"MAP", req.MAPs}, {"Slot", req.Slots}} {
			if part.n > 0 {
				b.WriteString("\n")
			}
			for i := 1; i <= part.n; i++ {
				fmt.Fprintf(&b, "%s %d:\n", part.kind, i)
			}
		}
		name := contentsFile(req.ID)
		if err := s.adopt(st, name); err != nil {
			return err
		}
		st.next["device.conf"] = dc.Render()
		st.next[name] = []byte(b.String())
		return nil
	})
}

type libraryPatchRequest struct {
	Stage string `json:"stage"`
	// Attrs maps a key to its new value; null deletes the key.
	Attrs map[string]*string `json:"attrs"`
}

func (s *Server) handleConfigLibraryPatch(w http.ResponseWriter, r *http.Request) {
	var req libraryPatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if err := setAttrs(dc, "Library", id, req.Attrs); err != nil {
			return err
		}
		// The library's live placement goes into its library_contents
		// with any change to it.
		if _, err := s.contents(ctx, st, id); err != nil {
			return err
		}
		st.next["device.conf"] = dc.Render()
		return nil
	})
}

type libraryDeleteRequest struct {
	Stage string `json:"stage"`
	// Dispositions say where the library's cartridges go.
	Dispositions []validate.Disposition `json:"dispositions"`
}

func (s *Server) handleConfigLibraryDelete(w http.ResponseWriter, r *http.Request) {
	var req libraryDeleteRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		lib, err := stageLibrary(s.cfg.ConfigDir, st, id)
		if err != nil {
			return err
		}
		for _, d := range req.Dispositions {
			if d.Barcode == "" {
				return badEdit("a disposition needs a barcode")
			}
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if err := dc.RemoveStanza("Library", id); err != nil {
			return err
		}
		for _, d := range lib.Drives {
			if err := dc.RemoveStanza("Drive", d.ID); err != nil {
				return err
			}
		}
		st.next["device.conf"] = dc.Render()
		delete(st.notes, id)
		name := contentsFile(id)
		if _, inCur := findConfigLibrary(st.cur, id); inCur {
			st.next[name] = nil
		} else {
			// New in the stage: there is nothing to delete.
			delete(st.next, name)
		}
		for _, d := range req.Dispositions {
			d.Library = id
			st.dispose(d)
		}
		return nil
	})
}

type driveCreateRequest struct {
	Stage   string `json:"stage"`
	ID      int    `json:"id"`
	Channel int    `json:"channel"`
	Target  int    `json:"target"`
	LUN     int    `json:"lun"`
	// Slot is the drive's number in the library, its "Library ID: L Slot:
	// N" and its "Drive N:" line.
	Slot  int       `json:"slot"`
	Attrs []attrDTO `json:"attrs"`
}

func (s *Server) handleConfigDriveCreate(w http.ResponseWriter, r *http.Request) {
	var req driveCreateRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		if err := checkAddress(req.Channel, req.Target, req.LUN); err != nil {
			return err
		}
		if req.ID < 0 || req.Slot < 1 {
			return badEdit("a drive needs an id and a slot from 1")
		}
		for _, a := range req.Attrs {
			if a.Key == "Library ID" {
				return badEdit("Library ID is the route's library and slot")
			}
		}
		if _, err := stageLibrary(s.cfg.ConfigDir, st, id); err != nil {
			return err
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if _, ok := dc.Stanza("Drive", req.ID); ok {
			return exists("drive %d already exists", req.ID)
		}
		attrs := append([]conffile.Attr{{Key: "Library ID", Value: fmt.Sprintf("%d Slot: %02d", id, req.Slot)}}, toAttrs(req.Attrs)...)
		h := conffile.Header{Kind: "Drive", ID: req.ID, Channel: req.Channel, Target: req.Target, LUN: req.LUN}
		if err := dc.AddStanza(h, attrs); err != nil {
			return err
		}
		lc, err := s.contents(ctx, st, id)
		if err != nil {
			return err
		}
		if !hasEntry(lc, "Drive", req.Slot) {
			if err := lc.AddEntry("Drive", req.Slot, ""); err != nil {
				return err
			}
		}
		st.next["device.conf"] = dc.Render()
		st.next[contentsFile(id)] = lc.Render()
		return nil
	})
}

// stageDrive returns drive driveID of library id as the stage has it.
func stageDrive(dir string, st *stage, r *http.Request) (int, config.Drive, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return 0, config.Drive{}, err
	}
	driveID, err := pathInt(r, "drive")
	if err != nil {
		return 0, config.Drive{}, err
	}
	lib, err := stageLibrary(dir, st, id)
	if err != nil {
		return 0, config.Drive{}, err
	}
	for _, d := range lib.Drives {
		if d.ID == driveID {
			return id, d, nil
		}
	}
	return 0, config.Drive{}, notFound("library %d has no drive %d", id, driveID)
}

type drivePatchRequest struct {
	Stage string             `json:"stage"`
	Attrs map[string]*string `json:"attrs"`
}

func (s *Server) handleConfigDrivePatch(w http.ResponseWriter, r *http.Request) {
	var req drivePatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, d, err := stageDrive(s.cfg.ConfigDir, st, r)
		if err != nil {
			return err
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if err := setAttrs(dc, "Drive", d.ID, req.Attrs); err != nil {
			return err
		}
		if _, err := s.contents(ctx, st, id); err != nil {
			return err
		}
		st.next["device.conf"] = dc.Render()
		return nil
	})
}

type driveDeleteRequest struct {
	Stage string `json:"stage"`
}

func (s *Server) handleConfigDriveDelete(w http.ResponseWriter, r *http.Request) {
	var req driveDeleteRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, d, err := stageDrive(s.cfg.ConfigDir, st, r)
		if err != nil {
			return err
		}
		dc, err := st.deviceConf()
		if err != nil {
			return err
		}
		if err := dc.RemoveStanza("Drive", d.ID); err != nil {
			return err
		}
		lc, err := s.contents(ctx, st, id)
		if err != nil {
			return err
		}
		if hasEntry(lc, "Drive", d.Slot) {
			if err := lc.RemoveEntry("Drive", d.Slot); err != nil {
				return err
			}
		}
		st.next["device.conf"] = dc.Render()
		st.next[contentsFile(id)] = lc.Render()
		return nil
	})
}

// entry returns the last entry of kind and n, the one mhVTL applies.
func entry(lc *conffile.LibContents, kind string, n int) (conffile.Entry, bool) {
	var found conffile.Entry
	ok := false
	for _, e := range lc.Entries() {
		if e.Kind == kind && e.Number == n {
			found, ok = e, true
		}
	}
	return found, ok
}

func hasEntry(lc *conffile.LibContents, kind string, n int) bool {
	_, ok := entry(lc, kind, n)
	return ok
}

// slotKind maps a request's kind, "slot" (the default) or "map", to its
// library_contents entry kind.
func slotKind(kind string) (string, error) {
	switch kind {
	case "", "slot":
		return "Slot", nil
	case "map":
		return "MAP", nil
	}
	return "", badEdit("kind must be slot or map, not %q", kind)
}

type slotsAddRequest struct {
	Stage string `json:"stage"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

func (s *Server) handleConfigSlotsAdd(w http.ResponseWriter, r *http.Request) {
	var req slotsAddRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		kind, err := slotKind(req.Kind)
		if err != nil {
			return err
		}
		if req.Count < 1 || req.Count > maxNewElements {
			return badEdit("count must be 1 to %d", maxNewElements)
		}
		lib, err := stageLibrary(s.cfg.ConfigDir, st, id)
		if err != nil {
			return err
		}
		// Refused before the lines are built.
		if n, limit := highestElement(lib, kind)+req.Count, elementLimit(lib, kind); n > limit {
			return slotsOutOfRange(lib, kind, n, limit)
		}
		lc, err := s.contents(ctx, st, id)
		if err != nil {
			return err
		}
		if err := lc.AppendEntries(kind, req.Count); err != nil {
			return err
		}
		st.next[contentsFile(id)] = lc.Render()
		return nil
	})
}

// stageSlot reads the library and slot of a slot route, and the stage's
// library_contents of that library.
func (s *Server) stageSlot(ctx context.Context, st *stage, r *http.Request) (int, int, *conffile.LibContents, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return 0, 0, nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return 0, 0, nil, err
	}
	if _, err := stageLibrary(s.cfg.ConfigDir, st, id); err != nil {
		return 0, 0, nil, err
	}
	lc, err := s.contents(ctx, st, id)
	return id, n, lc, err
}

type slotPatchRequest struct {
	Stage string `json:"stage"`
	Kind  string `json:"kind"`
	// Barcode pre-fills the slot; "" empties it.
	Barcode string `json:"barcode"`
}

func (s *Server) handleConfigSlotPatch(w http.ResponseWriter, r *http.Request) {
	var req slotPatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		kind, err := slotKind(req.Kind)
		if err != nil {
			return err
		}
		id, n, lc, err := s.stageSlot(ctx, st, r)
		if err != nil {
			return err
		}
		if err := lc.SetBarcode(kind, n, req.Barcode); err != nil {
			return err
		}
		st.next[contentsFile(id)] = lc.Render()
		return nil
	})
}

type slotDeleteRequest struct {
	Stage string `json:"stage"`
	Kind  string `json:"kind"`
	// Disposition says where the slot's cartridge goes; its library and
	// barcode are the slot's.
	Disposition *validate.Disposition `json:"disposition"`
}

func (s *Server) handleConfigSlotDelete(w http.ResponseWriter, r *http.Request) {
	var req slotDeleteRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		kind, err := slotKind(req.Kind)
		if err != nil {
			return err
		}
		id, n, lc, err := s.stageSlot(ctx, st, r)
		if err != nil {
			return err
		}
		e, ok := entry(lc, kind, n)
		if !ok {
			return notFound("library %d has no %s %d", id, kind, n)
		}
		if req.Disposition != nil && e.Barcode == "" {
			return badEdit("%s %d holds no cartridge to dispose of", kind, n)
		}
		if err := lc.RemoveEntry(kind, n); err != nil {
			return err
		}
		st.next[contentsFile(id)] = lc.Render()
		if d := req.Disposition; d != nil {
			d.Library, d.Barcode = id, e.Barcode
			st.dispose(*d)
		}
		return nil
	})
}

type applyRequest struct {
	Stage string `json:"stage"`
	// DryRun asks the helper for a plan; an apply needs one of the stage
	// as it is now.
	DryRun bool `json:"dry_run"`
	// ForceToken is a drive_loaded plan's token, for an apply that
	// unloads those drives; a dry run with one is refused.
	ForceToken string `json:"force_token"`
}

// applyPlanDTO is what the apply does: the units it stops and starts, what
// applies without a restart, and the libraries offline to hosts meanwhile.
//
// Plan 4.1 also names "merges" and "unloads"; the helper does not return
// them as lists of their own, and they are where applyResultDTO says.
type applyPlanDTO struct {
	// Units are the vtllibrary@ and vtltape@ instances the apply stops and
	// starts. After a plan refused with drive_loaded they are the forced
	// apply's: with the vtltape@ of each loaded drive it would unload
	// (mhvtl-control#58), which is where a plan shows the unloads.
	Units            []string `json:"units"`
	Notes            []string `json:"notes"`
	OfflineLibraries []int    `json:"offline_libraries"`
}

// applyResultDTO is POST /config/apply's answer: the helper's, and on a
// status other than 200 the error envelope's code and message too.
type applyResultDTO struct {
	Error *errorDetail `json:"error,omitempty"`
	// Stage is the stage applied; a service action has none.
	Stage  string       `json:"stage,omitempty"`
	Result string       `json:"result"`
	Plan   applyPlanDTO `json:"plan"`
	// Findings are the validation of the change, the placement merge's
	// included (validate.Merge of the staged library_contents with the
	// live placement): the removal.* findings say which cartridges a
	// change moves back, or makes leave a library and where to.
	Findings *validate.Result `json:"findings,omitempty"`
	// Warnings are what the apply would do that may not be meant: a Slot
	// cartridge missing from its home, created blank at the library's
	// start (mhvtl-control#43).
	Warnings []string `json:"warnings,omitempty"`
	// ForceToken and ForceExpires answer a dry run refused for
	// drive_loaded, whose error message names the loaded drives. An apply
	// refused for drive_loaded gives none: the client plans again and
	// confirms the unloads that plan lists.
	ForceToken   string     `json:"force_token,omitempty"`
	ForceExpires *time.Time `json:"force_expires,omitempty"`
	// ForcedUnloads are the unloads done: each cartridge a forced apply,
	// or a rollback, took out of a drive, and the slot it went to.
	ForcedUnloads []apply.ForcedUnload `json:"forced_unloads,omitempty"`
	// PlacementDrift is where the changer, read after the restart, differs
	// from the merged placement written.
	PlacementDrift []string `json:"placement_drift,omitempty"`
	// File and CurrentSHA256 describe a conflict.
	File          string  `json:"file,omitempty"`
	CurrentSHA256 *string `json:"current_sha256,omitempty"`
	// FailedUnit, JournalTail, OrphanedMedia, Unplaced, Conflicts and
	// RollbackDir describe a rollback (apply.Response).
	FailedUnit    string           `json:"failed_unit,omitempty"`
	JournalTail   string           `json:"journal_tail,omitempty"`
	OrphanedMedia []string         `json:"orphaned_media,omitempty"`
	Unplaced      []string         `json:"unplaced,omitempty"`
	Conflicts     []apply.Conflict `json:"conflicts,omitempty"`
	RollbackDir   string           `json:"rollback_dir,omitempty"`
	// Dispositions are the stage's, as sent.
	Dispositions []validate.Disposition `json:"dispositions,omitempty"`
}

// applyStatus maps a helper result to a status and an error code ("" for
// success).
func applyStatus(resp apply.Response) (int, string) {
	switch resp.Result {
	case apply.ResultValidated, apply.ResultApplied:
		return http.StatusOK, ""
	case apply.ResultRefused:
		// The helper's durable refusals: nothing the request changes
		// helps (capabilities.writes helper_blocked).
		if resp.Error == apply.ErrIntentUnreadable || resp.Error == apply.ErrLegacyEnv {
			return http.StatusConflict, resp.Error
		}
		return http.StatusUnprocessableEntity, resp.Error
	case apply.ResultConflict, apply.ResultInProgress, apply.ResultRecovered:
		return http.StatusConflict, resp.Result
	case apply.ResultRolledBack:
		return http.StatusInternalServerError, resp.Result
	case apply.ResultError:
		return http.StatusInternalServerError, resp.Error
	}
	return http.StatusBadGateway, "helper_error"
}

func applyMessage(resp apply.Response) string {
	switch resp.Result {
	case apply.ResultConflict:
		return resp.File + " changed since the stage read it: stage the change again"
	case apply.ResultInProgress:
		return "another apply is running"
	case apply.ResultRecovered:
		return "the helper recovered an apply that had died and did nothing else: plan again"
	case apply.ResultRolledBack:
		return strings.TrimPrefix(resp.Error+": "+resp.Message, ": ")
	}
	if resp.Message != "" {
		return resp.Message
	}
	return "helper result " + resp.Result
}

// offlineLibraries are the libraries whose vtllibrary@ units restart.
func offlineLibraries(units []string) []int {
	var ids []int
	for _, u := range units {
		if n, ok := strings.CutPrefix(u, "vtllibrary@"); ok {
			if id, err := strconv.Atoi(strings.TrimSuffix(n, ".service")); err == nil {
				ids = append(ids, id)
			}
		}
	}
	sort.Ints(ids)
	return ids
}

// applyResultOf is the helper's answer as applyResultDTO, with the error
// envelope's code when code is not "".
func applyResultOf(resp apply.Response, code string) applyResultDTO {
	out := applyResultDTO{
		Result:         resp.Result,
		Plan:           applyPlanDTO{Units: resp.Units, Notes: resp.Notes, OfflineLibraries: offlineLibraries(resp.Units)},
		Findings:       resp.Findings,
		Warnings:       resp.Warnings,
		ForceToken:     resp.ForceToken,
		ForceExpires:   resp.ForceExpires,
		ForcedUnloads:  resp.ForcedUnloads,
		PlacementDrift: resp.PlacementDrift,
		File:           resp.File,
		CurrentSHA256:  resp.CurrentSHA256,
		FailedUnit:     resp.FailedUnit,
		JournalTail:    resp.JournalTail,
		OrphanedMedia:  resp.OrphanedMedia,
		Unplaced:       resp.Unplaced,
		Conflicts:      resp.Conflicts,
		RollbackDir:    resp.RollbackDir,
	}
	if code != "" {
		out.Error = &errorDetail{Code: code, Message: applyMessage(resp)}
	}
	return out
}

// handleConfigApply sends a stage to the helper: as a plan with dry_run,
// else as an apply expecting the units of the stage's plan, which must be
// of the stage as it is (plan_required otherwise). A plan refused for
// drive_loaded gives the forced apply's units; the apply with its token
// expects those (mhvtl-control#58).
func (s *Server) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	if req.Stage == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "stage is required")
		return
	}
	if req.DryRun && req.ForceToken != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "force_token is for an apply, not a dry run")
		return
	}
	st, _, err := s.openStage(req.Stage)
	if err != nil {
		writeStageError(w, err)
		return
	}
	defer s.releaseStage(st)
	files, del := st.changes()
	if len(files) == 0 && len(del) == 0 {
		writeError(w, http.StatusBadRequest, "no_changes", "the stage changes nothing")
		return
	}
	hreq := apply.Request{
		Op:           apply.OpPlan,
		Files:        files,
		Delete:       del,
		Dispositions: st.dispositionList(),
		Actor:        "mhvtl-control API client " + r.RemoteAddr,
	}
	ctx := r.Context()
	if !req.DryRun {
		if st.plan == nil || st.plan.rev != st.rev {
			writeError(w, http.StatusConflict, "plan_required", "apply needs a dry run of the stage as it is now")
			return
		}
		hreq.Op, hreq.ExpectUnits, hreq.ForceToken = apply.OpApply, st.plan.units, req.ForceToken
		// The apply runs to its end even if the client goes away.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
		defer cancel()
	}
	resp, err := s.cfg.Helper.Do(ctx, hreq)
	s.forgetHello()
	if errors.Is(err, applyclient.ErrAbsent) {
		writeHelperAbsent(w)
		return
	}
	if err != nil {
		if !req.DryRun {
			// The apply may have run, or not: the stage plans again, and
			// shows the attempt (GET /config/stages/{id}).
			st.plan = nil
			st.last = &applyResultDTO{Stage: st.id, Result: "unknown", Dispositions: hreq.Dispositions,
				Error: &errorDetail{Code: helperUnreachable, Message: "no answer from the apply helper: " + err.Error()}}
		}
		writeError(w, http.StatusBadGateway, helperUnreachable, err.Error())
		return
	}

	st.plan = nil
	if req.DryRun && (resp.Result == apply.ResultValidated || resp.Result == apply.ResultRefused && resp.Error == apply.ErrDriveLoaded) {
		st.plan = &stagePlan{rev: st.rev, units: resp.Units}
	}
	if !req.DryRun {
		for _, id := range offlineLibraries(resp.Units) {
			s.live.forget(id)
		}
	}
	status, code := applyStatus(resp)
	out := applyResultOf(resp, code)
	out.Stage, out.Dispositions = st.id, hreq.Dispositions
	if !req.DryRun {
		out.ForceToken, out.ForceExpires = "", nil
		last := out
		st.last = &last
		if resp.Result == apply.ResultApplied {
			s.closeStage(st, out)
		}
	}
	writeJSON(w, status, out)
}

// handleConfigStage shows a stage: its files, its last apply's result, or
// for an applied one the tombstone with that result. It does not wait for
// a request using the stage: it answers busy.
func (s *Server) handleConfigStage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c := &s.stages
	c.mu.Lock()
	now := s.now()
	if t, ok := c.tombs[id]; ok && now.Before(t.expires) {
		c.mu.Unlock()
		res := t.result
		writeJSON(w, http.StatusOK, stageDTO{Stage: id, Expires: t.expires, State: stageApplied, Files: map[string]*string{},
			Base: map[string]string{}, Dispositions: res.Dispositions, LastApply: &res})
		return
	}
	st, err := c.find(id, now)
	if err != nil {
		c.mu.Unlock()
		writeStageError(w, err)
		return
	}
	if !st.mu.TryLock() {
		expires := st.expires
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, stageDTO{Stage: id, Expires: expires, State: stageBusy, Files: map[string]*string{}, Base: map[string]string{}})
		return
	}
	st.busy++
	c.mu.Unlock()
	if st.closed {
		// Applied since find: the tombstone answers.
		s.releaseStage(st)
		s.handleConfigStage(w, r)
		return
	}
	defer s.releaseStage(st)
	writeJSON(w, http.StatusOK, st.dto(now.Add(stageTTL)))
}

// handleConfigStageDelete discards a stage (#61 review L1). A stage an
// apply uses is not discarded.
func (s *Server) handleConfigStageDelete(w http.ResponseWriter, r *http.Request) {
	if !s.writeAllowed(w, r) {
		return
	}
	id := r.PathValue("id")
	c := &s.stages
	c.mu.Lock()
	st, err := c.find(id, s.now())
	if err == nil && st.busy > 0 {
		err = errStageBusy
	}
	if err == nil {
		delete(c.m, id)
	}
	c.mu.Unlock()
	if err != nil {
		writeStageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
