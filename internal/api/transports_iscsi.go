package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
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
// names only. Without an API token any local user can read it, so the CHAP
// names, half of each credential, are then left out.
type iscsiTargetDTO struct {
	ID         int    `json:"id"`
	TargetName string `json:"target_name"`
	// WWN is the name mhvtl-fabrics gives the target: the Target name,
	// else the Base name and ".lib<ID>" or ".tape<ID>"; "" when neither
	// can be had.
	WWN            string          `json:"wwn"`
	Initiators     []string        `json:"initiators"`
	CHAP           bool            `json:"chap"`
	CHAPName       string          `json:"chap_name,omitempty"`
	MutualCHAP     bool            `json:"mutual_chap"`
	MutualCHAPName string          `json:"mutual_chap_name,omitempty"`
	Device         *iscsiDeviceDTO `json:"device"`
}

// iscsiProblemDTO is one problem of fabrics.conf. Kind "parse" is a line
// mhvtl-fabrics would not read: no edit can be made until it is fixed by
// hand. Kind "plan" is a target it reads but would not export against
// device.conf (CheckPlan); Target is that target's ID, absent for a
// device.conf limit. An edit that removes or fixes the target is accepted.
type iscsiProblemDTO struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
	Kind    string `json:"kind"`
	Target  *int   `json:"target,omitempty"`
}

// iscsiConfigDTO is GET /transports/iscsi/config. SHA256 is fabrics.conf's
// version (fabricsFiles.version), left out without an API token: only the
// writes, which need the token, take it back. It is also left out while a
// write of fabrics.conf is being applied (fabricsFiles.busy).
type iscsiConfigDTO struct {
	Exists   bool              `json:"exists"`
	SHA256   *string           `json:"sha256,omitempty"`
	ISCSI    iscsiRecordDTO    `json:"iscsi"`
	Targets  []iscsiTargetDTO  `json:"targets"`
	Problems []iscsiProblemDTO `json:"problems"`
	Warnings []string          `json:"warnings"`
}

// fabricsFiles is fabrics.conf and device.conf as read for one request.
// sha, the file's SHA-256, goes to the helper only. version is what
// clients get and send back as sha256 and base_sha256: an HMAC-SHA256 of
// the file under a key made when the daemon starts (fabricsVersion). The
// file holds CHAP secrets, so its plain hash would let a client test
// guesses at them offline.
//
// busy means a write of fabrics.conf the daemon sent the helper was in
// flight while the file was read: version is then "" and must not be
// answered, nor a base_sha256 accepted. The helper writes the file before
// it reloads mhvtl-fabrics, for minutes at worst, and the revision in the
// version only advances when it answers: a version of the new file under
// the old revision would equal the version before the write exactly when
// the write left the file as it was, so it would confirm a CHAP guess.
//
// seq is the sequence (Server.fabricsSeq) the read was made at: a helper
// call is answered from what the helper said only when no other one
// started or ended since (fabricsWrite).
type fabricsFiles struct {
	seq     uint64
	data    []byte
	sha     string // "" when fabrics.conf does not exist
	version string // "" when fabrics.conf does not exist or busy
	busy    bool
	exists  bool
	conf    *conffile.FabricsConf
	dev     *conffile.DeviceConf
}

// readFabricsFiles reads fabrics.conf and device.conf from the
// configuration directory; a file that does not exist is empty. A read
// error names the file only: the absolute path goes to the log.
func (s *Server) readFabricsFiles() (fabricsFiles, error) {
	var f fabricsFiles
	s.fabricsMu.Lock()
	rev, seq, writing := s.fabricsRev, s.fabricsSeq, s.fabricsWriting > 0
	s.fabricsMu.Unlock()
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
	// A write that started after seq was taken advanced it: the file read
	// may be the new one, and its version under rev must not be answered.
	s.fabricsMu.Lock()
	f.busy = writing || s.fabricsWriting > 0 || s.fabricsSeq != seq
	s.fabricsMu.Unlock()
	f.seq = seq
	if f.exists && !f.busy {
		f.version = s.fabricsVersion(rev, data)
	}
	return f, nil
}

// versionOf is f's version as answered: nil when busy.
func (f fabricsFiles) versionOf() *string {
	if f.busy {
		return nil
	}
	v := f.version
	return &v
}

// fabricsVersion is the opaque version of fabrics.conf content data: the
// HMAC-SHA256 under s.fabricsKey of revision rev (s.fabricsRev) and data,
// in hex. It changes when the file does, after every write the daemon
// sends the helper, and when the daemon restarts. A write that leaves the
// file byte-identical, such as a CHAP secret guess equal to the stored
// one, still changes it: otherwise a version unchanged after the write
// would confirm the guess.
func (s *Server) fabricsVersion(rev uint64, data []byte) string {
	m := hmac.New(sha256.New, s.fabricsKey)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], rev)
	m.Write(b[:])
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// helperDo sends hreq to the helper; every helper call of the daemon
// goes through it. Any request but units and hello takes the helper's
// lock, and the first to find a dead apply's intent recovers it, which
// may put back fabrics.conf whatever the request was for (apply.Helper.
// Handle): a write of the file or not, a plan included. So while such a
// call is in flight no version of fabrics.conf is answered
// (fabricsFiles.busy), and after it the revision of the version advances
// whatever the answer, since the helper may have written the file even
// when the answer is an error or never came. Otherwise comparing the
// version before and after a recovery would tell whether the file it put
// back is byte-identical to the dead write's, a CHAP guess right. Only a
// call that never reached the helper leaves the revision: no helper on
// the socket (applyclient.ErrAbsent) or a failed connect.
func (s *Server) helperDo(ctx context.Context, hreq apply.Request) (resp apply.Response, err error) {
	if seqSteps(hreq.Op) > 0 {
		s.fabricsMu.Lock()
		s.fabricsWriting++
		s.fabricsSeq++
		s.fabricsMu.Unlock()
		reached := true
		defer func() {
			s.fabricsMu.Lock()
			if reached {
				s.fabricsRev++
			}
			s.fabricsSeq++
			s.fabricsWriting--
			s.fabricsMu.Unlock()
		}()
		resp, err = s.cfg.Helper.Do(ctx, hreq)
		var op *net.OpError
		reached = !errors.Is(err, applyclient.ErrAbsent) && !(errors.As(err, &op) && op.Op == "dial")
		return resp, err
	}
	return s.cfg.Helper.Do(ctx, hreq)
}

// seqSteps is how far a helper call of op advances fabricsSeq: by one
// when it is sent and one when it is answered for a call that may change
// fabrics.conf, else not at all. Units takes no lock and never recovers;
// hello (HelperService.Hello) neither.
func seqSteps(op string) uint64 {
	if op == apply.OpUnits || op == apply.OpHello {
		return 0
	}
	return 2
}

// currentFabricsVersion is the version of fabrics.conf as it is now, for
// a conflict the helper found: "" when it does not exist, nil when it
// cannot be read or a write of it is in flight.
func (s *Server) currentFabricsVersion() *string {
	f, err := s.readFabricsFiles()
	if err != nil {
		return nil
	}
	return f.versionOf()
}

// versionSince is the version of fabrics.conf as it is now, read after a
// helper call: "" when it does not exist, nil when it cannot be read, or
// when the sequence is not seq, the one a read before the call was made
// at advanced by the call's own steps: another call that may have changed
// the file started or ended meanwhile, or is in flight.
func (s *Server) versionSince(seq uint64) *string {
	g, err := s.readFabricsFiles()
	if err != nil || g.seq != seq {
		return nil
	}
	return g.versionOf()
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
	out.Exists = f.exists
	if s.cfg.Token != "" {
		out.SHA256 = f.versionOf()
	}
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
		dto := iscsiTargetDTO{
			ID: t.ID, TargetName: t.TargetName, WWN: fabricsWWN(t, base, d), Initiators: inits,
			CHAP: t.HasCHAP, MutualCHAP: t.HasMutualCHAP, Device: d,
		}
		if s.cfg.Token != "" {
			dto.CHAPName, dto.MutualCHAPName = t.CHAPName, t.MutualCHAPName
		}
		out.Targets = append(out.Targets, dto)
	}
	ps, kind := f.conf.Problems, "parse"
	if len(ps) == 0 {
		// CheckPlan is for a file without parse problems.
		ps, kind = f.conf.CheckPlan(f.dev), "plan"
	}
	ids := map[int]int{}
	for _, t := range f.conf.Targets() {
		ids[t.Line] = t.ID
	}
	stored := conffile.FabricsSecrets(f.data)
	for _, p := range ps {
		dto := iscsiProblemDTO{Line: p.Line, Message: refusalText(p.Msg, stored), Kind: kind}
		if s.cfg.Token == "" {
			dto.Message = quotedCHAP.ReplaceAllString(dto.Message, "'(not shown)'")
		}
		if id, ok := ids[p.Line]; ok && kind == "plan" && p.Line > 0 {
			dto.Target = &id
		}
		out.Problems = append(out.Problems, dto)
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
// Every iSCSI write takes base_sha256: the sha256 (fabrics.conf's opaque
// version, fabricsFiles.version) the change was made against, as a write
// answer or GET /config gave it ("" for no file). When present, the change
// is refused with 409 unless the file is still that one.
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
// change, its diff with every secret masked. SHA256 is the version of
// fabrics.conf ("" when absent): for an answer of the helper, as the daemon
// read it again after that answer, what an apply of the previewed change
// sends as base_sha256; else as read for the request. CurrentSHA256 is
// that of the file a conflict found. Both are left out while a write of
// the file is in flight (fabricsFiles.busy), and from a helper's answer
// when another call that may have changed the file started or ended since
// the request read it (versionSince). Result "unchanged" is an edit that
// leaves the file as it is, not sent to the helper.
type iscsiWriteDTO struct {
	DryRun        bool                 `json:"dry_run"`
	Action        string               `json:"action"`
	Diff          string               `json:"diff"`
	SHA256        *string              `json:"sha256,omitempty"`
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

// quotedCHAP is a quoted name in a parser message that mentions CHAP or a
// secret, such as "unknown iSCSI target field 'chap name pbs1 x'": a CHAP
// name run into the field name. GET config cuts these when it has no API
// token to answer CHAP names to.
var quotedCHAP = regexp.MustCompile(`(?i)'[^']*(?:secret|chap)[^']*'`)

// previewDiff is the fabrics.conf diff to answer. Secret lines are masked
// by RedactFabrics. In every line the request's own secrets are masked,
// and the stored ones (stored) by conffile.MaskFileLine: in comments,
// trailing comments (also those rewriteValue keeps on a changed line) and
// lines of any other kind, but not in the value of a field the client sets
// and GET config shows in clear, such as a CHAP name or an initiator.
// Masking a stored secret there would tell the client that a value it
// chose, now or in an earlier apply, contains one.
func previewDiff(before, after []byte, own, stored []string) string {
	lines := strings.SplitAfter(conffile.FabricsDiff(before, after), "\n")
	// The first three lines are FabricsDiff's header.
	for i := 3; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			continue
		}
		body, nl := strings.CutSuffix(l[1:], "\n")
		lines[i] = l[:1] + conffile.MaskSecrets(conffile.MaskFileLine(body, stored), own)
		if nl {
			lines[i] += "\n"
		}
	}
	return strings.Join(lines, "")
}

// refusalText is msg with the given secrets masked and quoted names that
// mention a secret cut. A refusal of the edit masks only the request's own
// secrets: its message quotes values the client sent, and masking the
// secrets stored in fabrics.conf there would tell the client which of its
// values is one (a stored secret it quotes is one the client typed). A
// refusal of the file as read, like a problem GET config lists, quotes no
// request value, so it masks those.
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
	q := fabricsWriteReq{dryRun: req.DryRun, base: req.BaseSHA256, secrets: secrets, fields: fields, target: &id, bind: true,
		setsCHAP: req.CHAP != nil, setsMutualCHAP: req.MutualCHAP != nil}
	s.fabricsWrite(w, r, q, func(f fabricsFiles) *fabricsInvalid {
		bound := false
		for _, t := range f.conf.Targets() {
			if t.ID != id {
				continue
			}
			bound = true
			if inv := secretPair(req, t); inv != nil {
				return inv
			}
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

// secretPair refuses a bind that sets one CHAP secret of a target whose
// other-direction secret is stored, without sending that one too.
// fabrics.conf(5) requires the mutual CHAP secret to differ from the CHAP
// secret (check_target,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L321-L348),
// so such a bind would be refused exactly when the secret sent equals the
// stored one: a dry run would tell the client a stored secret. The refusal
// depends only on which fields the request sends, so the two secrets are
// compared only when both come in the same request. Clearing mutual CHAP,
// or CHAP with it, compares nothing.
func secretPair(req iscsiBindRequest, t conffile.FabricsTarget) *fabricsInvalid {
	if req.ClearCHAP {
		return nil
	}
	switch {
	case req.CHAP != nil && req.MutualCHAP == nil && !req.ClearMutualCHAP && t.HasMutualCHAP:
		return &fabricsInvalid{http.StatusUnprocessableEntity, apply.ErrInvalid, "mutual_chap", fmt.Sprintf(
			"library %d has mutual CHAP: send both CHAP secrets when changing either while mutual CHAP is set (chap and mutual_chap), or set clear_mutual_chap", t.ID)}
	case req.MutualCHAP != nil && req.CHAP == nil && t.HasCHAP:
		return &fabricsInvalid{http.StatusUnprocessableEntity, apply.ErrInvalid, "chap", fmt.Sprintf(
			"library %d has CHAP: send both CHAP secrets when setting mutual CHAP (chap and mutual_chap)", t.ID)}
	}
	return nil
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
	q := fabricsWriteReq{dryRun: req.DryRun, base: req.BaseSHA256, fields: []string{"library"}, target: &id, unbind: true}
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
// library the request binds or unbinds, and bind is set for a bind, unbind
// for an unbind; setsCHAP and setsMutualCHAP are set when it sends that
// secret.
type fabricsWriteReq struct {
	dryRun                   bool
	base                     *string
	secrets                  []string
	fields                   []string
	target                   *int
	bind, unbind             bool
	setsCHAP, setsMutualCHAP bool
}

// fabricsWrite reads fabrics.conf and device.conf, runs edit on the
// former, checks the result as mhvtl-fabrics would plan it, and sends it
// to the helper: fabrics_plan with dryRun, else fabrics. A base_sha256
// other than the file read is a conflict, and an edit that leaves the file
// as it is answers unchanged; neither reaches the helper. An edit that
// sends a CHAP or mutual CHAP secret never answers unchanged, and its
// preview shows the secret line as removed and added even when the value
// is the stored one (conffile.PlaceholdSecrets): either would tell the
// client whether its secret is the stored one. With bind set, a real apply
// that reloads or starts the unit answers the target's state in configfs.
//
// The helper's validated or conflict compares the content sent against
// the file as it is then, so the base it gets must be the file as it was
// when the helper last answered: a base from before another write, or
// before a write that is still in flight, would have it tell whether that
// write left the file byte-identical, a CHAP guess right. So real writes
// are serialized (Server.fabricsWriteMu), a request read while one is in
// flight is answered apply_in_progress, and a request during which
// another helper call started or ended (helperDo) is answered a conflict
// that does not depend on what the helper said.
func (s *Server) fabricsWrite(w http.ResponseWriter, r *http.Request, q fabricsWriteReq, edit func(fabricsFiles) *fabricsInvalid) {
	if !q.dryRun {
		if !s.fabricsWriteMu.TryLock() {
			writeFabricsBusy(w, q.dryRun)
			return
		}
		defer s.fabricsWriteMu.Unlock()
	}
	f, err := s.readFabricsFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	// The file read may already be the new one of the write in flight.
	if f.busy {
		writeFabricsBusy(w, q.dryRun)
		return
	}
	if q.base != nil && !hmac.Equal([]byte(*q.base), []byte(f.version)) {
		writeJSON(w, http.StatusConflict, iscsiWriteDTO{DryRun: q.dryRun, SHA256: f.versionOf(), Result: apply.ResultConflict,
			CurrentSHA256: f.versionOf(), Error: &errorDetail{Code: apply.ResultConflict, Message: fabricsChanged}})
		return
	}
	own, stored := q.secrets, conffile.FabricsSecrets(f.data)
	refuseWith := func(inv *fabricsInvalid, mask []string) {
		writeJSON(w, inv.status, struct {
			Error  fieldErrorDetail `json:"error"`
			SHA256 *string          `json:"sha256,omitempty"`
		}{fieldErrorDetail{Code: inv.code, Message: refusalText(inv.msg, mask), Field: inv.field}, f.versionOf()})
	}
	refuse := func(inv *fabricsInvalid) { refuseWith(inv, own) }
	if err := f.conf.Writable(); err != nil {
		// The file as read: nothing the request says is at fault.
		refuseWith(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, msg: err.Error()}, stored)
		return
	}
	// An unbind is refused only for plan problems it adds, like the helper
	// (apply.FabricsNotApplied): with two broken targets, each can then be
	// unbound while the other is still there.
	had := map[string]bool{}
	if q.unbind {
		for _, p := range f.conf.CheckPlan(f.dev) {
			had[p.Msg] = true
		}
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
	var ps []conffile.Problem
	for _, p := range f.conf.CheckPlan(f.dev) {
		if !had[p.Msg] {
			ps = append(ps, p)
		}
	}
	if len(ps) > 0 {
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
	// An unbind that keeps every problem would only be written, not
	// applied: the target would stay exported. It must unbind a broken
	// target, as the helper requires.
	if q.unbind && len(had) > 0 {
		kept := map[string]bool{}
		for _, p := range f.conf.CheckPlan(f.dev) {
			kept[p.Msg] = true
		}
		if len(kept) >= len(had) {
			broken := brokenTargetIDs(conffile.ParseFabricsConf(f.data), f.dev)
			next := "fix device.conf first"
			if len(broken) > 0 {
				next = "unbind the broken targets first (" + strings.Join(broken, ", ") + ")"
			}
			refuse(&fabricsInvalid{status: http.StatusUnprocessableEntity, code: apply.ErrInvalid, field: "library",
				msg: fmt.Sprintf("library %d's target exports fine, but %s has targets mhvtl-fabrics would not export: with them in the file the unbind could only be written, not applied, and library %d would stay exported; %s",
					*q.target, fabricsConfName, *q.target, next)})
			return
		}
	}
	next := f.conf.Render()
	setsSecret := q.setsCHAP || q.setsMutualCHAP
	if f.exists && bytes.Equal(next, f.data) && !setsSecret {
		writeJSON(w, http.StatusOK, iscsiWriteDTO{DryRun: q.dryRun, SHA256: f.versionOf(), Result: resultUnchanged})
		return
	}
	// The helper's answers quote secrets and names the client set. The
	// request's own secrets are masked everywhere. The journal tail may
	// quote a stored secret anywhere: there the files' secrets are masked
	// except inside the names the stored file has, and the names the
	// request brings are masked whole (conffile.MaskSecretsExcept), as the
	// helper does. The helper's messages quote output from outside only
	// masked that way, and may name a new name, which is kept: the files'
	// secrets are masked except inside the names of either file. The names
	// include the target names each file plans (PlannedWWNs): a derived
	// one, the Base name and ".lib<ID>" or ".tape<ID>", may hold a secret
	// that spans the two, which must not be masked only when it is the
	// guess.
	fileSecrets := append(append([]string(nil), stored...), conffile.FabricsSecrets(next)...)
	storedWWNs := conffile.ParseFabricsConf(f.data).PlannedWWNs(f.dev)
	storedNames := append(conffile.FabricsClearValues(f.data), storedWWNs...)
	newNames := conffile.FabricsNewValues(f.data, next)
	allNames := append(append([]string(nil), storedNames...), conffile.FabricsClearValues(next)...)
	plannedBefore := map[string]bool{}
	for _, w := range storedWWNs {
		plannedBefore[w] = true
	}
	for _, w := range f.conf.PlannedWWNs(f.dev) {
		allNames = append(allNames, w)
		if !plannedBefore[w] {
			newNames = append(newNames, w)
		}
	}
	external := func(s string) string {
		return conffile.MaskSecrets(conffile.MaskSecretsExcept(conffile.CutHashes(s), fileSecrets, storedNames, newNames), own)
	}
	mask := func(s string) string {
		return conffile.MaskSecrets(conffile.MaskSecretsExcept(conffile.CutHashes(s), fileSecrets, allNames, nil), own)
	}

	// The helper checks the file's real hash: base_sha256, when sent, was
	// found to be the version of this very content above.
	hreq := apply.Request{Op: apply.OpFabricsPlan,
		Files: []apply.File{{Name: fabricsConfName, BaseSHA256: f.sha, Content: next}},
		Actor: "mhvtl-control API client " + r.RemoteAddr}
	ctx := r.Context()
	if !q.dryRun {
		hreq.Op = apply.OpFabrics
		// The apply runs to its end even if the client goes away.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
		defer cancel()
	}
	if s.beforeFabricsHelper != nil {
		s.beforeFabricsHelper()
	}
	resp, err := s.helperDo(ctx, hreq)
	s.forgetHello()
	if errors.Is(err, applyclient.ErrAbsent) {
		writeHelperAbsent(w)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, helperUnreachable, mask(err.Error()))
		return
	}
	if !s.fabricsQuiet(f.seq + seqSteps(hreq.Op)) {
		// Another call may have changed the file between the read and the
		// helper's check, a recovery included: what the helper said is not
		// answered. A write may have been made, which the client reads.
		out := iscsiWriteDTO{DryRun: q.dryRun, Result: apply.ResultConflict,
			Error: &errorDetail{Code: apply.ResultConflict, Message: fabricsChanged}}
		if !q.dryRun {
			out.Result, out.Error.Message = resultUnknown, fabricsOverlapped
		}
		writeJSON(w, http.StatusConflict, out)
		return
	}
	status, code := applyStatus(resp)
	before := f.data
	if setsSecret {
		before = conffile.PlaceholdSecrets(before, *q.target, q.setsCHAP, q.setsMutualCHAP)
	}
	// The version answered is the file's after the call, read anew: the
	// one read before it is stale once a write reached the helper.
	now := s.versionSince(f.seq + seqSteps(hreq.Op))
	out := iscsiWriteDTO{
		DryRun: q.dryRun, Action: resp.FabricsAction, Diff: previewDiff(before, next, own, stored),
		SHA256: now, Result: resp.Result, Message: mask(resp.Message),
		JournalTail: external(resp.JournalTail), RollbackDir: resp.RollbackDir,
	}
	if resp.Result == apply.ResultConflict {
		// The helper answers the file's real hash, in current_sha256 and
		// in its message; the client gets its version.
		out.CurrentSHA256 = now
		out.Message = fabricsChanged
	}
	if code != "" {
		msg := applyMessage(resp)
		if resp.Result == apply.ResultConflict {
			msg = fabricsChanged
		}
		out.Error = &errorDetail{Code: code, Message: mask(msg)}
	}
	if q.bind && !q.dryRun && resp.Result == apply.ResultApplied &&
		(resp.FabricsAction == apply.FabricsReload || resp.FabricsAction == apply.FabricsStart) {
		out.Target = s.boundTarget(conffile.ParseFabricsConf(next), f.dev, *q.target)
	}
	writeJSON(w, status, out)
}

const (
	// resultUnchanged answers an edit that leaves fabrics.conf as it is.
	resultUnchanged = "unchanged"
	fabricsChanged  = fabricsConfName + " changed since it was read: read it again and resend the change"
	fabricsBusy     = "a change is being applied: read " + fabricsConfName + " again once it has finished, and resend the change"
	// resultUnknown answers a write another helper call overlapped.
	resultUnknown     = "unknown"
	fabricsOverlapped = "another request reached the apply helper while this change was sent, so its answer is not passed on: read " +
		fabricsConfName + " again to see whether the change was made"
)

// fabricsQuiet reports whether fabricsSeq is seq and no helper call that
// moves it is in flight: none started or ended since a read made at seq,
// but the caller's own.
func (s *Server) fabricsQuiet(seq uint64) bool {
	s.fabricsMu.Lock()
	defer s.fabricsMu.Unlock()
	return s.fabricsSeq == seq && s.fabricsWriting == 0
}

// writeFabricsBusy answers an iSCSI write that came while another write
// was being applied: the same answer whatever the request and the file.
func writeFabricsBusy(w http.ResponseWriter, dryRun bool) {
	writeJSON(w, http.StatusConflict, iscsiWriteDTO{DryRun: dryRun, Result: apply.ResultInProgress,
		Error: &errorDetail{Code: apply.ResultInProgress, Message: fabricsBusy}})
}

// brokenTargetIDs lists the IDs of the targets of c with a plan problem
// against dev.
func brokenTargetIDs(c *conffile.FabricsConf, dev *conffile.DeviceConf) []string {
	ids := map[int]int{}
	for _, t := range c.Targets() {
		ids[t.Line] = t.ID
	}
	var out []string
	seen := map[int]bool{}
	for _, p := range c.CheckPlan(dev) {
		if id, ok := ids[p.Line]; ok && p.Line > 0 && !seen[id] {
			seen[id] = true
			out = append(out, fmt.Sprint(id))
		}
	}
	return out
}

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
// mhvtl-fabrics gives it in conf, the file as applied; nil when the name
// cannot be derived.
func (s *Server) boundTarget(conf *conffile.FabricsConf, dev *conffile.DeviceConf, id int) *iscsiTargetStateDTO {
	_, base, _ := conf.ISCSI()
	var wwn string
	for _, t := range conf.Targets() {
		if t.ID == id {
			wwn = fabricsWWN(t, base, deviceOf(dev, id))
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
