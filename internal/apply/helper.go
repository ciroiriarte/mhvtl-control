// Package apply is mhvtl-control-apply, the privileged helper that writes
// mhVTL's configuration for the mhvtl-control daemon (plan Phase 3). It
// runs as root, one instance per connection on a socket only the daemon's
// unit may use, and trusts nothing the daemon can influence: it
// authenticates the peer, takes the request apart under fixed limits,
// re-reads and re-validates the files itself, re-hashes each one right
// before replacing it, and keeps the state it reads back (intent, lock,
// rollback copies, audit) in directories the daemon cannot see.
//
// An apply stops the units the change affects through guardedStop,
// writes the files behind an intent journal, starts the units again, and
// rolls back on failure (exec.go); an apply that died is recovered by the
// next request, or at boot.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/validate"
)

// Config is where the helper finds things and how it reaches the system.
// Tests replace every field.
type Config struct {
	// ConfigDir is mhVTL's configuration directory (--config-dir).
	ConfigDir string
	// StateDir holds intent.json, audit.jsonl and rollback/
	// (StateDirectory=mhvtl-control-apply).
	StateDir string
	// RuntimeDir holds exclusive.lock (RuntimeDirectory=mhvtl-control-apply).
	RuntimeDir string
	// LegacyEnv is the daemon's options file at its 0.2.0 path.
	LegacyEnv string
	// DefaultHome is the media home when mhvtl.conf sets no
	// MHVTL_HOME_PATH.
	DefaultHome string
	// SysRoot is sysfs, for the host's SCSI identities.
	SysRoot string
	// BootIDPath is /proc/sys/kernel/random/boot_id.
	BootIDPath string
	Version    string

	Getenv func(string) string
	Now    func() time.Time
	// Systemctl runs systemctl with args and returns its stdout.
	Systemctl func(ctx context.Context, args ...string) ([]byte, error)
	// CheckPeer authenticates the caller.
	CheckPeer func(*net.UnixConn) (Peer, error)
	// Journal sends structured fields to the journal.
	Journal func(fields map[string]string)
	Logf    func(format string, a ...any)
	// Queue is the mhVTL message queue; Devices the changers and drives,
	// through the SG_IO guard.
	Queue   Queue
	Devices Devices
	// Lifecycle stops and restarts the units an interrupted apply had
	// touched; New sets the exec sequence's when it is nil.
	Lifecycle Lifecycle
	// ApplyLockDir holds the cooperative apply.lock (ApplyLockDir).
	ApplyLockDir string
	// MoveHomeDir holds mhvtl-move-home's journals (MoveHomeDir).
	MoveHomeDir string
	// Generators are where mhVTL's systemd generator may be installed:
	// with it, daemon-reload derives the instances from device.conf.
	Generators []string
	// MakeVTLMedia is make_vtl_media, a path or a name looked up in PATH,
	// read for its --per-library-home option.
	MakeVTLMedia string
	// JournalTail returns the last journal lines of a unit, for the
	// response of a rollback.
	JournalTail func(ctx context.Context, unit string) ([]byte, error)
	// ReadTimeout bounds reading the request.
	ReadTimeout time.Duration
}

// DefaultConfig is the installed helper's configuration.
func DefaultConfig(configDir, version string) Config {
	logf := log.Printf
	return Config{
		ConfigDir:   configDir,
		StateDir:    "/var/lib/mhvtl-control-apply",
		RuntimeDir:  "/run/mhvtl-control-apply",
		LegacyEnv:   "/etc/mhvtl/mhvtl-control.env",
		DefaultHome: "/var/lib/mhvtl",
		SysRoot:     "/sys",
		BootIDPath:  "/proc/sys/kernel/random/boot_id",
		Version:     version,
		Getenv:      os.Getenv,
		Now:         time.Now,
		Systemctl: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "systemctl", args...).Output()
		},
		CheckPeer: CheckPeer,
		Journal: func(fields map[string]string) {
			if err := SendJournal(JournalSocket, fields); err != nil {
				logf("%s", fields["MESSAGE"])
			}
		},
		Queue:        ipc.NewIPCController(),
		Devices:      SCSIAccess{SysRoot: "/sys", DevRoot: "/dev", Open: changer.OpenSG},
		ApplyLockDir: ApplyLockDir,
		MoveHomeDir:  MoveHomeDir,
		// SYSTEMD_GENERATOR_DIR, /usr/lib/systemd/system-generators by
		// default (mhvtl config.mk at 852b78a), under /lib before the
		// /usr merge.
		Generators: []string{
			"/usr/lib/systemd/system-generators/mhvtl-device-conf-generator",
			"/lib/systemd/system-generators/mhvtl-device-conf-generator",
		},
		MakeVTLMedia: "make_vtl_media",
		JournalTail: func(ctx context.Context, unit string) ([]byte, error) {
			return exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "cat", "-n", "20", "-u", unit).Output()
		},
		Logf:        logf,
		ReadTimeout: 30 * time.Second,
	}
}

// Lifecycle is the unit side of recovery: guardedStop (plan 3.3) in
// recover mode, and the start sequence. Runtime recovery calls Stop
// before it restores the files of an intent that names libraries, and
// Start after; boot recovery calls neither, as nothing runs yet.
type Lifecycle interface {
	Stop(ctx context.Context, it *Intent) error
	Start(ctx context.Context, it *Intent) error
}

// Helper serves one request.
type Helper struct {
	cfg Config
	// Test seams: killPoint simulates the helper dying after a step;
	// beforeWrite runs before each file is written; beforeBootLock runs
	// between RecoverBoot's first read of the intent and its lock.
	killPoint      func(step string) bool
	beforeWrite    func(name string)
	beforeBootLock func()
	// poll is the interval of the waits for a started unit.
	poll time.Duration
}

// New returns a helper.
func New(cfg Config) *Helper {
	h := &Helper{cfg: cfg, killPoint: func(string) bool { return false }, beforeWrite: func(string) {}, beforeBootLock: func() {}, poll: 200 * time.Millisecond}
	if h.cfg.Lifecycle == nil {
		h.cfg.Lifecycle = lifecycle{h}
	}
	return h
}

// Serve answers the one request on conn, the socket systemd passed as
// stdin (Accept=yes). The answer goes back on the same socket, never on
// stdout: Go dies of SIGPIPE on a broken stdout or stderr, and main ignores
// SIGPIPE besides. An error writing it is returned.
func (h *Helper) Serve(ctx context.Context, conn *net.UnixConn) error {
	defer conn.Close()
	resp := h.serve(ctx, conn)
	resp.V = Version
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(h.cfg.ReadTimeout))
	_, err = conn.Write(append(b, '\n'))
	return err
}

func (h *Helper) serve(ctx context.Context, conn *net.UnixConn) Response {
	// The peer is checked first, while the caller surely waits, and the
	// request is read either way, so that a refused caller gets its answer
	// rather than a reset connection.
	peer, perr := h.cfg.CheckPeer(conn)
	conn.SetReadDeadline(time.Now().Add(h.cfg.ReadTimeout))
	req, err := DecodeRequest(conn)
	if perr != nil {
		h.cfg.Logf("refused peer pid %d uid %d: %v", peer.PID, peer.UID, perr)
		h.audit(AuditRecord{TS: h.cfg.Now(), Peer: peer, Op: "connect", Result: ResultRefused, Error: ErrPeer, Message: perr.Error()})
		return Response{Result: ResultRefused, Error: ErrPeer, Message: "caller not allowed"}
	}
	if err != nil {
		resp := responseOf(err)
		h.audit(AuditRecord{TS: h.cfg.Now(), Peer: peer, Op: "decode", Result: resp.Result, Error: resp.Error, Message: resp.Message})
		return resp
	}
	return h.Handle(ctx, peer, req)
}

// Handle carries out a decoded request from an authenticated peer.
func (h *Helper) Handle(ctx context.Context, peer Peer, req *Request) Response {
	// Hello is not audited: it changes nothing, and the daemon asks it for
	// every GET /config/capabilities and write request (at most every 2 s),
	// which would bury the changes in the audit log.
	if req.Op == OpHello {
		st, err := h.state()
		if err != nil {
			return responseOf(err)
		}
		return Response{Result: ResultOK, State: st, Version: h.cfg.Version}
	}
	// Units is not audited either: it changes nothing, and the daemon asks
	// it whenever a stage first edits a library. It takes no lock: a unit
	// an apply stops or starts meanwhile shows as deactivating or
	// activating, which the daemon does not take as down.
	if req.Op == OpUnits {
		states, err := h.libraryStates(ctx)
		if err != nil {
			return responseOf(err)
		}
		return Response{Result: ResultOK, LibraryStates: states}
	}
	rec := AuditRecord{TS: h.cfg.Now(), Actor: req.Actor, Peer: peer, Op: req.Op, Units: req.ExpectUnits}
	l, held, err := h.waitLock()
	if err != nil {
		return responseOf(err)
	}
	if held {
		resp := Response{Result: ResultInProgress, Message: "another apply, or mhvtl-move-home, holds " + lockFile}
		h.auditResponse(rec, resp)
		return resp
	}
	defer l.release()

	// The lock is free, so an intent left behind is a dead apply's:
	// recover it and do nothing else. recover audits itself.
	it, err := h.loadIntent()
	if err != nil {
		resp := responseOf(err)
		h.auditResponse(rec, resp)
		return resp
	}
	if it != nil {
		return h.recover(ctx, it, false, req.Actor, peer)
	}
	resp := h.handle(ctx, req, &rec)
	if req.Op != OpPlan || resp.Result != ResultValidated {
		h.auditResponse(rec, resp)
	}
	return resp
}

// libraryStates is the ActiveState of the vtllibrary@ instance of each
// library in device.conf, as readLive reads it.
func (h *Helper) libraryStates(ctx context.Context) (map[int]string, error) {
	b, err := os.ReadFile(filepath.Join(h.cfg.ConfigDir, deviceConf))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	states := map[int]string{}
	for _, st := range conffile.ParseDeviceConf(b).Stanzas() {
		if st.Kind != "Library" {
			continue
		}
		if states[st.ID], err = showProp(ctx, &h.cfg, libraryUnit(st.ID), "ActiveState"); err != nil {
			return nil, err
		}
	}
	return states, nil
}

func (h *Helper) auditResponse(rec AuditRecord, resp Response) {
	rec.Result, rec.Error, rec.Message, rec.Conflicts = resp.Result, resp.Error, resp.Message, resp.Conflicts
	rec.OrphanedMedia, rec.ForcedUnloads, rec.Unplaced = resp.OrphanedMedia, resp.ForcedUnloads, resp.Unplaced
	h.audit(rec)
}

// handle checks a plan or apply, and carries out an apply; the caller
// holds exclusive.lock and found no intent.
func (h *Helper) handle(ctx context.Context, req *Request, rec *AuditRecord) Response {
	if req.Op == OpRecover {
		return Response{Result: ResultOK, Message: "nothing to recover"}
	}
	if req.Op == OpPlan && req.ForceToken != "" {
		return Response{Result: ResultRefused, Error: ErrBadRequest, Message: "a force token goes with an apply"}
	}
	// Exec step 1, after exclusive.lock: the daemon's cooperative lock,
	// then no media home move half done.
	if req.Op == OpApply {
		l, err := h.applyLock()
		if err != nil {
			return responseOf(err)
		}
		if l != nil {
			defer l.Close()
		}
		if err := checkMoveHome(h.cfg.MoveHomeDir); err != nil {
			return responseOf(err)
		}
	}

	files := newFileSet(h.cfg.ConfigDir)
	c, err := h.check(ctx, req, files, rec)
	var resp Response
	if err == nil {
		var forced map[int][]changer.DriveState
		forced, resp, err = h.planChecks(ctx, req, c)
		if err == nil && resp.Result == "" {
			if req.Op == OpPlan {
				resp = Response{Result: ResultValidated, Warnings: c.warnings}
			} else {
				resp = h.execute(ctx, req, c, forced, rec)
			}
		}
	}
	if err != nil {
		resp = responseOf(err)
	}
	if c != nil {
		resp.Findings = c.res
		if len(resp.Warnings) == 0 {
			resp.Warnings = c.warnings
		}
		if c.plan != nil && resp.Units == nil {
			resp.Units, resp.Notes = c.plan.Units(), c.plan.Notes
		}
	}
	return resp
}

// planChecks are plan 3.3 exec step 2 after the budget, which check ran:
// the media refusals, then drive_loaded. A plan refused only for
// drive_loaded gets a force token; an apply with a valid one goes on with
// those drives forced, and returns them. A non-empty response ends the
// request with it.
func (h *Helper) planChecks(ctx context.Context, req *Request, c *checked) (map[int][]changer.DriveState, Response, error) {
	if err := h.checkMediaPlan(c); err != nil {
		return nil, Response{}, err
	}
	forced, standalone, err := h.loadedDrives(ctx, c)
	if err != nil {
		return nil, Response{}, err
	}
	var loaded []string
	for _, id := range sortedInts(forced) {
		for _, d := range forced[id] {
			b := d.Barcode
			if b == "" {
				b = "a cartridge its stopped library does not know of"
			}
			loaded = append(loaded, fmt.Sprintf("library %d drive slot %d (%s)", id, d.Number, b))
		}
	}
	for _, id := range standalone {
		loaded = append(loaded, fmt.Sprintf("drive %d", id))
	}
	if len(loaded) == 0 {
		// A force token finds nothing to force: the change's own units.
		if req.Op == OpApply && req.ForceToken != "" {
			return nil, Response{}, checkExpectUnits(req.ExpectUnits, *c.plan)
		}
		return nil, Response{}, nil
	}
	// Forced, the apply also stops and starts the vtltape@ of each drive it
	// unloads (plan amendment 2): those units are what the plan shows, the
	// token binds and expect_units must name.
	fp := withForced(*c.plan, c, forced)
	binding := bindingOf(req, fp.Units(), c.live, loaded)
	if req.Op == OpApply && req.ForceToken != "" {
		// The response lists the forced units. The token, which binds
		// them, is used up only once expect_units names them.
		c.plan = &fp
		nonce, expiry, err := h.verifyForce(req.ForceToken, binding)
		if err != nil {
			return nil, Response{}, err
		}
		if err := checkExpectUnits(req.ExpectUnits, fp); err != nil {
			return nil, Response{}, err
		}
		if err := checkBudget(fp); err != nil {
			return nil, Response{}, err
		}
		if err := h.useForce(nonce, expiry); err != nil {
			return nil, Response{}, err
		}
		return forced, Response{}, nil
	}
	resp := Response{Result: ResultRefused, Error: ErrDriveLoaded, Units: fp.Units(), Notes: fp.Notes,
		Message: "loaded drives the apply would stop: " + strings.Join(loaded, ", ") + "; unload them, or apply with the force token to put each cartridge back in its slot"}
	if req.Op == OpPlan {
		tok, exp, err := h.mintForce(binding)
		if err != nil {
			return nil, Response{}, err
		}
		resp.ForceToken, resp.ForceExpires = tok, &exp
	}
	return nil, resp, nil
}

func deviceConfOf(get func(string) (fileState, error)) (*conffile.DeviceConf, error) {
	st, err := get(deviceConf)
	if err != nil {
		return nil, err
	}
	return conffile.ParseDeviceConf(st.data), nil
}

// changedContents lists the libraries whose library_contents the request
// writes with other bytes, or deletes.
func changedContents(req *Request, files *fileSet) (map[int]bool, error) {
	out := map[int]bool{}
	for _, f := range req.Files {
		n, ok := libraryOf(f.Name)
		if !ok {
			continue
		}
		st, err := files.current(f.Name)
		if err != nil {
			return nil, err
		}
		if !st.exists || !bytes.Equal(st.data, f.Content) {
			out[n] = true
		}
	}
	for _, d := range req.Delete {
		if n, ok := libraryOf(d.Name); ok {
			out[n] = true
		}
	}
	return out, nil
}

// check runs every refusal of plan 3.2 on the request, in order: names and
// units, the legacy options file, the base hashes, the configuration path,
// mhvtl.conf keys and homes, then the full validation of the new files
// against the current ones, the live placement (R1) and the host, then the
// unit plan and its budget. It returns what it found when it got as far
// as the validation.
func (h *Helper) check(ctx context.Context, req *Request, files *fileSet, rec *AuditRecord) (*checked, error) {
	if err := checkNames(req); err != nil {
		return nil, err
	}
	if err := checkUnits(req.ExpectUnits); err != nil {
		return nil, err
	}
	if err := h.checkLegacyEnv(); err != nil {
		return nil, err
	}
	if err := checkBases(req, files); err != nil {
		return nil, err
	}
	next := files.withChanges(req)
	// The hashes go in every record from here; the diff only once the
	// request passed the guards, so a refused one cannot fill the audit.
	auditedFiles, diff, truncated := auditFiles(changesOf(req), files)
	rec.Files = auditedFiles

	curConf, err := mhvtlConfOf(files.current)
	if err != nil {
		return nil, err
	}
	nextConf, err := mhvtlConfOf(next.next)
	if err != nil {
		return nil, err
	}
	if err := h.checkConfigPath(curConf, nextConf); err != nil {
		return nil, err
	}
	curRaw, err := files.current(mhvtlConf)
	if err != nil {
		return nil, err
	}
	nextRaw, err := next.next(mhvtlConf)
	if err != nil {
		return nil, err
	}
	if err := deriveMhvtlConf(curRaw.data, nextRaw.data); err != nil {
		return nil, err
	}

	// Every file written must be one mhVTL reads as written.
	for _, f := range req.Files {
		var werr error
		switch {
		case f.Name == deviceConf:
			werr = conffile.ParseDeviceConf(f.Content).Writable()
		case f.Name == mhvtlConf:
			werr = conffile.ParseMhvtlConf(f.Content).Writable()
		default:
			werr = conffile.ParseLibContents(f.Content).Writable()
		}
		if werr != nil {
			return nil, refuse(ErrNotWritable, "%s: %v", f.Name, werr)
		}
	}

	cur, err := config.ReadInventory(h.cfg.ConfigDir, reader(files.current))
	if err != nil && !missingDeviceConf(err) {
		return nil, err
	}
	nextInv, err := config.ReadInventory(h.cfg.ConfigDir, reader(next.next))
	if err != nil && !missingDeviceConf(err) {
		return nil, err
	}
	if err := checkLibraries(req, libraryIDs(cur), libraryIDs(nextInv)); err != nil {
		return nil, err
	}
	curHome, nextHome := h.defaultHome(curConf), h.defaultHome(nextConf)
	if err := checkHomes(cur, nextInv, curHome, nextHome); err != nil {
		return nil, err
	}

	rec.Diff, rec.DiffTruncated = diff, truncated

	live, invs, err := h.readLive(ctx, cur)
	if err != nil {
		return nil, err
	}
	host, hostErr := hostids.Read(h.cfg.SysRoot)
	res := validate.Validate(cur, nextInv, live, host, validate.Options{
		Dispositions:   req.Dispositions,
		ConfigDir:      h.cfg.ConfigDir,
		DefaultHome:    nextHome,
		CurDefaultHome: curHome,
		HostErr:        hostErr,
	})
	c := &checked{res: &res, cur: cur, next: nextInv, curHome: curHome, nextHome: nextHome, nextFiles: next, live: live, inv: invs}
	if !res.OK() {
		return c, refuse(ErrInvalid, "%d blocking finding(s), first: %s", len(res.Errors), res.Errors[0].Message)
	}

	// The units the change affects, which expect_units must name exactly,
	// and the time stopping and starting them may take.
	if c.curDC, err = deviceConfOf(files.current); err != nil {
		return c, err
	}
	if c.nextDC, err = deviceConfOf(next.next); err != nil {
		return c, err
	}
	contents, err := changedContents(req, files)
	if err != nil {
		return c, err
	}
	plan := derivePlan(c.curDC, c.nextDC, cur, nextInv, contents, curConf, nextConf, nextHome)
	c.plan = &plan
	// A plan reports the units; the daemon echoes them in expect_units on
	// the apply, which alone must name them exactly. A forced apply's
	// units include the drives it unloads, which planChecks finds.
	if req.Op == OpApply && req.ForceToken == "" {
		if err := checkExpectUnits(req.ExpectUnits, plan); err != nil {
			return c, err
		}
	}
	if err := checkBudget(plan); err != nil {
		return c, err
	}
	c.warnings = missingCartridges(c)
	return c, nil
}

func mhvtlConfOf(get func(string) (fileState, error)) (*conffile.MhvtlConf, error) {
	st, err := get(mhvtlConf)
	if err != nil {
		return nil, err
	}
	return conffile.ParseMhvtlConf(st.data), nil
}

// missingDeviceConf is a configuration without device.conf: no library.
func missingDeviceConf(err error) bool {
	var dce *config.DeviceConfError
	return errors.As(err, &dce) && errors.Is(err, fs.ErrNotExist)
}

func libraryIDs(inv config.Inventory) map[int]bool {
	ids := map[int]bool{}
	for _, l := range inv.Libraries {
		ids[l.ID] = true
	}
	return ids
}

// responseOf turns an error into a response: a refusal, a conflict, the
// files a restore left alone, or an internal error.
func responseOf(err error) Response {
	var r *refusal
	if errors.As(err, &r) {
		return Response{Result: ResultRefused, Error: r.code, Message: r.msg}
	}
	if c, ok := conflictOf(err); ok {
		cur := c.Current
		return Response{Result: ResultConflict, File: c.Path, CurrentSHA256: &cur, Message: err.Error()}
	}
	var rc restoreConflicts
	if errors.As(err, &rc) {
		return Response{Result: ResultError, Error: ErrInternal, Message: err.Error(), Conflicts: rc}
	}
	return Response{Result: ResultError, Error: ErrInternal, Message: err.Error()}
}
