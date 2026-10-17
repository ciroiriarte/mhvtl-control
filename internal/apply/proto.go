package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"

	"mhvtl-control/internal/validate"
)

// Request limits (plan 3.2, proto.go).
const (
	// MaxRequest bounds the whole request as sent, content in base64
	// included.
	MaxRequest = 1 << 20
	// MaxFiles bounds the files written plus the files deleted.
	MaxFiles = 64
	// MaxFileSize bounds one file's content, and a file read from the
	// configuration directory.
	MaxFileSize = 256 << 10
	// maxActor bounds the free-text actor recorded in the audit.
	maxActor = 256
)

// Version is the protocol version, Request.V and Response.V.
const Version = 1

// Operations.
const (
	// OpHello reports the helper's state and changes nothing.
	OpHello = "hello"
	// OpPlan checks a change set and reports what an apply would do.
	OpPlan = "plan"
	// OpApply checks and applies a change set.
	OpApply = "apply"
	// OpRecover recovers an apply that died, and does nothing else.
	OpRecover = "recover"
)

// Request is one call to the helper: a JSON object, decoded strictly.
type Request struct {
	V  int    `json:"v"`
	Op string `json:"op"`
	// Files are the files to write, whole.
	Files []File `json:"files,omitempty"`
	// Delete are the files to remove: only a library_contents.<N> whose
	// library the new device.conf no longer has.
	Delete []Delete `json:"delete,omitempty"`
	// Dispositions say where the cartridges leaving a library go
	// (validate.Disposition).
	Dispositions []validate.Disposition `json:"dispositions,omitempty"`
	// ExpectUnits are the units the daemon expects the apply to stop and
	// start. Each must be a vtllibrary@N or vtltape@N instance.
	ExpectUnits []string `json:"expect_units,omitempty"`
	// ForceToken is the confirmation of a forced apply (plan 3.4).
	ForceToken string `json:"force_token,omitempty"`
	// Actor says who asked, for the audit: the daemon's authenticated
	// user, or a description of the client.
	Actor string `json:"actor,omitempty"`
}

// File is a file to write. Content is base64 in JSON, so any bytes go
// through unchanged: mhVTL's files are not guaranteed to be UTF-8.
type File struct {
	Name string `json:"name"`
	// BaseSHA256 is the hex SHA-256 of the file the change was made
	// against; empty means the file must not exist yet.
	BaseSHA256 string `json:"base_sha256"`
	Content    []byte `json:"content"`
}

// Delete is a file to remove. BaseSHA256 is checked as for a File, so a
// file edited by hand since it was read is not removed.
type Delete struct {
	Name       string `json:"name"`
	BaseSHA256 string `json:"base_sha256"`
}

// Results.
const (
	// ResultOK answers hello, and recover when there was nothing to do.
	ResultOK = "ok"
	// ResultValidated answers a plan that passed every check.
	ResultValidated = "validated"
	// ResultRefused answers a request the helper will not carry out; Error
	// says why.
	ResultRefused = "refused"
	// ResultConflict answers a request whose base hash no longer matches
	// the file (the daemon's 409).
	ResultConflict = "conflict"
	// ResultInProgress answers while another apply (or the move-home CLI)
	// holds the helper's lock.
	ResultInProgress = "apply_in_progress"
	// ResultRecovered answers a request that found an apply that had died
	// and recovered it. Nothing else was done: the client must plan again.
	ResultRecovered = "recovered"
	// ResultError answers a failure that is not a refusal.
	ResultError = "error"
)

// States of a hello.
const (
	StateReady = "ready"
	// StateLegacyEnv: /etc/mhvtl/mhvtl-control.env exists, and the helper
	// refuses every change while it does.
	StateLegacyEnv = "legacy_env_blocks"
	// StateInProgress: an apply, a plan or the move-home CLI holds the
	// helper's lock.
	StateInProgress = "apply_in_progress"
	// StateIncomplete: an apply died and left its intent; the next plan,
	// apply or recover recovers it.
	StateIncomplete = "incomplete_apply"
)

// Response is the helper's answer.
type Response struct {
	V      int    `json:"v"`
	Result string `json:"result"`
	// State and Version answer hello.
	State   string `json:"state,omitempty"`
	Version string `json:"version,omitempty"`
	// Error is a machine-readable reason (one of the Err* codes) for
	// refused and error results; Message says it in words.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	// File and CurrentSHA256 describe a conflict: the file and its hash
	// now, "" when it no longer exists.
	File          string  `json:"file,omitempty"`
	CurrentSHA256 *string `json:"current_sha256,omitempty"`
	// Findings are the validation results of a plan or a refused apply.
	Findings *validate.Result `json:"findings,omitempty"`
	// Conflicts are the files a recovery left alone because they changed
	// after the apply died.
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// Conflict is a file a recovery did not restore.
type Conflict struct {
	File          string `json:"file"`
	CurrentSHA256 string `json:"current_sha256"`
}

// Error codes of refused and error results.
const (
	ErrBadRequest     = "bad_request"
	ErrOversize       = "oversize"
	ErrName           = "name"
	ErrUnit           = "unit"
	ErrPeer           = "peer"
	ErrLegacyEnv      = "legacy_env"
	ErrConfigPath     = "config_path"
	ErrMhvtlConfKey   = "mhvtl_conf_key"
	ErrMhvtlConfValue = "mhvtl_conf_value"
	// ErrMhvtlConfDerived: the new mhvtl.conf is not the current one with
	// only editable values changed (deriveMhvtlConf).
	ErrMhvtlConfDerived = "mhvtl_conf_not_derived"
	ErrHomeForbidden    = "home_forbidden"
	ErrHomeInUse        = "home_in_use"
	ErrNotWritable      = "not_writable"
	ErrInvalid          = "invalid"
	ErrNotImplemented   = "not_implemented"
	ErrInternal         = "internal"
)

// refusal is a request the helper will not carry out.
type refusal struct {
	code, msg string
}

func (r *refusal) Error() string { return r.code + ": " + r.msg }

func refuse(code, format string, a ...any) error {
	return &refusal{code: code, msg: fmt.Sprintf(format, a...)}
}

// DecodeRequest reads one request from r: at most MaxRequest bytes, no
// field the Request does not have, nothing after it but blanks, and within
// the limits. Every error is a refusal.
func DecodeRequest(r io.Reader) (*Request, error) {
	lr := &io.LimitedReader{R: r, N: MaxRequest + 1}
	dec := json.NewDecoder(lr)
	dec.DisallowUnknownFields()
	var req Request
	err := dec.Decode(&req)
	if err == nil {
		// Reads to EOF: the client half-closes after the request.
		if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
			err = errors.New("data after the request")
		}
	}
	if lr.N <= 0 {
		return nil, refuse(ErrOversize, "request larger than %d bytes", MaxRequest)
	}
	if err != nil {
		return nil, refuse(ErrBadRequest, "%v", err)
	}
	if req.V != Version {
		return nil, refuse(ErrBadRequest, "protocol version %d, want %d", req.V, Version)
	}
	switch req.Op {
	case OpHello, OpPlan, OpApply, OpRecover:
	default:
		return nil, refuse(ErrBadRequest, "unknown op %q", req.Op)
	}
	if n := len(req.Files) + len(req.Delete); n > MaxFiles {
		return nil, refuse(ErrOversize, "%d files, at most %d", n, MaxFiles)
	}
	for _, f := range req.Files {
		if len(f.Content) > MaxFileSize {
			return nil, refuse(ErrOversize, "%q: %d bytes, at most %d", f.Name, len(f.Content), MaxFileSize)
		}
	}
	if len(req.Actor) > maxActor {
		return nil, refuse(ErrBadRequest, "actor longer than %d bytes", maxActor)
	}
	for _, c := range req.Actor {
		if !unicode.IsPrint(c) {
			return nil, refuse(ErrBadRequest, "actor has a non-printable character")
		}
	}
	return &req, nil
}
