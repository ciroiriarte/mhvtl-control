package ipc

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
)

// ErrUnavailable means the mhvtl control queue does not exist (mhVTL is not
// running) or cannot be opened.
var ErrUnavailable = errors.New("ipc: mhvtl control queue unavailable")

// ReplyError is a library's refusal of a command, carrying its reply text
// verbatim (e.g. "MAP Full", "barcode already in library").
type ReplyError struct {
	Command string
	Reply   string
}

func (e *ReplyError) Error() string {
	return fmt.Sprintf("%s: %s", e.Command, e.Reply)
}

// Controller is the library control surface carried by the message queue.
// Moving cartridges between slots and drives is NOT a queue operation: it is
// SCSI MOVE MEDIUM to the changer (see internal/changer).
type Controller interface {
	Online(lib int64) error
	Offline(lib int64) error
	ImportMap(lib int64, barcode string) error
}

// IPCController drives libraries over the SysV queue keyed by QKey(). The
// queue is opened lazily and reopened when mhVTL recreates it, so the daemon
// may start before mhVTL and survives mhVTL restarts.
type IPCController struct {
	mu        sync.Mutex
	key       int
	client    *Client // nil until opened, and after the cached id went stale
	timeoutMs int
}

var _ Controller = (*IPCController)(nil)

// NewIPCController returns a controller for the queue keyed by QKey(). It
// never fails: the queue is opened on first use.
func NewIPCController() *IPCController {
	return &IPCController{key: QKey(), timeoutMs: 5000}
}

// staleQueue reports whether err means the cached queue id no longer names a
// live queue (removed, or removed and recreated under a new id).
func staleQueue(err error) bool {
	return errors.Is(err, syscall.EIDRM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT)
}

// open opens the queue (without creating it: the mhvtl daemons own it) unless
// a live id is cached. Caller holds c.mu.
func (c *IPCController) open() error {
	if c.client != nil {
		return nil
	}
	cl := &Client{}
	if err := cl.OpenQueue(c.key, false); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	c.client = cl
	return nil
}

// send delivers text to dev. A send that fails because the cached id went
// stale did not happen, so it is retried once on a freshly opened queue.
// Caller holds c.mu.
func (c *IPCController) send(dev int64, text string) error {
	for attempt := 0; ; attempt++ {
		if err := c.open(); err != nil {
			return err
		}
		err := c.client.Send(dev, text)
		if err == nil {
			return nil
		}
		if !staleQueue(err) {
			return err
		}
		c.client = nil
		if attempt > 0 {
			return fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
}

// command sends text to dev and, when the C handler replies, returns dev's
// reply. Replies share one message type (VTLCMD_Q), so one from any other
// device (a leftover from an earlier timed-out request) is discarded and the
// wait continues until the deadline. A receive error is never followed by a
// resend: the command was already delivered. Caller holds c.mu, so concurrent
// requests never read each other's replies.
func (c *IPCController) command(dev int64, text string, replies bool) (string, error) {
	if err := c.send(dev, text); err != nil {
		return "", err
	}
	if !replies {
		return "", nil
	}
	deadline := time.Now().Add(time.Duration(c.timeoutMs) * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", ErrTimeout
		}
		snd, reply, err := c.client.Receive(int(remaining.Milliseconds()))
		if err != nil {
			if staleQueue(err) {
				c.client = nil
			}
			return "", err
		}
		if snd == dev {
			return reply, nil
		}
	}
}

// Online brings a library online ("online"; vtllibrary sends no reply).
func (c *IPCController) Online(lib int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.command(lib, "online", false)
	return err
}

// Offline takes a library offline ("offline"; vtllibrary sends no reply).
func (c *IPCController) Offline(lib int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.command(lib, "offline", false)
	return err
}

// ImportMap places a cartridge into an empty MAP slot, as an operator would:
// "open map", "load map <barcode>", "close map", each answered by the library.
// vtllibrary refuses a load into a closed MAP, and the changer only reaches
// MAP slots while the MAP is closed, so the MAP is closed again even when the
// load is refused. The lock is held across all three commands.
func (c *IPCController) ImportMap(lib int64, barcode string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.expectOK(lib, "open map"); err != nil {
		return err
	}
	loadErr := c.expectOK(lib, "load map "+barcode)
	if err := c.expectOK(lib, "close map"); err != nil && loadErr == nil {
		return err
	}
	return loadErr
}

// expectOK runs a replying command whose success reply is "OK". Caller holds
// c.mu.
func (c *IPCController) expectOK(lib int64, text string) error {
	reply, err := c.command(lib, text, true)
	if err != nil {
		return err
	}
	if reply != "OK" {
		return &ReplyError{Command: text, Reply: reply}
	}
	return nil
}

// Call records one FakeController invocation.
type Call struct {
	Method string
	Dev    int64
	Arg    string
}

// FakeController is an in-memory Controller for api-layer tests. It records
// every call and returns its configured Err.
type FakeController struct {
	mu    sync.Mutex
	Calls []Call
	Err   error
}

var _ Controller = (*FakeController)(nil)

func (f *FakeController) record(method string, dev int64, arg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: method, Dev: dev, Arg: arg})
}

// Online records an Online call.
func (f *FakeController) Online(lib int64) error {
	f.record("Online", lib, "")
	return f.Err
}

// Offline records an Offline call.
func (f *FakeController) Offline(lib int64) error {
	f.record("Offline", lib, "")
	return f.Err
}

// ImportMap records an ImportMap call.
func (f *FakeController) ImportMap(lib int64, barcode string) error {
	f.record("ImportMap", lib, barcode)
	return f.Err
}
