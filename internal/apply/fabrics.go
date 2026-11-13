package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// Content equal to the file is applied like any other: refusing it
	// would tell the daemon's client that a CHAP secret it sent is the
	// stored one.
	conf := conffile.ParseFabricsConf(next)
	if err := conf.Writable(); err != nil {
		return responseOf(refuse(ErrInvalid, "%v", err))
	}
	if err := checkPlan(conf, files); err != nil {
		return responseOf(err)
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
// (Environment=, EnvironmentFile=,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/etc/mhvtl-fabrics.service.in#L16-L17),
// and apply does nothing for any other value while still exiting 0
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L255-L260).
// The value is the one systemd passes (conffile.EnvironmentFile), not the
// shell's: systemd keeps text after an unquoted value, so
// "MHVTL_BACKEND=tcmu # TCMU" passes "tcmu # TCMU".
func (h *Helper) checkBackend(files *fileSet) error {
	backend := "mhvtl"
	mc, err := files.current(mhvtlConf)
	if err != nil {
		return err
	}
	if mc.exists {
		if line, what := envSubsetViolation(mc.data); what != "" {
			return refuse(ErrFabricsBackend, "%s line %d has %s; systemd versions read that differently (an escaped newline in a comment, Unicode validation), so the helper cannot tell what MHVTL_BACKEND %s gets: keep %s to plain ASCII lines without backslashes",
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
// outside the subset every supported systemd reads the same way, and what
// is there: a backslash (escapes and continuations; v252 and v258 differ
// on a continued comment), or a byte other than printable ASCII and tab
// (control characters systemd counts as blanks, and UTF-8, which systemd
// validates more strictly than Go: a file it rejects is ignored and the
// unit keeps MHVTL_BACKEND=mhvtl). Inside the subset conffile.EnvironmentFile
// matches systemd. what is "" when the whole file is inside.
func envSubsetViolation(data []byte) (int, string) {
	line := 1
	for _, b := range data {
		switch {
		case b == '\n':
			line++
		case b == '\\':
			return line, "a backslash"
		case b != '\t' && (b < 0x20 || b > 0x7e):
			return line, fmt.Sprintf("byte 0x%02x", b)
		}
	}
	return 0, ""
}

// checkPlan runs conffile.CheckPlan against device.conf, which
// mhvtl-fabrics needs as well (load,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L99-L118):
// a file that would not plan is refused before anything is written.
func checkPlan(conf *conffile.FabricsConf, files *fileSet) error {
	dc, err := files.current(deviceConf)
	switch {
	case err != nil:
		return refuse(ErrInvalid, "%s cannot be read (%v): mhvtl-fabrics needs it", deviceConf, err)
	case !dc.exists:
		return refuse(ErrInvalid, "%s does not exist: mhvtl-fabrics needs it", deviceConf)
	}
	ps := conf.CheckPlan(conffile.ParseDeviceConf(dc.data))
	if len(ps) == 0 {
		return nil
	}
	msgs := make([]string, len(ps))
	for k, p := range ps {
		msgs[k] = p.Error()
	}
	return refuse(ErrInvalid, "%s: %s", fabricsConf, strings.Join(msgs, "; "))
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
			return "", refuse(ErrFabricsState, "%s failed; check journalctl -u %s, fix the cause, then systemctl restart %s, which starts it again and removes any target the failed run left behind", FabricsUnit, FabricsUnit, FabricsUnit)
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
		advice, ok := h.dropForLeftovers(err)
		if !ok {
			advice = reconcileAdvice(err, removed)
		}
		resp.Message = conffile.MaskSecrets(msg+"; "+advice, secrets)
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
// and the unit was not running before that start. An inactive unit has no
// targets and is left alone, and so is a failed one unless mhvtl-fabrics'
// state file still lists targets: its ExecStopPost=remove can fail too
// ("remove left targets behind",
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/cmd/mhvtl-fabrics.c#L270-L276),
// and it then keeps the state file, which a clean remove deletes
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_cfs.c#L530-L551);
// that is a *fabricsLeftover. Any other state (a job still running) and a
// failed systemctl are errors: the targets may not follow the file. It
// runs to its end even when ctx ends.
func (h *Helper) reconcileFabrics(ctx context.Context, removed bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fabricsTimeout)
	defer cancel()
	st, err := showProp(ctx, &h.cfg, FabricsUnit, "ActiveState")
	if err != nil {
		return "", err
	}
	switch st {
	case "failed":
		if owned, err := fabricsOwned(h.cfg.FabricsState); err != nil || len(owned) > 0 {
			return "", &fabricsLeftover{path: h.cfg.FabricsState, owned: owned, err: err}
		}
		return FabricsUnit + " is " + st + ", not reloaded", nil
	case "inactive":
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

// fabricsSecrets lists the CHAP secret values of both files, for masking
// output the helper did not write itself (systemctl, the journal).
func fabricsSecrets(files ...[]byte) []string {
	var out []string
	for _, f := range files {
		out = append(out, conffile.FabricsSecrets(f)...)
	}
	return out
}
