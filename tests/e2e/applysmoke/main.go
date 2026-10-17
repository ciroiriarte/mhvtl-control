// Command applysmoke is the client of the Phase 3.5 VM smoke test
// (tests/e2e/apply-smoke.sh). It talks to mhvtl-control-apply's socket
// directly, with internal/applyclient, as the Phase 4 write API will: it
// builds hello, plan and apply requests from files on disk and prints the
// helper's JSON response. It also turns `mtx status` output into the
// library_contents a client builds from the live view, and into a
// placement listing the script diffs.
//
// It is a test tool: make build and make install leave it out, and no
// package ships it.
//
// The helper serves only processes in mhvtl-control.service's cgroup
// (internal/apply/peer.go). The script therefore moves the shell that execs
// this client into that cgroup first, by writing its PID to
// /sys/fs/cgroup/system.slice/mhvtl-control.service/cgroup.procs. That is a
// test-only technique: it needs root and a running mhvtl-control.service,
// and a process moved there is killed with the daemon if the daemon stops.
//
// Usage:
//
//	applysmoke request -config-dir DIR [-delete NAME]... [-dispositions FILE] [-actor TEXT] FILE...
//	applysmoke send -op hello|plan|apply|recover [-socket PATH] [-timeout DUR] [-units-from RESP] [-force-from RESP] [REQUEST]
//	applysmoke contents -from LIBRARY_CONTENTS -mtx MTX_STATUS [-slots N]
//	applysmoke placement -mtx MTX_STATUS | -contents LIBRARY_CONTENTS
//
// request prints a request without an op: each FILE is the new content of
// the file of the same base name in DIR, whose current hash is the base
// ("" when DIR lacks it). send sets the op, and the expected units and force
// token from an earlier response, then prints the response; it fails only
// when no response arrives.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/validate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "applysmoke:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: applysmoke request|send|contents|placement ...")
	}
	fs := flag.NewFlagSet("applysmoke "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "request":
		return request(fs, args[1:], stdout)
	case "send":
		return send(fs, args[1:], stdout)
	case "contents":
		return contents(fs, args[1:], stdout)
	case "placement":
		return placement(fs, args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// names is a repeatable string flag.
type names []string

func (n *names) String() string     { return fmt.Sprint(*n) }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

// baseOf returns the hash of dir/name, "" when it does not exist.
func baseOf(dir, name string) (string, error) {
	_, sha, err := conffile.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return sha, err
}

func request(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	configDir := fs.String("config-dir", "/etc/mhvtl", "mhVTL's configuration directory, for the base hashes")
	dispositions := fs.String("dispositions", "", "JSON file with the dispositions")
	actor := fs.String("actor", "apply-smoke", "actor recorded in the audit")
	var deletes names
	fs.Var(&deletes, "delete", "file to delete (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req := apply.Request{Actor: *actor}
	for _, path := range fs.Args() {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		base, err := baseOf(*configDir, name)
		if err != nil {
			return err
		}
		req.Files = append(req.Files, apply.File{Name: name, BaseSHA256: base, Content: content})
	}
	for _, name := range deletes {
		_, base, err := conffile.ReadFile(filepath.Join(*configDir, name))
		if err != nil {
			return err
		}
		req.Delete = append(req.Delete, apply.Delete{Name: name, BaseSHA256: base})
	}
	if *dispositions != "" {
		b, err := os.ReadFile(*dispositions)
		if err != nil {
			return err
		}
		var d []validate.Disposition
		if err := json.Unmarshal(b, &d); err != nil {
			return fmt.Errorf("%s: %w", *dispositions, err)
		}
		req.Dispositions = d
	}
	return printJSON(stdout, req)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func send(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	op := fs.String("op", "", "hello, plan, apply or recover")
	socket := fs.String("socket", applyclient.DefaultSocket, "the helper's socket")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for the response")
	unitsFrom := fs.String("units-from", "", "response whose units become expect_units")
	forceFrom := fs.String("force-from", "", "response whose force_token is sent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var req apply.Request
	if fs.NArg() > 0 {
		if err := readJSON(fs.Arg(0), &req); err != nil {
			return err
		}
	}
	req.Op = *op
	if *unitsFrom != "" {
		var r apply.Response
		if err := readJSON(*unitsFrom, &r); err != nil {
			return err
		}
		req.ExpectUnits = r.Units
	}
	if *forceFrom != "" {
		var r apply.Response
		if err := readJSON(*forceFrom, &r); err != nil {
			return err
		}
		if r.ForceToken == "" {
			return fmt.Errorf("%s has no force_token", *forceFrom)
		}
		req.ForceToken = r.ForceToken
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := applyclient.Client{Socket: *socket}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	return printJSON(stdout, resp)
}

// printJSON prints v on one line, for jq and for logs.
func printJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
