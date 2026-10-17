// Package applyclient is the daemon's side of mhvtl-control-apply: it dials
// the helper's socket once per request, as the socket unit starts one helper
// instance per connection (plan 3.2, "Daemon side").
package applyclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
	"time"

	"mhvtl-control/internal/apply"
)

// DefaultSocket is mhvtl-control-apply.socket's ListenStream=.
const DefaultSocket = "/run/mhvtl-control-apply.sock"

// What Hello reports, the helper value of GET /api/v1/config/capabilities.
const (
	// Absent: no helper listens; the socket unit is not enabled.
	Absent = "absent"
	// Present: the helper answers and accepts changes.
	Present = "present"
	// LegacyEnv: the helper refuses changes while
	// /etc/mhvtl/mhvtl-control.env exists.
	LegacyEnv = apply.StateLegacyEnv
	// InProgress: an apply (or mhvtl-move-home) runs now.
	InProgress = apply.StateInProgress
	// Incomplete: an apply died; the next plan, apply or recover recovers
	// it.
	Incomplete = apply.StateIncomplete
)

// Client calls the helper.
type Client struct {
	// Socket is the helper's socket; empty means DefaultSocket.
	Socket string
	// Timeout bounds a call whose context has no deadline; zero means 30
	// seconds. An apply can take far longer (up to the helper's
	// RuntimeMaxSec=): give it a context with its own deadline.
	Timeout time.Duration
}

// ErrAbsent means no helper listens on the socket.
var ErrAbsent = errors.New("mhvtl-control-apply is not enabled (no socket)")

// Do sends req and returns the helper's response. The request is
// half-closed after it is written, which tells the helper it is complete.
// A socket that is missing or refuses the connection is ErrAbsent.
func (c *Client) Do(ctx context.Context, req apply.Request) (apply.Response, error) {
	socket := c.Socket
	if socket == "" {
		socket = DefaultSocket
	}
	if _, ok := ctx.Deadline(); !ok {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req.V = apply.Version
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", socket)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return apply.Response{}, fmt.Errorf("%w: %v", ErrAbsent, err)
	}
	if err != nil {
		return apply.Response{}, err
	}
	conn := nc.(*net.UnixConn)
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return apply.Response{}, err
	}
	if err := conn.CloseWrite(); err != nil {
		return apply.Response{}, err
	}
	b, err := io.ReadAll(io.LimitReader(conn, apply.MaxRequest+1))
	if err != nil {
		return apply.Response{}, err
	}
	var resp apply.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		return apply.Response{}, fmt.Errorf("helper response: %w", err)
	}
	return resp, nil
}

// Hello returns the helper's state: Absent, Present, LegacyEnv,
// InProgress or Incomplete.
func (c *Client) Hello(ctx context.Context) (string, error) {
	resp, err := c.Do(ctx, apply.Request{Op: apply.OpHello})
	if errors.Is(err, ErrAbsent) {
		return Absent, nil
	}
	if err != nil {
		return "", err
	}
	if resp.Result != apply.ResultOK {
		return "", fmt.Errorf("helper refused hello: %s %s", resp.Error, resp.Message)
	}
	switch resp.State {
	case apply.StateReady:
		return Present, nil
	case LegacyEnv, InProgress, Incomplete:
		return resp.State, nil
	}
	return "", fmt.Errorf("helper reported unknown state %q", resp.State)
}
