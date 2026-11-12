package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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
	if bytes.Equal(cur.data, next) && cur.exists {
		return Response{Result: ResultRefused, Error: ErrBadRequest, Message: fabricsConf + " is unchanged"}
	}
	if err := conffile.ParseFabricsConf(next).Writable(); err != nil {
		return responseOf(refuse(ErrInvalid, "%v", err))
	}
	if err := h.checkBackend(files); err != nil {
		return responseOf(err)
	}
	action, err := h.fabricsAction(ctx, cur.exists)
	if err != nil {
		return responseOf(err)
	}
	if action != FabricsDeferred {
		rec.Units = []string{action + " " + FabricsUnit}
	}
	if req.Op == OpFabricsPlan {
		return Response{Result: ResultValidated, FabricsAction: action}
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
	if action == FabricsDeferred {
		if err := h.removeIntent(); err != nil {
			return responseOf(err)
		}
		return Response{Result: ResultApplied, FabricsAction: action,
			Message: targetUnit + " is not running; " + FabricsUnit + " applies " + fabricsConf + " when it starts"}
	}
	it.Step = "applying"
	if err := h.saveIntent(it); err != nil {
		return responseOf(err)
	}
	if h.killPoint(it.Step) {
		return responseOf(errKilled)
	}
	secrets := fabricsSecrets(cur.data, next)
	if err := h.runFabrics(ctx, action); err != nil {
		resp := h.rollBackFabrics(ctx, it, action, err, secrets)
		if it.RollbackDir != "" {
			resp.RollbackDir = filepath.Join(h.cfg.StateDir, rollbackDir, it.RollbackDir)
		}
		return resp
	}
	if err := h.removeIntent(); err != nil {
		return responseOf(err)
	}
	return Response{Result: ResultApplied, FabricsAction: action}
}

// checkBackend refuses unless mhvtl-fabrics.service would run with
// MHVTL_BACKEND=tcmu: the unit sets mhvtl and reads mhvtl.conf after it
// (Environment=, EnvironmentFile=), and apply does nothing for any other
// backend while still exiting 0 (markh794/mhvtl usr/cmd/mhvtl-fabrics.c
// at 19b71bd,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L255-L267).
func (h *Helper) checkBackend(files *fileSet) error {
	backend := "mhvtl"
	mc, err := files.current(mhvtlConf)
	if err != nil {
		return err
	}
	if mc.exists {
		if v, ok := conffile.ParseMhvtlConf(mc.data).Get("MHVTL_BACKEND"); ok {
			backend = v
		}
	}
	if backend != "tcmu" {
		return refuse(ErrFabricsBackend, "MHVTL_BACKEND is %q in %s, not tcmu: mhvtl-fabrics exports only TCMU backstores", backend, mhvtlConf)
	}
	return nil
}

// fabricsAction decides how the edited file gets applied, from the state
// of mhvtl-fabrics.service and mhvtl.target.
func (h *Helper) fabricsAction(ctx context.Context, existed bool) (string, error) {
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
		case st == "failed" && existed:
			// The failed run's ExecStopPost=mhvtl-fabrics remove took its
			// targets down, so no session is up.
			return "", refuse(ErrFabricsState, "%s failed; check journalctl -u %s, fix the cause, then systemctl restart %s, which starts it again (the failed run removed its targets, so no session is up to drop)", FabricsUnit, FabricsUnit, FabricsUnit)
		case target["ActiveState"] != "active":
			return FabricsDeferred, nil
		case !existed:
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
// or start). The intent is kept when the restore or the reconcile fails,
// so that recovery retries them; a failed reconcile answers
// fabrics_recovery_required, as configfs may then not match the file.
func (h *Helper) rollBackFabrics(ctx context.Context, it *Intent, action string, runErr error, secrets []string) Response {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricsTimeout)
	defer cancel()
	resp := Response{Result: ResultRolledBack, Error: ErrFabricsApply, FailedUnit: FabricsUnit, FabricsAction: action}
	msg := fmt.Sprintf("systemctl %s %s failed: %s", action, FabricsUnit, conffile.MaskSecrets(errText(runErr), secrets))
	if h.cfg.JournalTail != nil {
		if tail, err := h.cfg.JournalTail(ctx, FabricsUnit); err == nil {
			resp.JournalTail = conffile.MaskSecrets(string(tail), secrets)
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
	note, err := h.reconcileFabrics(ctx, removed)
	if err != nil {
		resp.Result, resp.Error = ResultError, ErrFabricsState
		resp.Message = conffile.MaskSecrets(msg+"; "+reconcileAdvice(err, removed), secrets)
		return resp
	}
	msg += "; " + note
	if err := h.removeIntent(); err != nil {
		msg += "; " + err.Error()
	}
	resp.Message = conffile.MaskSecrets(msg, secrets)
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
// and the unit was not running before that start. An inactive or failed
// unit has no targets and is left alone. Any other state (a job still
// running) and a failed systemctl are errors: the targets may not follow
// the file. It runs to its end even when ctx ends.
func (h *Helper) reconcileFabrics(ctx context.Context, removed bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricsTimeout)
	defer cancel()
	st, err := showProp(ctx, &h.cfg, FabricsUnit, "ActiveState")
	if err != nil {
		return "", err
	}
	switch st {
	case "inactive", "failed":
		return FabricsUnit + " is " + st + ", not reloaded", nil
	case "active":
	default:
		return "", fmt.Errorf("%s is %s", FabricsUnit, orUnknown(st))
	}
	verb, done := FabricsReload, "reloaded "+FabricsUnit+" with it"
	if removed {
		verb, done = fabricsStop, "stopped "+FabricsUnit+", which removed its targets: it ran only for the start that failed"
	}
	if err := h.runFabrics(ctx, verb); err != nil {
		return "", fmt.Errorf("systemctl %s %s with it failed too: %s", verb, FabricsUnit, errText(err))
	}
	return done, nil
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

// errText is err with the stderr of a failed command, which Output keeps
// in the *exec.ExitError.
func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return err.Error() + ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// fabricsDiff is conffile.FabricsDiff capped like auditFiles' diffs.
func fabricsDiff(before, after []byte) (string, bool) {
	d := conffile.FabricsDiff(before, after)
	if len(d) > maxDiff {
		return d[:maxDiff], true
	}
	return d, false
}

// fabricsSecrets lists the CHAP secret values of both files, for masking
// output the helper did not write itself (systemctl, the journal).
func fabricsSecrets(files ...[]byte) []string {
	var out []string
	for _, f := range files {
		out = append(out, conffile.FabricsSecrets(f)...)
	}
	return out
}
