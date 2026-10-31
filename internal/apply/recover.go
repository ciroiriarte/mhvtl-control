package apply

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// recover brings back the files of the apply that left it, which is dead:
// the caller holds exclusive.lock. When the intent names libraries, or is
// the exec sequence's, its units go through Lifecycle around the file
// restore, except at boot, when nothing runs yet. The recovery step is
// journaled in the intent apart from the apply's, so a recovery that dies
// is resumed by the next one; each file restore is idempotent by the
// restore rule. The intent is
// removed once every file is restored or reported as a conflict.
// actor and peer are who asked for the request that recovers.
func (h *Helper) recover(ctx context.Context, it *Intent, boot bool, actor string, peer Peer) Response {
	rec := AuditRecord{TS: h.cfg.Now(), Actor: actor, Peer: peer, Op: OpRecover}
	for _, f := range it.Files {
		rec.Files = append(rec.Files, AuditFile{Name: f.Name, Before: f.NewSHA, After: f.OriginalSHA})
	}
	resp := h.recoverIntent(ctx, it, boot)
	if it.RollbackDir != "" {
		resp.RollbackDir = filepath.Join(h.cfg.StateDir, rollbackDir, it.RollbackDir)
	}
	rec.Result, rec.Error, rec.Message, rec.Conflicts, rec.RollbackDir = resp.Result, resp.Error, resp.Message, resp.Conflicts, resp.RollbackDir
	rec.OrphanedMedia = resp.OrphanedMedia
	h.audit(rec)
	return resp
}

func (h *Helper) recoverIntent(ctx context.Context, it *Intent, boot bool) Response {
	// An intent of the exec sequence may have stopped standalone drives
	// too, which Libraries does not name.
	units := (len(it.Libraries) > 0 || it.Exec != nil) && !boot
	if it.Recovery == nil {
		it.Recovery = &Recovery{}
	}
	if units && it.Recovery.Step == "" {
		if err := h.cfg.Lifecycle.Stop(ctx, it); err != nil {
			return responseOf(fmt.Errorf("recovery: stopping: %w", err))
		}
		it.Recovery.Step = "stopped"
		if err := h.saveIntent(it); err != nil {
			return responseOf(err)
		}
	}
	conflicts, err := h.restore(it)
	if err != nil {
		return responseOf(fmt.Errorf("recovery: %w", err))
	}
	it.Recovery.Step = "restored"
	if err := h.saveIntent(it); err != nil {
		return responseOf(err)
	}
	if units {
		if err := h.cfg.Lifecycle.Start(ctx, it); err != nil {
			return responseOf(fmt.Errorf("recovery: starting: %w", err))
		}
	}
	if err := h.removeIntent(); err != nil {
		return responseOf(err)
	}
	msg := fmt.Sprintf("recovered the %s of %s by %q; plan again", it.Op, it.Started.Format("2006-01-02 15:04:05Z07:00"), it.Actor)
	if it.Op == OpService+":"+ActionStop && units && it.Exec != nil && stoppedActive(it.Exec.Stops) {
		// unwind starts again every unit the stop had stopped of a library
		// that ran: the stop did not finish, so the library is back as it
		// was.
		msg += "; the units the stop had stopped were started again"
	}
	if it.Op == OpService+":"+ActionStart && units && it.Exec != nil && stoppedActive(it.Exec.Recovery) {
		// The recovery's guardedStop stopped what the start had started,
		// and unwind leaves it down, as it was before the start.
		msg += "; the units the start had started were stopped again"
	}
	if len(conflicts) > 0 {
		msg += "; " + restoreConflicts(conflicts).Error()
	}
	resp := Response{Result: ResultRecovered, Message: msg, Conflicts: conflicts}
	if it.Exec != nil {
		resp.OrphanedMedia = it.Exec.NewMedia
	}
	return resp
}

// RecoverBoot is --recover-boot, run by mhvtl-control-apply-recover.service
// before mhvtl.target's units start (plan 3.2, "Boot recovery"). It
// restores the files of an apply that died with an earlier boot, and does
// nothing, successfully, unless all of these hold:
//
//   - intent.json exists and records a boot ID other than this boot's, so
//     the apply did not run in this boot;
//   - exclusive.lock is free (taken with LOCK_NB: waiting could deadlock
//     against a helper that waits for systemctl start), and intent.json,
//     read again under it, is the same intent (boot ID and start time);
//   - no vtllibrary@ or vtltape@ instance is active; if one is, recovery is
//     left to the runtime path (op recover), which can stop it.
//
// It reports what it did. An error means it could not tell or could not
// restore; the intent is then kept.
func (h *Helper) RecoverBoot(ctx context.Context) (string, error) {
	it, err := h.loadIntent()
	if err != nil {
		return "", err
	}
	if it == nil {
		return "no interrupted apply", nil
	}
	boot, err := h.bootID()
	if err != nil {
		return "", err
	}
	if it.BootID == boot {
		return "the interrupted apply ran in this boot; left to runtime recovery", nil
	}
	h.beforeBootLock()
	l, held, err := h.tryLock()
	if err != nil {
		return "", err
	}
	if held {
		return exclusiveHeld, nil
	}
	defer l.release()
	// Read again under the lock: a helper that held it until now may have
	// recovered that intent, or recovered it and died in a new apply.
	again, err := h.loadIntent()
	if err != nil {
		return "", err
	}
	if again == nil {
		return "no interrupted apply", nil
	}
	if again.BootID != it.BootID || !again.Started.Equal(it.Started) {
		return intentChanged, nil
	}
	active, err := h.activeInstances(ctx)
	if err != nil {
		return "", err
	}
	if len(active) > 0 {
		return "instances already active (" + strings.Join(active, ", ") + "); left to runtime recovery", nil
	}
	resp := h.recover(ctx, it, true, "boot recovery", Peer{})
	if resp.Result != ResultRecovered {
		return "", errors.New(resp.Message)
	}
	return resp.Message, nil
}

const (
	exclusiveHeld = lockFile + " is held; left to its holder"
	intentChanged = "intent.json changed before " + lockFile + " was taken; left to runtime recovery"
)

// activeInstances lists the vtllibrary@ and vtltape@ units in any state
// but inactive or failed.
func (h *Helper) activeInstances(ctx context.Context) ([]string, error) {
	out, err := h.cfg.Systemctl(ctx, "list-units", "--all", "--plain", "--no-legend", "--no-pager",
		"--type=service", "vtllibrary@*", "vtltape@*")
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w", err)
	}
	var active []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		// Some systemd versions mark a unit that is not found with a
		// leading bullet even with --plain.
		if len(f) > 0 && (f[0] == "●" || f[0] == "*") {
			f = f[1:]
		}
		if len(f) < 3 || !unitRE.MatchString(f[0]) {
			continue
		}
		if f[2] != "inactive" && f[2] != "failed" {
			active = append(active, f[0])
		}
	}
	return active, nil
}

// stoppedActive reports whether stops, verdicts of a guardedStop, stopped
// a unit of a library that was running.
func stoppedActive(stops []*LibStop) bool {
	for _, ls := range stops {
		if ls.Active && len(ls.Stopped) > 0 {
			return true
		}
	}
	return false
}
