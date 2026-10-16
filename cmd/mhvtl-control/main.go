// Command mhvtl-control is the mhVTL management daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/api"
	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/drivestats"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/rawdisk"
	"mhvtl-control/internal/webui"
)

// version is set at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

// The options file mhvtl-control.service reads, and where 0.2.0 kept it.
const (
	envFile       = "/etc/mhvtl-control/mhvtl-control.env"
	legacyEnvFile = "/etc/mhvtl/mhvtl-control.env"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	configDir := flag.String("config-dir", "/etc/mhvtl", "directory holding device.conf and library_contents.<id>")
	apiToken := flag.String("api-token", "", "bearer token required on /api/v1/*, /metrics and /readyz (falls back to MHVTL_CONTROL_API_TOKEN)")
	corsOrigin := flag.String("cors-origin", "", "Access-Control-Allow-Origin value; empty disables CORS")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (enables HTTPS with --tls-key)")
	tlsKey := flag.String("tls-key", "", "TLS key file (enables HTTPS with --tls-cert)")
	insecure := flag.Bool("insecure", false, "allow binding a non-loopback address without a token")
	formatTool := flag.String("format-tool", "mhvtl-raw-format", "path to the raw-disk format tool")
	includeVirtual := flag.Bool("include-virtual", false, "include virtual block devices (loop/ram/dm/zram) in disk listing")
	var scanPaths []string
	flag.Func("scan-path", "explicit disk/image path to scan (repeatable)", func(v string) error {
		scanPaths = append(scanPaths, v)
		return nil
	})
	changers := map[int]string{}
	flag.Func("changer", "pin a library's changer node, <libid>=/dev/sgN, skipping sysfs discovery (repeatable)", func(v string) error {
		return parseChangerFlag(changers, v)
	})
	drives := map[int]string{}
	flag.Func("drive", "pin a tape drive's node for statistics and node display, <driveid>=/dev/sgN or a link to it, skipping sysfs discovery (repeatable)", func(v string) error {
		return parseChangerFlag(drives, v)
	})
	mktape := flag.String("mktape", "mktape", "path to mhVTL's mktape (creates media)")
	editTape := flag.String("edit-tape", "edit_tape", "path to mhVTL's edit_tape (edits media)")
	homeDir := flag.String("home-dir", "/var/lib/mhvtl", "media home of a library device.conf and mhvtl.conf give none for")
	args, legacyToken, legacy, err := legacyOptions(envFile, legacyEnvFile, os.Getenv)
	if err != nil {
		log.Fatalf("reading %s: %v", legacyEnvFile, err)
	}
	if legacy {
		log.Printf("deprecated: options read from %s; move it to %s", legacyEnvFile, envFile)
		// Ahead of the command line, so explicit flags still win.
		os.Args = append(append([]string{os.Args[0]}, args...), os.Args[1:]...)
		if legacyToken != "" {
			os.Setenv("MHVTL_CONTROL_API_TOKEN", legacyToken)
		}
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("mhvtl-control", version)
		return
	}

	token := *apiToken
	if token == "" {
		token = os.Getenv("MHVTL_CONTROL_API_TOKEN")
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("invalid --listen %q: %v", *listen, err)
	}
	if bindRefused(host, token, *insecure) {
		log.Fatalf("refusing to bind non-loopback %s without --api-token; pass --insecure to override", *listen)
	}

	locator := &changer.Locator{
		SysRoot:   "/sys",
		DevRoot:   "/dev",
		Overrides: changers,
		Open:      changer.OpenSG,
	}
	stats := &drivestats.Reader{Locator: locator, Overrides: drives, Open: changer.OpenSG}
	// Node display reads sysfs only: an INQUIRY fallback per drive on every
	// GET /libraries could stall on a hung physical drive.
	sysfsOnly := *locator
	sysfsOnly.Open = nil
	nodeDrives := &drivestats.Reader{Locator: &sysfsOnly, Overrides: drives}
	cfg := api.Config{
		ConfigDir:   *configDir,
		Controller:  ipc.NewIPCController(),
		Changers:    changer.NewManager(locator, changer.OpenSG),
		Media:       media.Tools{Mktape: *mktape, EditTape: *editTape, ConfigDir: *configDir},
		DefaultHome: *homeDir,
		Drives:      stats,
		Nodes:       changer.NodeResolver{Locator: &sysfsOnly, Drive: nodeDrives.Locate},
		DiskOptions: rawdisk.Options{IncludeVirtual: *includeVirtual, ScanPaths: scanPaths},
		Formatter:   rawdisk.ToolFormatter{ToolPath: *formatTool},
		Token:       token,
		CORSOrigin:  *corsOrigin,
		StaticFS:    webui.FS(),
		Version:     version,
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.NewServer(cfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if *tlsCert != "" && *tlsKey != "" {
			log.Printf("mhvtl-control %s listening on %s (TLS)", version, *listen)
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			log.Printf("mhvtl-control %s listening on %s", version, *listen)
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Fatalf("shutdown error: %v", err)
		}
	}
}

// isLoopbackHost reports whether host is treated as loopback for bind safety.
// This covers "localhost" and any loopback IP ("127.0.0.1"/127.x, "::1"). An
// EMPTY host is NOT loopback: "--listen :8080" binds the wildcard (all
// interfaces), exactly like "0.0.0.0:8080", so it must be subject to the
// no-token refusal.
func isLoopbackHost(host string) bool {
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// bindRefused reports whether the daemon must refuse to start: a non-loopback
// bind with no token and without the --insecure override.
func bindRefused(host, token string, insecure bool) bool {
	return !isLoopbackHost(host) && token == "" && !insecure
}

// legacyOptions reads the options a 0.2.0 install kept in legacy, for a
// daemon whose unit now reads cur. It does so only when cur is absent and
// neither MHVTL_CONTROL_OPTS nor MHVTL_CONTROL_API_TOKEN is set, that is when
// the service manager passed no options at all; ok is false otherwise, and
// when legacy is absent too. args is MHVTL_CONTROL_OPTS split on blanks, as
// systemd splits an unquoted $MHVTL_CONTROL_OPTS on ExecStart=. The file is
// parsed as the plain KEY=VALUE subset of systemd's EnvironmentFile= format
// that dist/mhvtl-control.env uses: comments, blank lines, and values in
// single or double quotes.
func legacyOptions(cur, legacy string, getenv func(string) string) (args []string, token string, ok bool, err error) {
	if getenv("MHVTL_CONTROL_OPTS") != "" || getenv("MHVTL_CONTROL_API_TOKEN") != "" {
		return nil, "", false, nil
	}
	if _, err := os.Stat(cur); !errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, nil
	}
	b, err := os.ReadFile(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		switch strings.TrimSpace(key) {
		case "MHVTL_CONTROL_OPTS":
			args = strings.Fields(val)
		case "MHVTL_CONTROL_API_TOKEN":
			token = val
		}
	}
	return args, token, true, nil
}

// parseChangerFlag records one --changer <libid>=<path> or --drive
// <driveid>=<path> override.
func parseChangerFlag(m map[int]string, v string) error {
	id, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return fmt.Errorf("want <libid>=/dev/sgN, got %q", v)
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 0 {
		return fmt.Errorf("bad id %q", id)
	}
	m[n] = path
	return nil
}
