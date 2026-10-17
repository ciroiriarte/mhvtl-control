// Command mhvtl-control-apply is mhvtl-control's privileged configuration
// helper. mhvtl-control-apply.socket starts one instance per connection
// with the connected socket as stdin (Accept=yes); with --recover-boot it
// instead recovers, at boot, an apply that died with the previous boot.
// See internal/apply.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"mhvtl-control/internal/apply"
)

// version is set at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

// configure, when set, adjusts the configuration main uses (tests).
var configure func(*apply.Config)

func main() {
	// A client that hangs up must not kill the helper mid-apply. The reply
	// goes to the socket on fd 0, where Go turns SIGPIPE into EPIPE
	// anyway; ignoring it covers the journal streams on fds 1 and 2 too,
	// which Go would otherwise die on (os/signal, "SIGPIPE").
	signal.Ignore(syscall.SIGPIPE)
	// SIGTERM (a stop, or RuntimeMaxSec=) cancels the context: Go runs no
	// deferred cleanup on a signal it does not handle.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, configure))
}

// run is main without the process: adjust, when not nil, changes the default
// configuration (tests point it at temporary directories).
func run(ctx context.Context, args []string, stdin *os.File, stdout io.Writer, adjust func(*apply.Config)) int {
	log.SetFlags(0)
	fs := flag.NewFlagSet("mhvtl-control-apply", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	configDir := fs.String("config-dir", "/etc/mhvtl", "mhVTL's configuration directory")
	recoverBoot := fs.Bool("recover-boot", false, "recover an apply that died with an earlier boot, then exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "mhvtl-control-apply", version)
		return 0
	}
	cfg := apply.DefaultConfig(*configDir, version)
	if adjust != nil {
		adjust(&cfg)
	}
	h := apply.New(cfg)

	if *recoverBoot {
		msg, err := h.RecoverBoot(ctx)
		if err != nil {
			log.Printf("boot recovery: %v", err)
			return 1
		}
		log.Printf("boot recovery: %s", msg)
		return 0
	}

	c, err := net.FileConn(stdin)
	if err != nil {
		log.Printf("stdin is not a socket (start through mhvtl-control-apply.socket): %v", err)
		return 2
	}
	conn, ok := c.(*net.UnixConn)
	if !ok {
		log.Printf("stdin is not a Unix socket")
		return 2
	}
	if err := h.Serve(ctx, conn); err != nil {
		log.Printf("sending the reply: %v", err)
		return 1
	}
	return 0
}
