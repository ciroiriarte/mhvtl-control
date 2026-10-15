// Command mhvtld is the mhVTL management daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mhvtld/internal/api"
	"mhvtld/internal/changer"
	"mhvtld/internal/ipc"
	"mhvtld/internal/rawdisk"
	"mhvtld/internal/webui"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	configDir := flag.String("config-dir", "/etc/mhvtl", "directory holding device.conf and library_contents.<id>")
	apiToken := flag.String("api-token", "", "bearer token required on /api/v1/* (falls back to MHVTLD_API_TOKEN)")
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
	flag.Parse()

	token := *apiToken
	if token == "" {
		token = os.Getenv("MHVTLD_API_TOKEN")
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("invalid --listen %q: %v", *listen, err)
	}
	if bindRefused(host, token, *insecure) {
		log.Fatalf("refusing to bind non-loopback %s without --api-token; pass --insecure to override", *listen)
	}

	cfg := api.Config{
		ConfigDir:  *configDir,
		Controller: tryNewController(),
		Changers: changer.NewManager(&changer.Locator{
			SysRoot:   "/sys",
			DevRoot:   "/dev",
			Overrides: changers,
			Open:      changer.OpenSG,
		}, changer.OpenSG),
		DiskOptions: rawdisk.Options{IncludeVirtual: *includeVirtual, ScanPaths: scanPaths},
		Formatter:   rawdisk.ToolFormatter{ToolPath: *formatTool},
		Token:       token,
		CORSOrigin:  *corsOrigin,
		StaticFS:    webui.FS(),
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
			log.Printf("mhvtld listening on %s (TLS)", *listen)
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			log.Printf("mhvtld listening on %s", *listen)
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

// tryNewController opens the mhvtl control queue. On failure it logs a warning
// and returns a nil Controller so the daemon still serves read endpoints while
// mount/unmount return 503.
func tryNewController() ipc.Controller {
	c, err := ipc.NewIPCController()
	if err != nil {
		log.Printf("warning: mhvtl control queue unavailable: %v (mount/unmount will return 503)", err)
		return nil
	}
	return c
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

// parseChangerFlag records one --changer <libid>=<path> override.
func parseChangerFlag(m map[int]string, v string) error {
	id, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return fmt.Errorf("want <libid>=/dev/sgN, got %q", v)
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 0 {
		return fmt.Errorf("bad library id %q", id)
	}
	m[n] = path
	return nil
}
