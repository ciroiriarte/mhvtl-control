package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/conffile"
)

// fabrics.conf is mhVTL's iSCSI export configuration, read by
// mhvtl-fabrics (fabrics.conf(5), markh794/mhvtl man/fabrics.conf.5.in at
// 19b71bd,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/man/fabrics.conf.5.in).
// The fabrics ops write it and have mhvtl-fabrics.service apply it. They
// never write configfs themselves: the unit's own sandbox does
// (mhvtl-control#6).
const (
	fabricsConf = "fabrics.conf"
	// FabricsUnit applies fabrics.conf: ExecStart and ExecReload run
	// mhvtl-fabrics apply, ExecStopPost runs remove.
	FabricsUnit = "mhvtl-fabrics.service"
	targetUnit  = "mhvtl.target"
	// fabricsTimeout bounds the reload or start: the unit's
	// TimeoutStartSec (300 s, which also bounds a reload,
	// systemd.service(5)) and a margin.
	fabricsTimeout = 330 * time.Second
)

// Fabrics actions: what an apply does after it writes fabrics.conf.
const (
	// FabricsReload is systemctl reload of the active unit: apply
	// reconciles in place, and sessions to targets the edit leaves alone
	// survive. A restart would run ExecStopPost=remove first and drop every
	// session, so the helper never restarts it.
	FabricsReload = "reload"
	// FabricsStart is systemctl start of a unit that is inactive because
	// fabrics.conf did not exist when mhvtl.target started
	// (ConditionPathExists), or failed in an earlier first start whose
	// file the rollback removed.
	FabricsStart = "start"
	// FabricsDeferred writes the file only: mhvtl.target is not running,
	// and the unit applies the file when it starts.
	FabricsDeferred = "none"
	// FabricsNotApplied writes the file only: it still has targets
	// mhvtl-fabrics would not plan, which were there before the change, so
	// a reload or start would fail (fab_plan stops at the first problem,
	// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L537-L611).
	// The change that leaves none reloads or starts the unit. This lets an
	// operator unbind broken targets one at a time.
	FabricsNotApplied = "not_applied"
)

// Fabrics refusals.
const (
	// ErrFabricsBackend: the effective MHVTL_BACKEND is not tcmu, so
	// mhvtl-fabrics apply would exit 0 without doing anything.
	ErrFabricsBackend = "fabrics_backend"
	// ErrFabricsUnit: mhvtl-fabrics.service is not installed, or cannot
	// reload (no ExecReload).
	ErrFabricsUnit = "fabrics_unit"
	// ErrFabricsState: the unit is in a state the helper does not guess
	// its way out of (failed, changing, or stopped while mhvtl.target
	// runs), or a rollback could not make the targets follow the restored
	// file.
	ErrFabricsState = "fabrics_recovery_required"
	// ErrFabricsApply: the reload or start failed; the file was restored.
	ErrFabricsApply = "fabrics_apply_failed"
)

// fabricsOp reports whether op is a fabrics op.
func fabricsOp(op string) bool { return op == OpFabricsPlan || op == OpFabrics }

// checkFabricsRequest refuses a fabrics op that carries anything but
// fabrics.conf: one file, no deletes, dispositions, units, force token,
// action, library or target.
func checkFabricsRequest(req *Request) error {
	if len(req.Files) != 1 || req.Files[0].Name != fabricsConf {
		return refuse(ErrName, "%s writes %s and nothing else", req.Op, fabricsConf)
	}
	if len(req.Delete)+len(req.Dispositions)+len(req.ExpectUnits) > 0 || req.ForceToken != "" ||
		req.Action != "" || req.Library != nil || req.Target {
		return refuse(ErrBadRequest, "%s takes only %s", req.Op, fabricsConf)
	}
	if req.Files[0].BaseSHA256 != "" && !shaRE.MatchString(req.Files[0].BaseSHA256) {
		return refuse(ErrBadRequest, "base_sha256 of %s is not a SHA-256", fabricsConf)
	}
	return nil
}

// fabrics checks fabrics.conf and, for OpFabrics, writes it and has the
// unit apply it. The caller holds exclusive.lock and found no intent.
func (h *Helper) fabrics(ctx context.Context, req *Request, rec *AuditRecord) Response {
	// As for every other write: the daemon's options file, which holds the
	// API token, must not sit in the directory the helper writes.
	if err := h.checkLegacyEnv(); err != nil {
		return responseOf(err)
	}
	// A plan does not act: while the marker exists it only says that the
	// apply retries first. Refusing it would leave the dashboard, which
	// applies only what it previewed, no way to make the retry happen.
	var markerNote string
	if req.Op == OpFabricsPlan {
		note, err := h.peekMarker()
		if err != nil {
			return responseOf(err)
		}
		markerNote = note
	} else if err := h.retryReconcile(ctx, rec); err != nil {
		return responseOf(err)
	}
	f := req.Files[0]
	files := newFileSet(h.cfg.ConfigDir)
	cur, err := files.current(fabricsConf)
	if err != nil {
		return responseOf(err)
	}
	next := f.Content
	rec.Files = []AuditFile{{Name: fabricsConf, Before: cur.sha, After: conffile.Hash(next)}}
	rec.Diff, rec.DiffTruncated = fabricsDiff(cur.data, next)
	if cur.sha != f.BaseSHA256 {
		return responseOf(&conffile.ConflictError{Path: filepath.Join(h.cfg.ConfigDir, fabricsConf), Current: cur.sha})
	}
	// Content equal to the file is applied like any other: refusing it
	// would tell the daemon's client that a CHAP secret it sent is the
	// stored one.
	conf := conffile.ParseFabricsConf(next)
	if err := conf.Writable(); err != nil {
		return responseOf(refuse(ErrInvalid, "%v", err))
	}
	left, curBroken, err := checkPlan(conf, cur, files)
	if err != nil {
		return responseOf(err)
	}
	if err := h.checkForeignTargets(conf, files); err != nil {
		return responseOf(err)
	}
	if err := h.checkBackend(files.current); err != nil {
		return responseOf(err)
	}
	action := FabricsNotApplied
	if len(left) == 0 {
		if action, err = h.fabricsAction(ctx, cur.exists, curBroken); err != nil {
			return responseOf(err)
		}
	}
	if action != FabricsDeferred {
		rec.Units = []string{action + " " + FabricsUnit}
	}
	if req.Op == OpFabricsPlan {
		return Response{Result: ResultValidated, FabricsAction: action, Message: markerNote}
	}

	boot, err := h.bootID()
	if err != nil {
		return responseOf(err)
	}
	it := &Intent{V: Version, BootID: boot, Started: h.cfg.Now().UTC(), Actor: req.Actor, Peer: rec.Peer, Op: rec.Op, Step: "planned"}
	// Always root:root 0600 (the helper's ids): the file may hold CHAP
	// secrets, and mhvtl-fabrics refuses one with a secret that group or
	// others can read (fabrics.conf(5) SECURITY).
	if err := h.writeFiles(ctx, it, []change{{name: fabricsConf, base: cur.sha, data: next, private: true}}, files); err != nil {
		return responseOf(err)
	}
	if action == FabricsDeferred || action == FabricsNotApplied {
		if err := h.removeIntent(); err != nil {
			return responseOf(err)
		}
		msg := targetUnit + " is not running; " + FabricsUnit + " applies " + fabricsConf + " when it starts"
		if action == FabricsNotApplied {
			msg = notAppliedText(left)
		}
		return Response{Result: ResultApplied, FabricsAction: action, Message: msg + h.pruneFabricsCopies(it.RollbackDir)}
	}
	it.Step = "applying"
	if err := h.saveIntent(it); err != nil {
		return responseOf(err)
	}
	if h.killPoint(it.Step) {
		return responseOf(errKilled)
	}
	if err := h.runFabrics(ctx, action); err != nil {
		resp := h.rollBackFabrics(ctx, it, action, err, h.fabricsMask(cur.data, next))
		if it.RollbackDir != "" {
			resp.RollbackDir = filepath.Join(h.cfg.StateDir, rollbackDir, it.RollbackDir)
		}
		return resp
	}
	if err := h.removeIntent(); err != nil {
		return responseOf(err)
	}
	return Response{Result: ResultApplied, FabricsAction: action, Message: strings.TrimPrefix(h.pruneFabricsCopies(it.RollbackDir), "; ")}
}

// pruneFabricsCopies removes the fabrics.conf copies of every rollback
// directory but keep, this apply's, after a successful apply: they hold
// CHAP secrets the file may no longer have, and recovery never reads them
// (it restores from intent.json). keep's copy, the file just replaced,
// stays for a manual undo until the next fabrics apply succeeds. It
// returns "" or a note on what it could not remove.
func (h *Helper) pruneFabricsCopies(keep string) string {
	dir := filepath.Join(h.cfg.StateDir, rollbackDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "; listing " + dir + ": " + err.Error()
	}
	var errs []string
	for _, e := range ents {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name(), fabricsConf)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return "; removing older " + fabricsConf + " rollback copies: " + strings.Join(errs, "; ")
	}
	return ""
}

// checkBackend refuses unless mhvtl-fabrics.service would run with
// MHVTL_BACKEND=tcmu: the unit sets mhvtl and reads mhvtl.conf after it
// (Environment=, EnvironmentFile=,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/etc/mhvtl-fabrics.service.in#L16-L17),
// and apply does nothing for any other value while still exiting 0
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L255-L260).
// The value is the one systemd passes (conffile.EnvironmentFile), not the
// shell's: systemd keeps text after an unquoted value, so
// "MHVTL_BACKEND=tcmu # TCMU" passes "tcmu # TCMU".
func (h *Helper) checkBackend(read func(string) (fileState, error)) error {
	backend := "mhvtl"
	mc, err := read(mhvtlConf)
	if err != nil {
		return err
	}
	if mc.exists {
		if line, what := envSubsetViolation(mc.data); what != "" {
			return refuse(ErrFabricsBackend, "%s line %d has %s; the helper checks only what every supported systemd reads the same way, so it cannot tell what MHVTL_BACKEND %s gets: keep %s to plain ASCII lines without backslashes, under 4096 bytes",
				mhvtlConf, line, what, FabricsUnit, mhvtlConf)
		}
		vars, err := conffile.EnvironmentFile(mc.data)
		if err != nil {
			return refuse(ErrFabricsBackend, "%s: %v, so %s does not start", mhvtlConf, err, FabricsUnit)
		}
		if v, ok := vars["MHVTL_BACKEND"]; ok {
			backend = v
		}
	}
	if backend != "tcmu" {
		return refuse(ErrFabricsBackend, "systemd passes MHVTL_BACKEND=%q from %s to %s, not tcmu: mhvtl-fabrics exports only TCMU backstores (systemd keeps text after an unquoted value; write MHVTL_BACKEND=tcmu alone on its line)",
			backend, mhvtlConf, FabricsUnit)
	}
	return nil
}

// envSubsetViolation returns the first line of an EnvironmentFile= file
// outside the subset every supported systemd (v249 to v258) reads the same
// way, and what is there: a backslash (escapes and continuations; v252 and
// v258 differ on a continued comment), a byte other than printable ASCII
// and tab (UTF-8, which systemd validates more strictly than Go: a file it
// rejects is ignored and the unit keeps MHVTL_BACKEND=mhvtl; other control
// bytes are refused to keep the subset small), or a line longer than
// envMaxLine (systemd drops an assignment over its ARG_MAX-derived limit).
// Inside the subset conffile.EnvironmentFile matches systemd. what is ""
// when the whole file is inside.
func envSubsetViolation(data []byte) (int, string) {
	line, n := 1, 0
	for _, b := range data {
		switch {
		case b == '\n':
			line, n = line+1, 0
			continue
		case b == '\\':
			return line, "a backslash"
		case b != '\t' && (b < 0x20 || b > 0x7e):
			return line, fmt.Sprintf("byte 0x%02x", b)
		}
		if n++; n > envMaxLine {
			return line, fmt.Sprintf("a line over %d bytes", envMaxLine)
		}
	}
	return 0, ""
}

// envMaxLine is far below any systemd assignment limit and far above any
// line mhvtl.conf needs.
const envMaxLine = 4096

// checkPlan runs conffile.CheckPlan against device.conf, which
// mhvtl-fabrics needs as well (load,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L99-L118):
// a file with a plan problem the current file, cur, does not have is
// refused before anything is written. Problems are compared by message,
// not line, as an edit moves the records after it. The problems that
// were there before are returned in left (the file is then written but
// not applied, FabricsNotApplied); curBroken reports whether cur has plan
// problems at all.
func checkPlan(conf *conffile.FabricsConf, cur fileState, files *fileSet) (left []conffile.Problem, curBroken bool, err error) {
	dc, err := files.current(deviceConf)
	switch {
	case err != nil:
		return nil, false, refuse(ErrInvalid, "%s cannot be read (%v): mhvtl-fabrics needs it", deviceConf, err)
	case !dc.exists:
		return nil, false, refuse(ErrInvalid, "%s does not exist: mhvtl-fabrics needs it", deviceConf)
	}
	dev := conffile.ParseDeviceConf(dc.data)
	had := map[string]bool{}
	if cc := conffile.ParseFabricsConf(cur.data); cur.exists && len(cc.Problems) == 0 {
		for _, p := range cc.CheckPlan(dev) {
			had[p.Msg] = true
		}
	}
	var msgs []string
	for _, p := range conf.CheckPlan(dev) {
		if had[p.Msg] {
			left = append(left, p)
			continue
		}
		msgs = append(msgs, p.Error())
	}
	if len(msgs) > 0 {
		return nil, false, refuse(ErrInvalid, "%s: %s", fabricsConf, strings.Join(msgs, "; "))
	}
	return left, len(had) > 0, nil
}

// notAppliedText is the answer of a FabricsNotApplied apply.
func notAppliedText(left []conffile.Problem) string {
	msgs := make([]string, len(left))
	for k, p := range left {
		msgs[k] = p.Error()
	}
	return fmt.Sprintf("%s written, %s not reloaded: the file still has targets mhvtl-fabrics would not export, which it had before (%s), so its apply would fail; stop exporting them or fix device.conf, and the change that leaves none applies the file",
		fabricsConf, FabricsUnit, strings.Join(msgs, "; "))
}

// checkFabricsDevices refuses a device.conf change that leaves an iSCSI
// target of fabrics.conf without a plan: mhvtl-fabrics plans against
// device.conf on every apply (load,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L99-L118),
// so deleting an exported library, or giving two of its drives the same
// Slot, would make the next reload or start of mhvtl-fabrics.service fail.
// Only problems the change brings are refused. It also refuses a change
// that makes the file name a target it did not name before (a derived
// name follows the device kind: ".lib<ID>" or ".tape<ID>", and a target
// that did not plan may plan now) which exists in configfs and
// mhvtl-fabrics does not own (checkForeignTargets). Without fabrics.conf,
// with one mhvtl-fabrics cannot parse, or when fabrics are not in use
// (fabricsInUse), there is nothing to check.
func (h *Helper) checkFabricsDevices(ctx context.Context, files, next *fileSet) error {
	cur, err := files.current(deviceConf)
	if err != nil {
		return err
	}
	nd, err := next.next(deviceConf)
	if err != nil {
		return err
	}
	if nd.sha == cur.sha {
		return nil
	}
	fc, err := files.current(fabricsConf)
	if err != nil || !fc.exists {
		return err
	}
	conf := conffile.ParseFabricsConf(fc.data)
	if len(conf.Problems) > 0 {
		return nil
	}
	if inUse, err := h.fabricsInUse(ctx, files, next); err != nil || !inUse {
		return err
	}
	curDev, nextDev := conffile.ParseDeviceConf(cur.data), conffile.ParseDeviceConf(nd.data)
	had := map[string]bool{}
	for _, p := range conf.CheckPlan(curDev) {
		had[p.Error()] = true
	}
	ids := map[int]int{}
	for _, t := range conf.Targets() {
		ids[t.Line] = t.ID
	}
	var targets, msgs []string
	for _, p := range conf.CheckPlan(nextDev) {
		if had[p.Error()] {
			continue
		}
		msgs = append(msgs, p.Error())
		if id, ok := ids[p.Line]; ok {
			targets = append(targets, fmt.Sprint(id))
		}
	}
	if len(msgs) > 0 {
		what := "fabrics.conf"
		if len(targets) > 0 {
			what = "the iSCSI target(s) " + strings.Join(targets, ", ") + " of " + fabricsConf
		}
		return refuse(ErrInvalid, "%s: the change would leave %s without a plan, and the next reload or start of %s would fail (%s: %s); stop exporting them first (iSCSI unbind), or keep the devices they export",
			deviceConf, what, FabricsUnit, fabricsConf, strings.Join(msgs, "; "))
	}
	named := map[string]bool{}
	for _, w := range conf.PlannedWWNs(curDev) {
		named[w] = true
	}
	var appear []string
	for _, w := range conf.PlannedWWNs(nextDev) {
		if !named[w] {
			appear = append(appear, w)
		}
	}
	foreign, err := h.foreignTargets(appear)
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return refuse(ErrInvalid, "%s: with the change %s names iSCSI target %s, which exists in configfs and mhvtl-fabrics does not own (not in %s): the next reload or start of %s would take it over, remove the ACLs and LUNs it does not list, and delete it on unbind; remove that target first, or keep the devices as they are",
			deviceConf, fabricsConf, strings.Join(foreign, ", "), h.cfg.FabricsState, FabricsUnit)
	}
	return nil
}

// fabricsInUse reports whether mhvtl-fabrics would apply fabrics.conf at
// all: MHVTL_BACKEND is tcmu (checkBackend) in mhvtl.conf as it is or as
// the request leaves it, and mhvtl-fabrics.service is installed. Otherwise
// nothing reloads it, and a stale file must not block device.conf edits:
// the iSCSI unbind that would clear it is refused with fabrics_backend or
// fabrics_unit.
func (h *Helper) fabricsInUse(ctx context.Context, files, next *fileSet) (bool, error) {
	if h.checkBackend(files.current) != nil && h.checkBackend(next.next) != nil {
		return false, nil
	}
	load, err := showProp(ctx, &h.cfg, FabricsUnit, "LoadState")
	if err != nil {
		return false, err
	}
	return load == "loaded", nil
}

// checkForeignTargets refuses a file that plans a target which exists in
// configfs but is not in mhvtl-fabrics' state file: a target someone else
// made, with targetcli or another tool. mhvtl-fabrics apply would adopt it:
// cfs_mkdir accepts EEXIST
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L138-L154),
// state_write records every configured name as owned
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L461-L483),
// apply_target removes the ACLs the record does not list and the LUNs it
// does not map
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L576-L688),
// and a later unbind or remove deletes the target
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L690-L710).
// A target mhvtl-fabrics already owns is its own to change.
func (h *Helper) checkForeignTargets(conf *conffile.FabricsConf, files *fileSet) error {
	dc, err := files.current(deviceConf)
	if err != nil {
		return err
	}
	foreign, err := h.foreignTargets(conf.PlannedWWNs(conffile.ParseDeviceConf(dc.data)))
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return refuse(ErrInvalid, "iSCSI target %s already exists in configfs and mhvtl-fabrics does not own it (not in %s): applying %s would take it over, remove the ACLs and LUNs it does not list, and delete it on unbind; choose another Target name or Base name, or remove that target first",
			strings.Join(foreign, ", "), h.cfg.FabricsState, fabricsConf)
	}
	return nil
}

// foreignTargets returns the names of wwns that exist in configfs as
// targets mhvtl-fabrics' state file does not list. The state file is read
// only when one of them exists.
func (h *Helper) foreignTargets(wwns []string) ([]string, error) {
	var foreign []string
	var owned map[string]bool
	for _, wwn := range wwns {
		st, err := os.Lstat(filepath.Join(h.cfg.TargetRoot, "iscsi", wwn))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		case !st.IsDir():
			continue
		}
		if owned == nil {
			list, err := fabricsOwned(h.cfg.FabricsState)
			if err != nil {
				return nil, fmt.Errorf("%s exists in configfs and the state file cannot be read to tell whether mhvtl-fabrics owns it: %w", wwn, err)
			}
			owned = map[string]bool{}
			for _, o := range list {
				owned[o] = true
			}
		}
		if !owned[wwn] {
			foreign = append(foreign, wwn)
		}
	}
	return foreign, nil
}

// fabricsForeign is a reload the helper did not run: fabrics.conf as it is
// on disk (a restored file, or the one a crash recovery or the marker's
// retry finds) plans targets that exist in configfs and mhvtl-fabrics does
// not own, which the reload would adopt (checkForeignTargets).
type fabricsForeign struct {
	wwns  []string
	state string
}

func (e *fabricsForeign) Error() string {
	return fmt.Sprintf("%s not reloaded: %s names iSCSI target %s, which exists in configfs and mhvtl-fabrics does not own (not in %s); a reload would take it over, remove the ACLs and LUNs it does not list, and delete it on unbind",
		FabricsUnit, fabricsConf, strings.Join(e.wwns, ", "), e.state)
}

// foreignOnDisk runs the foreign-target check on fabrics.conf and
// device.conf as they are on disk, right before the helper reloads the
// unit with them. A file that does not exist, or that mhvtl-fabrics cannot
// parse, creates no target: apply fails before it touches configfs (load,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L80-L118).
func (h *Helper) foreignOnDisk() error {
	fc, err := readTarget(h.cfg.ConfigDir, fabricsConf)
	if err != nil || !fc.exists {
		return err
	}
	conf := conffile.ParseFabricsConf(fc.data)
	if len(conf.Problems) > 0 {
		return nil
	}
	dc, err := readTarget(h.cfg.ConfigDir, deviceConf)
	if err != nil {
		return err
	}
	foreign, err := h.foreignTargets(conf.PlannedWWNs(conffile.ParseDeviceConf(dc.data)))
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return &fabricsForeign{wwns: foreign, state: h.cfg.FabricsState}
	}
	return nil
}

// fabricsAction decides how the edited file gets applied, from the state
// of mhvtl-fabrics.service and mhvtl.target. curBroken is set when the
// current file has plan problems, which the edit removes: a failed unit
// most likely failed on them, and is started with the file that has none.
func (h *Helper) fabricsAction(ctx context.Context, existed, curBroken bool) (string, error) {
	out, err := h.cfg.Systemctl(ctx, "show", "--property=LoadState,ActiveState,CanReload", FabricsUnit, targetUnit)
	if err != nil {
		return "", fmt.Errorf("systemctl show: %w", err)
	}
	units, err := showBlocks(out, 2)
	if err != nil {
		return "", err
	}
	fab, target := units[0], units[1]
	if fab["LoadState"] != "loaded" {
		return "", refuse(ErrFabricsUnit, "%s is %s: install mhVTL's TCMU package (mhvtl-tcmu)", FabricsUnit, orUnknown(fab["LoadState"]))
	}
	if fab["CanReload"] != "yes" {
		return "", refuse(ErrFabricsUnit, "%s has no ExecReload: install mhvtl-control's drop-in mhvtl-fabrics.service.d/50-mhvtl-control-reload.conf and run systemctl daemon-reload", FabricsUnit)
	}
	switch st := fab["ActiveState"]; st {
	case "active":
		return FabricsReload, nil
	case "inactive", "failed":
		// A failed first start leaves the unit failed and its file
		// removed by the rollback: systemctl start then replaces the
		// failed state, as start does for an inactive unit, now that the
		// file exists again for ConditionPathExists.
		switch {
		case st == "failed" && existed && !curBroken:
			// The failed run's ExecStopPost=mhvtl-fabrics remove took its
			// targets down, so no session is up.
			return "", refuse(ErrFabricsState, "%s failed; check journalctl -u %s, fix the cause, then systemctl restart %s, which starts it again and removes any target the failed run left behind", FabricsUnit, FabricsUnit, FabricsUnit)
		case target["ActiveState"] != "active":
			return FabricsDeferred, nil
		case !existed || st == "failed":
			return FabricsStart, nil
		default:
			return "", refuse(ErrFabricsState, "%s is stopped while %s runs and %s exists; check journalctl -u %s, then systemctl start %s", FabricsUnit, targetUnit, fabricsConf, FabricsUnit, FabricsUnit)
		}
	default:
		return "", refuse(ErrFabricsState, "%s is %s; try again when it settles", FabricsUnit, orUnknown(st))
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// showBlocks parses systemctl show output for n units into one map per
// unit, in order.
func showBlocks(out []byte, n int) ([]map[string]string, error) {
	var units []map[string]string
	for _, b := range bytes.Split(out, []byte("\n\n")) {
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		m := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				m[k] = v
			}
		}
		units = append(units, m)
	}
	if len(units) != n {
		return nil, fmt.Errorf("systemctl show: %d units in the answer, asked for %d", len(units), n)
	}
	return units, nil
}

// fabricsStop stops the unit; ExecStopPost=mhvtl-fabrics remove removes
// its targets.
const fabricsStop = "stop"

// runFabrics runs systemctl reload, start or stop on the unit, and on no
// other, bounded by fabricsTimeout.
func (h *Helper) runFabrics(ctx context.Context, action string) error {
	switch action {
	case FabricsReload, FabricsStart, fabricsStop:
	default:
		return fmt.Errorf("systemctl %s %s: not a fabrics action", action, FabricsUnit)
	}
	ctx, cancel := context.WithTimeout(ctx, fabricsTimeout)
	defer cancel()
	_, err := h.cfg.Systemctl(ctx, action, FabricsUnit)
	return err
}

// rollBackFabrics restores fabrics.conf after a failed reload or start
// and has the targets follow the restored file (reconcileFabrics): apply
// is incremental, so the failed run may have changed some of them. It runs
// to its end even when ctx ends (the helper got SIGTERM during the reload
// or start). The intent is kept when the restore fails, so that recovery
// retries it. A failed reconcile answers fabrics_recovery_required, as
// configfs may then not match the file; reconcileFailed says what is kept
// for the retry. m masks the failed commands' errors, the journal and
// the state file's names as output, the message as one the helper
// composed.
func (h *Helper) rollBackFabrics(ctx context.Context, it *Intent, action string, runErr error, m fabricsMask) Response {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricsTimeout)
	defer cancel()
	resp := Response{Result: ResultRolledBack, Error: ErrFabricsApply, FailedUnit: FabricsUnit, FabricsAction: action}
	msg := fmt.Sprintf("systemctl %s %s failed: %s", action, FabricsUnit, m.output(errText(runErr)))
	if h.cfg.JournalTail != nil {
		if tail, err := h.cfg.JournalTail(ctx, FabricsUnit); err == nil {
			resp.JournalTail = m.output(string(tail))
		}
	}
	conflicts, err := h.restore(it)
	resp.Conflicts = conflicts
	if err != nil {
		resp.Result, resp.Error = ResultError, ErrInternal
		resp.Message = msg + "; restoring " + fabricsConf + ": " + err.Error()
		return resp
	}
	if len(conflicts) > 0 {
		msg += "; " + restoreConflicts(conflicts).Error()
	} else {
		msg += "; " + fabricsConf + " restored"
	}
	removed := h.newFabricsRemoved(it)
	note, err := h.reconcileFabrics(ctx, removed, m)
	if err != nil {
		resp.Result, resp.Error = ResultError, ErrFabricsState
		resp.Message = m.message(msg + "; " + h.reconcileFailed(err, removed))
		return resp
	}
	msg += "; " + note
	if err := h.removeIntent(); err != nil {
		msg += "; " + err.Error()
	}
	resp.Message = m.message(msg)
	return resp
}

// newFabricsRemoved reports whether the restore removed a fabrics.conf the
// apply had created: the intent records it as new, and it is gone.
func (h *Helper) newFabricsRemoved(it *Intent) bool {
	for _, f := range it.Files {
		if f.Name == fabricsConf && !f.Existed {
			cur, err := readTarget(h.cfg.ConfigDir, fabricsConf)
			return err == nil && !cur.exists
		}
	}
	return false
}

// reconcileFabrics has the targets follow fabrics.conf as it is now, and
// says what it did. An active unit is reloaded, or stopped when removed is
// set (the restore removed a file the apply had created): mhvtl-fabrics
// apply fails without fabrics.conf (load,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L80-L83),
// while ExecStopPost=mhvtl-fabrics remove removes the targets
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/etc/mhvtl-fabrics.service.in#L20),
// and the unit was not running before that start. An inactive unit has no
// targets and is left alone, and so is a failed one unless mhvtl-fabrics'
// state file still lists targets: its ExecStopPost=remove can fail too
// ("remove left targets behind",
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L270-L276),
// and it then keeps the state file, which a clean remove deletes
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L530-L551);
// that is a *fabricsLeftover. Any other state (a job still running) and a
// failed systemctl are errors: the targets may not follow the file. A
// reload is not run when the file would adopt a target mhvtl-fabrics does
// not own (a *fabricsForeign): a stop removes only owned targets. It runs
// to its end even when ctx ends. What the error quotes from outside the
// helper (systemctl's error, the state file's names and read error) is
// masked with m.output.
func (h *Helper) reconcileFabrics(ctx context.Context, removed bool, m fabricsMask) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricsTimeout)
	defer cancel()
	st, err := showProp(ctx, &h.cfg, FabricsUnit, "ActiveState")
	if err != nil {
		return "", err
	}
	switch st {
	case "failed":
		if owned, err := fabricsOwned(h.cfg.FabricsState); err != nil || len(owned) > 0 {
			lo := &fabricsLeftover{path: h.cfg.FabricsState}
			for _, o := range owned {
				lo.owned = append(lo.owned, m.name(o))
			}
			if err != nil {
				lo.err = errors.New(m.output(err.Error()))
			}
			return "", lo
		}
		return FabricsUnit + " is " + st + ", not reloaded", nil
	case "inactive":
		return FabricsUnit + " is " + st + ", not reloaded", nil
	case "active":
	default:
		return "", &fabricsBusy{state: orUnknown(st)}
	}
	verb, done := FabricsReload, "reloaded "+FabricsUnit+" with it"
	if removed {
		verb, done = fabricsStop, "stopped "+FabricsUnit+", which removed its targets: it ran only for the start that failed"
	} else if err := h.foreignOnDisk(); err != nil {
		return "", err
	}
	if err := h.runFabrics(ctx, verb); err != nil {
		return "", fmt.Errorf("systemctl %s %s with it failed too: %s", verb, FabricsUnit, m.output(errText(err)))
	}
	return done, nil
}

// fabricsBusy is a unit in a state other than active, inactive or failed:
// a job (the timed-out reload, a start or stop) still runs, and the
// targets follow whatever file that job read.
type fabricsBusy struct{ state string }

func (e *fabricsBusy) Error() string { return FabricsUnit + " is " + e.state }

// reconcileFailed answers a reconcile that failed after the file restore
// succeeded, and decides what the helper keeps for the retry:
//
//   - a *fabricsLeftover: nothing (dropForLeftovers);
//   - a *fabricsBusy unit: the intent, so that the next request's recovery
//     reconciles once the job ends; recovery then only reads the unit's
//     state while it stays busy;
//   - any other error (the reload or stop of the restored file failed):
//     the fabrics marker, and the intent is removed. A kept intent would
//     make every request, config applies and service actions included,
//     run the reload again first, each up to fabricsTimeout, and fail. The
//     marker is read only by fabrics ops (retryReconcile).
func (h *Helper) reconcileFailed(err error, removed bool) string {
	if msg, ok := h.dropForLeftovers(err); ok {
		return msg
	}
	var busy *fabricsBusy
	if errors.As(err, &busy) {
		return reconcileAdvice(err, removed)
	}
	msg := markerAdvice(err, removed)
	if merr := h.markFabrics(removed); merr != nil {
		return msg + "; recording that failed (" + merr.Error() + "), so the intent is kept and the next helper request retries it"
	}
	if rerr := h.removeIntent(); rerr != nil {
		msg += "; " + rerr.Error()
	}
	return msg
}

// fabricsMarkerFile, in StateDir, records that a rollback restored
// fabrics.conf but could not make the targets follow it: configfs may not
// match the file. Only fabrics ops read it. It holds for the boot it was
// written in: configfs starts empty at boot, and mhvtl-fabrics.service
// applies the file when mhVTL starts.
const fabricsMarkerFile = "fabrics-reconcile.json"

type fabricsMarker struct {
	V     int       `json:"v"`
	Since time.Time `json:"since"`
	// BootID is the boot the marker was written in
	// (/proc/sys/kernel/random/boot_id).
	BootID string `json:"boot_id"`
	// Removed: the restore removed a fabrics.conf the apply had created,
	// and the reconcile is a stop.
	Removed bool `json:"removed"`
}

func (h *Helper) markFabrics(removed bool) error {
	boot, err := h.bootID()
	if err != nil {
		return err
	}
	b, err := json.Marshal(fabricsMarker{V: Version, Since: h.cfg.Now().UTC(), BootID: boot, Removed: removed})
	if err != nil {
		return err
	}
	return conffile.WriteAtomic(h.cfg.StateDir, fabricsMarkerFile, b, 0o600, -1, -1)
}

// liveMarker returns the marker of this boot, nil when there is none. A
// marker of an earlier boot is ignored, and removed when remove is set.
func (h *Helper) liveMarker(remove bool) (*fabricsMarker, error) {
	m, err := h.readFabricsMarker()
	if err != nil || m == nil {
		return nil, err
	}
	boot, err := h.bootID()
	if err != nil {
		return nil, err
	}
	if m.BootID == boot {
		return m, nil
	}
	if remove {
		return nil, h.clearFabricsMarker()
	}
	return nil, nil
}

// readFabricsMarker returns the marker, nil when there is none. It does
// not follow a symbolic link.
func (h *Helper) readFabricsMarker() (*fabricsMarker, error) {
	path := filepath.Join(h.cfg.StateDir, fabricsMarkerFile)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	var m fabricsMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func (h *Helper) clearFabricsMarker() error {
	if err := os.Remove(filepath.Join(h.cfg.StateDir, fabricsMarkerFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(h.cfg.StateDir)
}

// retryReconcile runs before a fabrics apply (a plan only reads the
// marker, peekMarker) while the fabrics marker exists: it has the targets follow fabrics.conf as it is now
// (reconcileFabrics) and removes the marker when that succeeds, an
// inactive unit included. A marker of an earlier boot is removed without a
// retry. Otherwise the op is refused with
// fabrics_recovery_required and the marker stays; a *fabricsLeftover
// removes it, as the state file then holds what to remove. Each retry is
// audited, as it may reload or stop the unit.
func (h *Helper) retryReconcile(ctx context.Context, rec *AuditRecord) error {
	m, err := h.liveMarker(true)
	if err != nil || m == nil {
		return err
	}
	cur, err := readTarget(h.cfg.ConfigDir, fabricsConf)
	if err != nil {
		return err
	}
	removed := markerStops(m, cur)
	mask := h.fabricsMask(cur.data)
	audit := AuditRecord{TS: h.cfg.Now(), Actor: rec.Actor, Peer: rec.Peer, Op: OpRecover, Result: ResultRecovered}
	note, err := h.reconcileFabrics(ctx, removed, mask)
	var lo *fabricsLeftover
	switch {
	case err == nil:
		if err := h.clearFabricsMarker(); err != nil {
			return err
		}
		audit.Message = mask.message("an earlier rollback left configfs possibly out of step with " + fabricsConf + ": " + note)
		h.audit(audit)
		return nil
	case errors.As(err, &lo):
		err = errors.Join(refuse(ErrFabricsState, "%s", mask.message(lo.Error())), h.clearFabricsMarker())
	default:
		err = refuse(ErrFabricsState, "%s", mask.message("an earlier rollback restored "+fabricsConf+" but could not make the targets follow it, and retrying failed: "+markerAdvice(err, removed)))
	}
	audit.Result, audit.Error, audit.Message = ResultError, ErrFabricsState, err.Error()
	h.audit(audit)
	return err
}

// peekMarker is retryReconcile for a plan, which reads and changes
// nothing but answers: "" without the marker; with it, a note that the
// apply retries first. When the retry would be refused for a foreign
// target (foreignOnDisk), the plan is refused the same way, with
// fabrics_recovery_required.
func (h *Helper) peekMarker() (string, error) {
	m, err := h.liveMarker(false)
	if err != nil || m == nil {
		return "", err
	}
	cur, err := readTarget(h.cfg.ConfigDir, fabricsConf)
	if err != nil {
		return "", err
	}
	removed := markerStops(m, cur)
	verb := FabricsReload
	if removed {
		verb = fabricsStop
	} else if err := h.foreignOnDisk(); err != nil {
		return "", refuse(ErrFabricsState, "%s", h.fabricsMask(cur.data).message("an earlier rollback restored "+fabricsConf+" but could not make the targets follow it, and applying a change would retry that first: "+markerAdvice(err, removed)))
	}
	return fmt.Sprintf("an earlier rollback restored %s but could not make the targets follow it: applying this change first runs systemctl %s %s again, and is refused if that fails", fabricsConf, verb, FabricsUnit), nil
}

// markerStops reports whether the marker's retry is a stop: the rollback
// removed a fabrics.conf the apply had created (m.Removed) and there is
// still none. When the file exists again, someone wrote it since (and
// likely started the unit with it): the retry reloads, which makes the
// targets follow that file, rather than stop the unit and remove them.
func markerStops(m *fabricsMarker, cur fileState) bool { return m.Removed && !cur.exists }

// fabricsLeftover is a failed mhvtl-fabrics.service whose state file
// lists targets, or cannot be read: they may still be exported.
type fabricsLeftover struct {
	path  string
	owned []string
	err   error
}

func (e *fabricsLeftover) Error() string {
	what := "lists targets the failed run exported (" + strings.Join(e.owned, ", ") + ")"
	if e.err != nil {
		what = "cannot be read (" + e.err.Error() + "), so targets the failed run exported may remain"
	}
	return fmt.Sprintf("%s failed and its state file %s %s: they stay exported until systemctl restart %s, whose apply removes the owned targets %s no longer names, or mhvtl-fabrics remove",
		FabricsUnit, e.path, what, FabricsUnit, fabricsConf)
}

// dropForLeftovers answers a reconcile that found a *fabricsLeftover: the
// intent is removed rather than kept for the next request to retry, as the
// state file, not the intent, holds what to remove, and a kept intent
// would block every request. ok is false for any other error.
func (h *Helper) dropForLeftovers(err error) (msg string, ok bool) {
	var lo *fabricsLeftover
	if !errors.As(err, &lo) {
		return "", false
	}
	msg = lo.Error() + "; the helper removed its intent rather than keep it for the next request: the state file holds what to remove"
	if err := h.removeIntent(); err != nil {
		msg += "; " + err.Error()
	}
	return msg, true
}

// fabricsMaxOwned is the size of struct owned's list: FAB_MAX_TARGETS * 2
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L410-L413,
// include/fabrics.h L28).
const fabricsMaxOwned = 32 * 2

// fabricsOwned reads mhvtl-fabrics' state file as state_read does
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L435-L448):
// fscanf "%31s %223s" pairs, at most fabricsMaxOwned, each kept when its
// fabric is "iscsi" and its name a valid iSCSI name. Like open_nofollow it
// does not follow a symlink. A file that does not exist owns nothing; any
// other error opening or reading it is returned.
func fabricsOwned(path string) ([]string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	p := 0
	// word is one %Ns conversion: white space skipped, then up to width
	// bytes that are not white space. C compares it only up to a NUL.
	word := func(width int) (string, bool) {
		for p < len(data) && isCSpaceByte(data[p]) {
			p++
		}
		if p == len(data) {
			return "", false
		}
		start := p
		for p < len(data) && p-start < width && !isCSpaceByte(data[p]) {
			p++
		}
		w, _, _ := strings.Cut(string(data[start:p]), "\x00")
		return w, true
	}
	var owned []string
	for len(owned) < fabricsMaxOwned {
		fabric, ok := word(31)
		if !ok {
			break
		}
		wwn, ok := word(223)
		if !ok {
			break
		}
		if fabric == "iscsi" && conffile.ValidISCSIName(wwn) {
			owned = append(owned, wwn)
		}
	}
	return owned, nil
}

// isCSpaceByte is C's isspace in the "C" locale.
func isCSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// reconcileAdvice is the message of a reconcile that failed: what to run
// by hand, and that the next helper request retries it.
func reconcileAdvice(err error, removed bool) string {
	verb := FabricsReload
	if removed {
		verb = fabricsStop
	}
	return fmt.Sprintf("%v; configfs may not match %s: check journalctl -u %s, then systemctl %s %s; the next helper request retries it",
		err, fabricsConf, FabricsUnit, verb, FabricsUnit)
}

// markerAdvice is the message of a reconcile that failed with the fabrics
// marker recorded: what to check, what clears the marker, and that only
// fabrics requests wait for it.
func markerAdvice(err error, removed bool) string {
	var foreign *fabricsForeign
	if errors.As(err, &foreign) {
		return fmt.Sprintf("%v; remove that target from configfs, or change %s by hand so that it no longer names it: the next iSCSI change retries the reload first and is refused while the target is there; this is cleared when that retry succeeds or finds the unit stopped, or by a reboot; other requests go ahead",
			err, fabricsConf)
	}
	verb := FabricsReload
	if removed {
		verb = fabricsStop
	}
	return fmt.Sprintf("%v; configfs may not match %s: check journalctl -u %s and fix the cause; the next iSCSI change retries systemctl %s %s first and is refused while it fails; this is cleared when that retry succeeds or finds the unit stopped, or by a reboot; other requests go ahead",
		err, fabricsConf, FabricsUnit, verb, FabricsUnit)
}

// errText is err with the stderr of a failed command, which Output keeps
// in the *exec.ExitError.
func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return err.Error() + ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// fabricsDiff is conffile.FabricsDiff with every secret of either file
// masked in every line, as a hand-written comment may quote one, capped
// like auditFiles' diffs.
func fabricsDiff(before, after []byte) (string, bool) {
	d := conffile.MaskSecrets(conffile.FabricsDiff(before, after), fabricsSecrets(before, after))
	if len(d) > maxDiff {
		return d[:maxDiff], true
	}
	return d, false
}

// fabricsMask masks the CHAP secrets of the files of a fabrics answer:
// stored, the file a client could read before (the file the request
// edits, or the one a rollback or recovery restored), and newer, the files
// a request brought (the edit, or the half-applied file a recovery finds).
// Besides the files' clear values (names a client sets and reads in
// clear, conffile.FabricsClearValues) it knows the target names each file
// plans against device.conf as it is on disk
// (conffile.FabricsConf.PlannedWWNs): a derived name, the Base name and
// ".lib<ID>" or ".tape<ID>", is one the client can tell from the file
// too, and a secret that spans the Base name and that suffix must be
// masked, or not, whether or not it is the guess.
type fabricsMask struct {
	secrets []string
	// keepOut are the names stored has, clear values and planned target
	// names; hideOut the names newer brings, which stored has not.
	keepOut, hideOut []string
	// keepMsg are the names of every file.
	keepMsg []string
	wwns    map[string]bool
}

func (h *Helper) fabricsMask(stored []byte, newer ...[]byte) fabricsMask {
	files := append([][]byte{stored}, newer...)
	m := fabricsMask{secrets: fabricsSecrets(files...), wwns: map[string]bool{}}
	var dev *conffile.DeviceConf
	if dc, err := readTarget(h.cfg.ConfigDir, deviceConf); err == nil {
		dev = conffile.ParseDeviceConf(dc.data)
	}
	planned := func(f []byte) []string {
		if c := conffile.ParseFabricsConf(f); dev != nil && len(c.Problems) == 0 {
			return c.PlannedWWNs(dev)
		}
		return nil
	}
	storedWWNs := planned(stored)
	had := map[string]bool{}
	for _, w := range storedWWNs {
		had[w], m.wwns[w] = true, true
	}
	m.keepOut = append(conffile.FabricsClearValues(stored), storedWWNs...)
	m.keepMsg = append([]string(nil), m.keepOut...)
	for _, f := range newer {
		m.hideOut = append(m.hideOut, conffile.FabricsNewValues(stored, f)...)
		m.keepMsg = append(m.keepMsg, conffile.FabricsClearValues(f)...)
		for _, w := range planned(f) {
			m.wwns[w] = true
			m.keepMsg = append(m.keepMsg, w)
			if !had[w] {
				m.hideOut = append(m.hideOut, w)
			}
		}
	}
	return m
}

// output masks text from outside the helper (systemctl's error, the
// journal, mhvtl-fabrics' state file), which may quote a stored secret
// anywhere (conffile.MaskSecretsExcept). A secret inside a name stored
// has, which the client reads in clear, is left as it is; the names newer
// brings are masked whole: either way the answer is the same whether or
// not a name the client chose contains or equals a stored secret.
func (m fabricsMask) output(s string) string {
	return conffile.MaskSecretsExcept(s, m.secrets, m.keepOut, m.hideOut)
}

// message masks a message the helper composed, whose parts from outside
// went through output: it names targets and quotes the reconcile's
// systemctl errors, but no journal. Secrets inside the names of every
// file are left as they are, so that a new name the message gives, such
// as a derived target name, shows as the client sent it whether or not it
// contains a stored secret.
func (m fabricsMask) message(s string) string {
	return conffile.MaskSecretsExcept(s, m.secrets, m.keepMsg, nil)
}

// name masks a target name mhvtl-fabrics' state file lists: one that a
// file plans is a name the client set or derives, shown as it is whatever
// it contains; any other is output.
func (m fabricsMask) name(s string) string {
	if m.wwns[s] {
		return s
	}
	return m.output(s)
}

// fabricsSecrets lists the CHAP secret values of both files, for the audit
// diff.
func fabricsSecrets(files ...[]byte) []string {
	var out []string
	for _, f := range files {
		out = append(out, conffile.FabricsSecrets(f)...)
	}
	return out
}
