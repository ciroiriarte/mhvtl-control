// Package apply is mhvtl-control-apply, the privileged helper that writes
// mhVTL's configuration for the mhvtl-control daemon (plan Phase 3). It
// runs as root, one instance per connection on a socket only the daemon's
// unit may use, and trusts nothing the daemon can influence: it
// authenticates the peer, takes the request apart under fixed limits,
// re-reads and re-validates the files itself, re-hashes each one right
// before replacing it, and keeps the state it reads back (intent, lock,
// rollback copies, audit) in directories the daemon cannot see.
//
// This is PR 3a: the request checks, validation, durable writes with an
// intent journal, recovery of the files of an apply that died, and boot
// recovery. Stopping and starting mhVTL's units (guardedStop, the exec
// sequence, force tokens) is PR 3b; until then an apply is refused with
// not_implemented, and a plan stops after validation.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
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
	// Lifecycle stops and restarts the units an interrupted apply had
	// touched (PR 3b). Nil until then: an intent naming libraries is then
	// left for a helper that has it.
	Lifecycle Lifecycle
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
		Logf:        logf,
		ReadTimeout: 30 * time.Second,
	}
}

// Lifecycle is the unit side of recovery, PR 3b's guardedStop (plan 3.3)
// in recover mode and the start sequence. Runtime recovery calls Stop
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
}

// New returns a helper.
func New(cfg Config) *Helper {
	return &Helper{cfg: cfg, killPoint: func(string) bool { return false }, beforeWrite: func(string) {}, beforeBootLock: func() {}}
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
	if req.Op == OpHello {
		st, err := h.state()
		if err != nil {
			return responseOf(err)
		}
		return Response{Result: ResultOK, State: st, Version: h.cfg.Version}
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
	resp := h.handle(req, &rec)
	if req.Op != OpPlan || resp.Result != ResultValidated {
		h.auditResponse(rec, resp)
	}
	return resp
}

func (h *Helper) auditResponse(rec AuditRecord, resp Response) {
	rec.Result, rec.Error, rec.Message, rec.Conflicts = resp.Result, resp.Error, resp.Message, resp.Conflicts
	h.audit(rec)
}

// handle checks a plan or apply; the caller holds the lock and found no
// intent.
func (h *Helper) handle(req *Request, rec *AuditRecord) Response {
	if req.Op == OpRecover {
		return Response{Result: ResultOK, Message: "nothing to recover"}
	}
	if req.ForceToken != "" {
		return Response{Result: ResultRefused, Error: ErrNotImplemented, Message: "force tokens come with the apply sequence (PR 3b)"}
	}

	files := newFileSet(h.cfg.ConfigDir)
	findings, err := h.check(req, files, rec)
	if err != nil {
		resp := responseOf(err)
		resp.Findings = findings
		return resp
	}
	if req.Op == OpPlan {
		return Response{Result: ResultValidated, Findings: findings}
	}
	return Response{Result: ResultRefused, Error: ErrNotImplemented, Findings: findings,
		Message: "the request passed every check; applying needs the unit lifecycle of PR 3b"}
}

// check runs every refusal of plan 3.2 on the request, in order: names and
// units, the legacy options file, the base hashes, the configuration path,
// mhvtl.conf keys and homes, then the full validation of the new files
// against the current ones and the host. It returns the validation
// findings when it got that far.
func (h *Helper) check(req *Request, files *fileSet, rec *AuditRecord) (*validate.Result, error) {
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

	// Validation runs without the live placement: reading it needs the
	// SG_IO guard of PR 3b, which then passes R1 here.
	host, hostErr := hostids.Read(h.cfg.SysRoot)
	res := validate.Validate(cur, nextInv, nil, host, validate.Options{
		Dispositions: req.Dispositions,
		ConfigDir:    h.cfg.ConfigDir,
		DefaultHome:  nextHome,
		HostErr:      hostErr,
	})
	if !res.OK() {
		return &res, refuse(ErrInvalid, "%d blocking finding(s), first: %s", len(res.Errors), res.Errors[0].Message)
	}
	return &res, nil
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
