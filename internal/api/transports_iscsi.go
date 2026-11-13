package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
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
	stored := conffile.FabricsSecrets(f.data)
	for _, p := range ps {
		msg := quotedSecret.ReplaceAllString(conffile.MaskSecrets(p.Msg, stored), "'(not shown)'")
		out.Problems = append(out.Problems, iscsiProblemDTO{Line: p.Line, Message: msg})
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
//
// Every iSCSI write takes base_sha256: the sha256 of fabrics.conf the
// change was made against, as a write answer or GET /config gave it ("" for
// no file). When present, the change is refused with 409 unless the file
// is still that one.
type iscsiPatchRequest struct {
	Portals    []string `json:"portals"`
	BaseName   *string  `json:"base_name"`
	BaseSHA256 *string  `json:"base_sha256"`
	DryRun     bool     `json:"dry_run"`
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
	BaseSHA256      *string  `json:"base_sha256"`
	DryRun          bool     `json:"dry_run"`
}

// iscsiUnbindRequest is POST /transports/iscsi/unbind.
type iscsiUnbindRequest struct {
	Library    *int    `json:"library"`
	BaseSHA256 *string `json:"base_sha256"`
	DryRun     bool    `json:"dry_run"`
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
// change, its diff with every secret masked. SHA256 is fabrics.conf as the
// daemon read it ("" when absent), what an apply of the previewed change
// sends as base_sha256. Result "unchanged" is an edit that leaves the file
// as it is, not sent to the helper.
type iscsiWriteDTO struct {
	DryRun        bool                 `json:"dry_run"`
	Action        string               `json:"action"`
	Diff          string               `json:"diff"`
	SHA256        string               `json:"sha256"`
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

// fieldOf is the request field msg is about, if it is one of fields. Only
// the text before the first quote counts: a quoted value is the client's
// and may hold any of the words.
func fieldOf(msg string, fields []string) string {
	m := msg
	if i := strings.IndexAny(m, `'"`); i >= 0 {
		m = m[:i]
	}
	m = strings.ToLower(m)
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

// quotedSecret is a quoted name in a parser message that mentions a
// secret, such as "unknown iSCSI target field 'chap secret= pa'": a field
// name run into its value can carry the start of the secret.
var quotedSecret = regexp.MustCompile(`(?i)'[^']*secret[^']*'`)

// previewDiff is the fabrics.conf diff to answer: secret lines are masked
// by RedactFabrics, a removed line (from the file as read) also has every
// known secret masked, and an added line only the request's own. Added
// lines may quote values the client sent, and masking a stored secret
// there would tell it which of them is one.
func previewDiff(before, after []byte, own, stored []string) string {
	all := append(append([]string(nil), own...), stored...)
	lines := strings.SplitAfter(conffile.FabricsDiff(before, after), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "-"):
			lines[i] = conffile.MaskSecrets(l, all)
		case strings.HasPrefix(l, "+"):
			lines[i] = conffile.MaskSecrets(l, own)
		}
	}
	return strings.Join(lines, "")
}

// refusalText is msg with the given secrets masked and quoted names that
// mention a secret cut. A refusal of the edit masks only the request's own
// secrets: its message quotes values the client sent, and masking the
// secrets stored in fabrics.conf there would tell the client which of its
// values is one (a stored secret it quotes is one the client typed). A
// refusal of the file as read quotes no request value, so it masks those.
func refusalText(msg string, secrets []string) string {
	return quotedSecret.ReplaceAllString(conffile.MaskSecrets(msg, secrets), "'(not shown)'")
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
	s.fabricsWrite(w, r, fabricsWriteReq{dryRun: req.DryRun, base: req.BaseSHA256, fields: []string{"portals", "base_name"}},
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
	if req.Initiators != nil && len(req.Initiators) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error fieldErrorDetail `json:"error"`
		}{fieldErrorDetail{Code: apply.ErrInvalid, Field: "initiators", Message: fmt.Sprintf(
			"initiators is empty: a record without an initiator exports nothing (fabrics.conf(5)); to stop exporting library %d, unbind it", id)}})
		return
	}
	fields := []string{"library", "initiators", "target_name", "chap", "mutual_chap"}
	q := fabricsWriteReq{dryRun: req.DryRun, base: req.BaseSHA256, secrets: secrets, fields: fields, target: &id, bind: true}
	s.fabricsWrite(w, r, q, func(f fabricsFiles) *fabricsInvalid {
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
	q := fabricsWriteReq{dryRun: req.DryRun, base: req.BaseSHA256, fields: []string{"library"}, target: &id}
	s.fabricsWrite(w, r, q, func(f fabricsFiles) *fabricsInvalid {
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

// fabricsWriteReq is what fabricsWrite needs of an iSCSI write: base is
// the request's base_sha256 (nil when absent); secrets are the request's
// own; fields are the request fields a refusal may name; target is the
// library the request binds or unbinds, and bind is set for a bind.
type fabricsWriteReq struct {
	dryRun  bool
	base    *string
	secrets []string
	fields  []string
	target  *int
	bind    bool
}

// fabricsWrite reads fabrics.conf and device.conf, runs edit on the
// former, checks the result as mhvtl-fabrics would plan it, and sends it
// to the helper: fabrics_plan with dryRun, else fabrics. A base_sha256
// other than the file read is a conflict, and an edit that leaves the file
// as it is answers unchanged; neither reaches the helper. With bind set, a
// real apply that reloads or starts the unit answers the target's state in
// configfs.
func (s *Server) fabricsWrite(w http.ResponseWriter, r *http.Request, q fabricsWriteReq, edit func(fabricsFiles) *fabricsInvalid) {
	f, err := s.readFabricsFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	if q.base != nil && *q.base != f.sha {
		cur := f.sha
		writeJSON(w, http.StatusConflict, iscsiWriteDTO{DryRun: q.dryRun, SHA256: f.sha, Result: apply.ResultConflict,
			CurrentSHA256: &cur, Error: &errorDetail{Code: apply.ResultConflict, Message: fabricsChanged}})
		return
	}
	own, stored := q.secrets, conffile.FabricsSecrets(f.data)
	secrets := append(append([]string(nil), own...), stored...)
	refuseWith := func(inv *fabricsInvalid, mask []string) {
		writeJSON(w, inv.status, struct {
			Error  fieldErrorDetail `json:"error"`
			SHA256 string           `json:"sha256"`
		}{fieldErrorDetail{Code: inv.code, Message: refusalText(inv.msg, mask), Field: inv.field}, f.sha})
	}
	refuse := func(inv *fabricsInvalid) { refuseWith(inv, own) }
	if err := f.conf.Writable(); err != nil {
		// The file as read: nothing the request says is at fault.
		refuseWith(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}, stored)
		return
	}
	if inv := edit(f); inv != nil {
		if inv.field == "" && inv.code == apply.ErrInvalid {
			inv.field = fieldOf(inv.msg, q.fields)
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
		field := ""
		for k, p := range ps {
			msgs[k] = p.Error()
			if field == "" && aboutTarget(f.conf, p, q.target) {
				field = fieldOf(p.Msg, q.fields)
			}
		}
		refuse(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid,
			field: field, msg: fabricsConfName + ": " + strings.Join(msgs, "; ")})
		return
	}
	next := f.conf.Render()
	if f.exists && bytes.Equal(next, f.data) {
		writeJSON(w, http.StatusOK, iscsiWriteDTO{DryRun: q.dryRun, SHA256: f.sha, Result: resultUnchanged})
		return
	}
	secrets = append(secrets, conffile.FabricsSecrets(next)...)

	hreq := apply.Request{Op: apply.OpFabricsPlan,
		Files: []apply.File{{Name: fabricsConfName, BaseSHA256: f.sha, Content: next}},
		Actor: "mhvtl-control API client " + r.RemoteAddr}
	if q.base != nil {
		hreq.Files[0].BaseSHA256 = *q.base
	}
	ctx := r.Context()
	if !q.dryRun {
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
		writeError(w, http.StatusBadGateway, helperUnreachable, conffile.MaskSecrets(err.Error(), secrets))
		return
	}
	status, code := applyStatus(resp)
	out := iscsiWriteDTO{
		DryRun: q.dryRun, Action: resp.FabricsAction, Diff: previewDiff(f.data, next, own, stored),
		SHA256: f.sha, Result: resp.Result, Message: conffile.MaskSecrets(resp.Message, secrets), CurrentSHA256: resp.CurrentSHA256,
		JournalTail: conffile.MaskSecrets(resp.JournalTail, secrets), RollbackDir: resp.RollbackDir,
	}
	if code != "" {
		msg := applyMessage(resp)
		if resp.Result == apply.ResultConflict {
			msg = fabricsChanged
		}
		out.Error = &errorDetail{Code: code, Message: conffile.MaskSecrets(msg, secrets)}
	}
	if q.bind && !q.dryRun && resp.Result == apply.ResultApplied &&
		(resp.FabricsAction == apply.FabricsReload || resp.FabricsAction == apply.FabricsStart) {
		out.Target = s.boundTarget(f, *q.target)
	}
	writeJSON(w, status, out)
}

const (
	// resultUnchanged answers an edit that leaves fabrics.conf as it is.
	resultUnchanged = "unchanged"
	fabricsChanged  = fabricsConfName + " changed since it was read: read it again and resend the change"
)

// aboutTarget reports whether CheckPlan problem p is about the target the
// request edits: its message names "iSCSI target <id>", or its line is
// that record's header. Without a target (PATCH) every problem is the
// request's.
func aboutTarget(c *conffile.FabricsConf, p conffile.Problem, target *int) bool {
	if target == nil {
		return true
	}
	if regexp.MustCompile(fmt.Sprintf(`\biSCSI target %d\b`, *target)).MatchString(p.Msg) {
		return true
	}
	for _, t := range c.Targets() {
		if t.ID == *target {
			return p.Line > 0 && p.Line == t.Line
		}
	}
	return false
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
