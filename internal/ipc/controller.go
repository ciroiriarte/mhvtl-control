package ipc

import (
	"errors"
	"fmt"
	"os"
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
	OpenMap(lib int64) error
	CloseMap(lib int64) error
	EjectMap(lib int64) error
}

// IPCController drives libraries over the SysV queue keyed by QKey(). The
// queue is opened lazily and reopened when mhVTL recreates it, so the daemon
// may start before mhVTL and survives mhVTL restarts.
type IPCController struct {
	mu        sync.Mutex
	key       int
	client    *Client // nil until opened, and after the cached id went stale
	reply     int64   // this process's reply mtype (ReplyType)
	timeoutMs int
	// late holds devices whose last command timed out: their reply may
	// still arrive, and must not be taken as the answer to the next one.
	late map[int64]bool
}

var _ Controller = (*IPCController)(nil)

// NewIPCController returns a controller for the queue keyed by QKey(). It
// never fails: the queue is opened on first use.
func NewIPCController() *IPCController {
	return &IPCController{key: QKey(), reply: ReplyType(os.Getpid()), timeoutMs: 5000}
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
	cl := &Client{Reply: c.reply}
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
// reply. Replies come back on this process's own mtype (c.reply), so vtlcmd
// and other clients never see them, nor this one theirs. What can still be
// queued there is a leftover from one of our earlier timed-out requests: one
// from another device is discarded and the wait continues until the
// deadline. A receive error is never followed by a resend: the command was
// already delivered. Caller holds c.mu, so concurrent requests never read
// each other's replies.
func (c *IPCController) command(dev int64, text string, replies bool) (string, error) {
	if c.late[dev] && c.client != nil {
		// Replies carry no request id, so a late one from dev would be read
		// as this command's answer. Drain whatever is queued first.
		for {
			if _, _, err := c.client.Receive(0); err != nil {
				break
			}
		}
		delete(c.late, dev)
	}
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
			c.markLate(dev)
			return "", ErrTimeout
		}
		snd, reply, err := c.client.Receive(int(remaining.Milliseconds()))
		if err != nil {
			if staleQueue(err) {
				c.client = nil
			}
			if errors.Is(err, ErrTimeout) {
				c.markLate(dev)
			}
			return "", err
		}
		if snd == dev {
			return reply, nil
		}
	}
}

// markLate records that dev's reply to a timed-out command may still come.
func (c *IPCController) markLate(dev int64) {
	if c.late == nil {
		c.late = map[int64]bool{}
	}
	c.late[dev] = true
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

// ListMap returns the library's answer to "list map": the barcodes in its
// MAP slots, concatenated in slot order with no separator, "" for an empty
// MAP (list_map, mhvtl usr/cmd/vtllibrary.c:248-264 at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/cmd/vtllibrary.c#L248-L264).
func (c *IPCController) ListMap(lib int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.command(lib, "list map", true)
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

// OpenMap opens the library's MAP ("open map"). While it is open the changer
// cannot reach the MAP slots.
func (c *IPCController) OpenMap(lib int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expectOK(lib, "open map")
}

// CloseMap closes the library's MAP ("close map").
func (c *IPCController) CloseMap(lib int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expectOK(lib, "close map")
}

// EjectMap takes every cartridge out of the MAP, as an operator emptying it
// would: "open map", "empty map", "close map". The cartridges leave the
// library; their media stays in the home directory. vtllibrary only empties an
// open MAP, and the MAP is closed again even when emptying is refused. The lock
// is held across all three commands.
func (c *IPCController) EjectMap(lib int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.expectOK(lib, "open map"); err != nil {
		return err
	}
	emptyErr := c.expectOK(lib, "empty map")
	if err := c.expectOK(lib, "close map"); err != nil && emptyErr == nil {
		return err
	}
	return emptyErr
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
	// Map is what ListMap returns.
	Map string
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

// ListMap records a ListMap call and returns Map.
func (f *FakeController) ListMap(lib int64) (string, error) {
	f.record("ListMap", lib, "")
	return f.Map, f.Err
}

// ImportMap records an ImportMap call.
func (f *FakeController) ImportMap(lib int64, barcode string) error {
	f.record("ImportMap", lib, barcode)
	return f.Err
}

// OpenMap records an OpenMap call.
func (f *FakeController) OpenMap(lib int64) error {
	f.record("OpenMap", lib, "")
	return f.Err
}

// CloseMap records a CloseMap call.
func (f *FakeController) CloseMap(lib int64) error {
	f.record("CloseMap", lib, "")
	return f.Err
}

// EjectMap records an EjectMap call.
func (f *FakeController) EjectMap(lib int64) error {
	f.record("EjectMap", lib, "")
	return f.Err
}
