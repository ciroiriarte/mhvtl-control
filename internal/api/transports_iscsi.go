package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/transports"
)

// The iSCSI routes edit fabrics.conf, mhvtl-fabrics' configuration
// (fabrics.conf(5)), and have the helper apply it (apply.OpFabrics). The
// daemon never writes configfs: mhvtl-fabrics.service does (mhvtl-control#6).
// No answer of these routes, and no log line of theirs, holds a CHAP or
// mutual CHAP secret.

const fabricsConfName = "fabrics.conf"

// iscsiRecordDTO is fabrics.conf's iSCSI record.
type iscsiRecordDTO struct {
	Present  bool     `json:"present"`
	Portals  []string `json:"portals"`
	BaseName string   `json:"base_name"`
}

// iscsiDeviceDTO is the device.conf device a target exports.
type iscsiDeviceDTO struct {
	Kind string `json:"kind"`
	ID   int    `json:"id"`
}

// iscsiTargetDTO is one "iSCSI target:" record, CHAP as booleans and
// names only.
type iscsiTargetDTO struct {
	ID         int    `json:"id"`
	TargetName string `json:"target_name"`
	// WWN is the name mhvtl-fabrics gives the target: the Target name,
	// else the Base name and ".lib<ID>" or ".tape<ID>"; "" when neither
	// can be had.
	WWN            string          `json:"wwn"`
	Initiators     []string        `json:"initiators"`
	CHAP           bool            `json:"chap"`
	CHAPName       string          `json:"chap_name"`
	MutualCHAP     bool            `json:"mutual_chap"`
	MutualCHAPName string          `json:"mutual_chap_name"`
	Device         *iscsiDeviceDTO `json:"device"`
}

type iscsiProblemDTO struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// iscsiConfigDTO is GET /transports/iscsi/config.
type iscsiConfigDTO struct {
	Exists   bool              `json:"exists"`
	SHA256   string            `json:"sha256"`
	ISCSI    iscsiRecordDTO    `json:"iscsi"`
	Targets  []iscsiTargetDTO  `json:"targets"`
	Problems []iscsiProblemDTO `json:"problems"`
	Warnings []string          `json:"warnings"`
}

// fabricsFiles is fabrics.conf and device.conf as read for one request.
type fabricsFiles struct {
	data   []byte
	sha    string // "" when fabrics.conf does not exist
	exists bool
	conf   *conffile.FabricsConf
	dev    *conffile.DeviceConf
}

// readFabricsFiles reads fabrics.conf and device.conf from the
// configuration directory; a file that does not exist is empty. A read
// error names the file only: the absolute path goes to the log.
func (s *Server) readFabricsFiles() (fabricsFiles, error) {
	var f fabricsFiles
	data, sha, err := conffile.ReadFile(filepath.Join(s.cfg.ConfigDir, fabricsConfName))
	switch {
	case err == nil:
		f.data, f.sha, f.exists = data, sha, true
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("transports/iscsi: %v", err)
		return f, errors.New(fabricsConfName + ": unreadable")
	}
	dev, _, err := conffile.ReadFile(filepath.Join(s.cfg.ConfigDir, "device.conf"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("transports/iscsi: %v", err)
		return f, errors.New("device.conf: unreadable")
	}
	f.conf, f.dev = conffile.ParseFabricsConf(f.data), conffile.ParseDeviceConf(dev)
	return f, nil
}

// deviceOf finds device.conf's Library or Drive id.
func deviceOf(dev *conffile.DeviceConf, id int) *iscsiDeviceDTO {
	for _, st := range dev.Stanzas() {
		if st.ID == id {
			return &iscsiDeviceDTO{Kind: strings.ToLower(st.Kind), ID: id}
		}
	}
	return nil
}

// fabricsWWN is the name mhvtl-fabrics gives target t: its Target name,
// else the Base name and ".lib<ID>" for a library or ".tape<ID>" for a
// drive (fab_plan,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L537-L611).
// A drive in a library gets the name it would have, though CheckPlan
// refuses to export it.
func fabricsWWN(t conffile.FabricsTarget, base string, d *iscsiDeviceDTO) string {
	switch {
	case t.TargetName != "":
		return t.TargetName
	case base == "" || d == nil:
		return ""
	case d.Kind == "library":
		return fmt.Sprintf("%s.lib%d", base, t.ID)
	}
	return fmt.Sprintf("%s.tape%d", base, t.ID)
}

// handleISCSIConfig answers fabrics.conf as the iSCSI configuration. It
// never fails: what could not be read is named in warnings.
func (s *Server) handleISCSIConfig(w http.ResponseWriter, _ *http.Request) {
	out := iscsiConfigDTO{ISCSI: iscsiRecordDTO{Portals: []string{}}, Targets: []iscsiTargetDTO{},
		Problems: []iscsiProblemDTO{}, Warnings: []string{}}
	f, err := s.readFabricsFiles()
	if err != nil {
		out.Warnings = append(out.Warnings, err.Error())
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Exists, out.SHA256 = f.exists, f.sha
	portals, base, present := f.conf.ISCSI()
	out.ISCSI.Present, out.ISCSI.BaseName = present, base
	if portals != nil {
		out.ISCSI.Portals = portals
	}
	for _, t := range f.conf.Targets() {
		d := deviceOf(f.dev, t.ID)
		inits := t.Initiators
		if inits == nil {
			inits = []string{}
		}
		out.Targets = append(out.Targets, iscsiTargetDTO{
			ID: t.ID, TargetName: t.TargetName, WWN: fabricsWWN(t, base, d), Initiators: inits,
			CHAP: t.HasCHAP, CHAPName: t.CHAPName, MutualCHAP: t.HasMutualCHAP, MutualCHAPName: t.MutualCHAPName,
			Device: d,
		})
	}
	ps := f.conf.Problems
	if len(ps) == 0 {
		// CheckPlan is for a file without parse problems.
		ps = f.conf.CheckPlan(f.dev)
	}
	for _, p := range ps {
		out.Problems = append(out.Problems, iscsiProblemDTO{Line: p.Line, Message: p.Msg})
	}
	writeJSON(w, http.StatusOK, out)
}

// chapDTO is a CHAP name and secret in a request. The secret is never
// answered or logged.
type chapDTO struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

func (c *chapDTO) creds() *conffile.CHAPCreds {
	if c == nil {
		return nil
	}
	return &conffile.CHAPCreds{Name: c.Name, Secret: c.Secret}
}

// iscsiPatchRequest is PATCH /transports/iscsi: an absent field is left
// as it is; portals [] removes them all, base_name "" removes it.
type iscsiPatchRequest struct {
	Portals  []string `json:"portals"`
	BaseName *string  `json:"base_name"`
	DryRun   bool     `json:"dry_run"`
}

// iscsiBindRequest is POST /transports/iscsi/bind.
type iscsiBindRequest struct {
	Library         *int     `json:"library"`
	Initiators      []string `json:"initiators"`
	TargetName      *string  `json:"target_name"`
	CHAP            *chapDTO `json:"chap"`
	ClearCHAP       bool     `json:"clear_chap"`
	MutualCHAP      *chapDTO `json:"mutual_chap"`
	ClearMutualCHAP bool     `json:"clear_mutual_chap"`
	DryRun          bool     `json:"dry_run"`
}

// iscsiUnbindRequest is POST /transports/iscsi/unbind.
type iscsiUnbindRequest struct {
	Library *int `json:"library"`
	DryRun  bool `json:"dry_run"`
}

// fieldErrorDetail is errorDetail with the request field at fault.
type fieldErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// iscsiTargetStateDTO is a bound target as configfs shows it after the
// apply. Present false is an answer, not an error.
type iscsiTargetStateDTO struct {
	WWN     string   `json:"wwn"`
	Present bool     `json:"present"`
	Enabled *bool    `json:"enabled"`
	LUNs    int      `json:"luns"`
	ACLs    []string `json:"acls"`
}

// iscsiWriteDTO answers the iSCSI writes: the helper's answer and the
// change, its diff with every secret masked.
type iscsiWriteDTO struct {
	DryRun        bool                 `json:"dry_run"`
	Action        string               `json:"action"`
	Diff          string               `json:"diff"`
	Result        string               `json:"result"`
	Message       string               `json:"message,omitempty"`
	Error         *errorDetail         `json:"error,omitempty"`
	CurrentSHA256 *string              `json:"current_sha256,omitempty"`
	JournalTail   string               `json:"journal_tail,omitempty"`
	RollbackDir   string               `json:"rollback_dir,omitempty"`
	Target        *iscsiTargetStateDTO `json:"target,omitempty"`
}

// fabricsInvalid is a change refused before the helper sees it.
type fabricsInvalid struct {
	status      int
	code, field string
	msg         string
}

// fieldKeywords attribute a conffile message to a request field, first
// match wins: "mutual chap" before "chap", "base name" before "target
// name" (CheckPlan's "no Target name and the iSCSI record no Base name").
var fieldKeywords = []struct{ word, field string }{
	{"mutual chap", "mutual_chap"},
	{"chap", "chap"},
	{"base name", "base_name"},
	{"derived name", "base_name"},
	{"initiator", "initiators"},
	{"target name", "target_name"},
	{"portal", "portals"},
	{"no library or drive", "library"},
	{"belongs to library", "library"},
	{"slot", "library"},
	{"lun", "library"},
}

// fieldOf is the request field msg is about, if it is one of fields.
func fieldOf(msg string, fields []string) string {
	m := strings.ToLower(msg)
	for _, k := range fieldKeywords {
		if strings.Contains(m, k.word) {
			for _, f := range fields {
				if f == k.field {
					return f
				}
			}
			return ""
		}
	}
	return ""
}

// maskAll replaces each secret in s, longest first so a secret that
// contains another is masked whole.
func maskAll(s string, secrets []string) string {
	sorted := append([]string(nil), secrets...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, v := range sorted {
		if v != "" {
			s = strings.ReplaceAll(s, v, conffile.SecretMask)
		}
	}
	return s
}

func (s *Server) handleISCSIPatch(w http.ResponseWriter, r *http.Request) {
	var req iscsiPatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	if req.Portals == nil && req.BaseName == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: give portals or base_name")
		return
	}
	s.fabricsWrite(w, r, req.DryRun, nil, []string{"portals", "base_name"}, nil,
		func(f fabricsFiles) *fabricsInvalid {
			if err := f.conf.SetISCSI(req.Portals, req.BaseName); err != nil {
				return &fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}
			}
			return nil
		})
}

func (s *Server) handleISCSIBind(w http.ResponseWriter, r *http.Request) {
	var req iscsiBindRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	if req.Library == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "library is required")
		return
	}
	if *req.Library < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "library must not be negative")
		return
	}
	id := *req.Library
	var secrets []string
	for _, c := range []*chapDTO{req.CHAP, req.MutualCHAP} {
		if c != nil {
			secrets = append(secrets, c.Secret)
		}
	}
	fields := []string{"library", "initiators", "target_name", "chap", "mutual_chap", "base_name"}
	s.fabricsWrite(w, r, req.DryRun, secrets, fields, &id, func(f fabricsFiles) *fabricsInvalid {
		bound := false
		for _, t := range f.conf.Targets() {
			bound = bound || t.ID == id
		}
		if !bound && len(req.Initiators) == 0 {
			return &fabricsInvalid{http.StatusUnprocessableEntity, apply.ErrInvalid, "initiators",
				fmt.Sprintf("library %d is not bound yet: initiators needs at least one initiator name", id)}
		}
		if _, _, present := f.conf.ISCSI(); !present {
			if err := f.conf.SetISCSI(nil, nil); err != nil {
				return &fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}
			}
		}
		err := f.conf.UpsertTarget(id, conffile.TargetEdit{
			Initiators: req.Initiators, TargetName: req.TargetName,
			CHAP: req.CHAP.creds(), ClearCHAP: req.ClearCHAP,
			MutualCHAP: req.MutualCHAP.creds(), ClearMutualCHAP: req.ClearMutualCHAP,
		})
		if err != nil {
			return &fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}
		}
		return nil
	})
}

func (s *Server) handleISCSIUnbind(w http.ResponseWriter, r *http.Request) {
	var req iscsiUnbindRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	if req.Library == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "library is required")
		return
	}
	if *req.Library < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "library must not be negative")
		return
	}
	id := *req.Library
	s.fabricsWrite(w, r, req.DryRun, nil, []string{"library"}, nil, func(f fabricsFiles) *fabricsInvalid {
		err := f.conf.RemoveTarget(id)
		switch {
		case errors.Is(err, conffile.ErrNotFound):
			return &fabricsInvalid{status: http.StatusNotFound, code: "not_bound",
				msg: fmt.Sprintf("library %d has no iSCSI target in %s", id, fabricsConfName)}
		case err != nil:
			return &fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}
		}
		return nil
	})
}

// handleTransportUnsupported answers the bind routes of a transport
// without one. It is gated like the writes it stands for.
func (s *Server) handleTransportUnsupported(w http.ResponseWriter, r *http.Request) {
	if !s.writeAllowed(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "unsupported_transport",
		"transport "+r.PathValue("type")+" cannot be configured through the API; only iscsi can")
}

// fabricsWrite reads fabrics.conf and device.conf, runs edit on the
// former, checks the result as mhvtl-fabrics would plan it, and sends it
// to the helper: fabrics_plan with dryRun, else fabrics. secrets are the
// request's own; fields are the request fields a refusal may name. With
// bind set, a real apply that reloads or starts the unit answers the
// target's state in configfs.
func (s *Server) fabricsWrite(w http.ResponseWriter, r *http.Request, dryRun bool, secrets, fields []string,
	bind *int, edit func(fabricsFiles) *fabricsInvalid) {
	f, err := s.readFabricsFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	secrets = append(secrets, conffile.FabricsSecrets(f.data)...)
	refuse := func(inv *fabricsInvalid) {
		writeJSON(w, inv.status, struct {
			Error fieldErrorDetail `json:"error"`
		}{fieldErrorDetail{Code: inv.code, Message: maskAll(inv.msg, secrets), Field: inv.field}})
	}
	if err := f.conf.Writable(); err != nil {
		// The file as read: nothing the request says is at fault.
		refuse(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()})
		return
	}
	if inv := edit(f); inv != nil {
		if inv.field == "" && inv.code == apply.ErrInvalid {
			inv.field = fieldOf(inv.msg, fields)
		}
		refuse(inv)
		return
	}
	if err := f.conf.Writable(); err != nil {
		refuse(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()})
		return
	}
	if ps := f.conf.CheckPlan(f.dev); len(ps) > 0 {
		msgs := make([]string, len(ps))
		for k, p := range ps {
			msgs[k] = p.Error()
		}
		refuse(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid,
			field: fieldOf(ps[0].Msg, fields), msg: fabricsConfName + ": " + strings.Join(msgs, "; ")})
		return
	}
	next := f.conf.Render()
	secrets = append(secrets, conffile.FabricsSecrets(next)...)

	hreq := apply.Request{Op: apply.OpFabricsPlan,
		Files: []apply.File{{Name: fabricsConfName, BaseSHA256: f.sha, Content: next}},
		Actor: "mhvtl-control API client " + r.RemoteAddr}
	ctx := r.Context()
	if !dryRun {
		hreq.Op = apply.OpFabrics
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
		writeError(w, http.StatusBadGateway, helperUnreachable, maskAll(err.Error(), secrets))
		return
	}
	status, code := applyStatus(resp)
	out := iscsiWriteDTO{
		DryRun: dryRun, Action: resp.FabricsAction, Diff: conffile.FabricsDiff(f.data, next), Result: resp.Result,
		Message: maskAll(resp.Message, secrets), CurrentSHA256: resp.CurrentSHA256,
		JournalTail: maskAll(resp.JournalTail, secrets), RollbackDir: resp.RollbackDir,
	}
	if code != "" {
		msg := applyMessage(resp)
		if resp.Result == apply.ResultConflict {
			msg = fabricsConfName + " changed since it was read: read it again and resend the change"
		}
		out.Error = &errorDetail{Code: code, Message: maskAll(msg, secrets)}
	}
	if bind != nil && !dryRun && resp.Result == apply.ResultApplied &&
		(resp.FabricsAction == apply.FabricsReload || resp.FabricsAction == apply.FabricsStart) {
		out.Target = s.boundTarget(f, *bind)
	}
	writeJSON(w, status, out)
}

// boundTarget reads target id's state from configfs, by the name
// mhvtl-fabrics gives it; nil when the name cannot be derived.
func (s *Server) boundTarget(f fabricsFiles, id int) *iscsiTargetStateDTO {
	_, base, _ := f.conf.ISCSI()
	var wwn string
	for _, t := range f.conf.Targets() {
		if t.ID == id {
			wwn = fabricsWWN(t, base, deviceOf(f.dev, id))
		}
	}
	if wwn == "" {
		return nil
	}
	st := &iscsiTargetStateDTO{WWN: wwn, ACLs: []string{}}
	for _, fab := range transports.Read(s.cfg.TargetRoot).Fabrics {
		if fab.Transport != "iscsi" {
			continue
		}
		for _, t := range fab.Targets {
			if t.WWN != wwn {
				continue
			}
			st.Present = true
			for k, tpg := range t.TPGs {
				if k == 0 {
					st.Enabled = tpg.Enabled
				}
				st.LUNs += len(tpg.LUNs)
				for _, a := range tpg.ACLs {
					st.ACLs = append(st.ACLs, a.Initiator)
				}
			}
		}
	}
	return st
}
