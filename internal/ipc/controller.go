package ipc

import "fmt"

// Controller is the high-level library/drive control surface. Verb strings
// match those vtlcmd sends on the queue (see mhvtl usr/cmd/vtlcmd.c).
type Controller interface {
	Load(dev int64, slot int) error
	LoadMap(dev int64) error
	Unload(dev int64) error
	Online(dev int64) error
	Offline(dev int64) error
	List(dev int64) (string, error)
}

// IPCController drives devices over a real SysV queue keyed by QKey().
type IPCController struct {
	client    *Client
	timeoutMs int
}

var _ Controller = (*IPCController)(nil)

// NewIPCController opens the control queue (keyed by QKey()) without creating
// it: the mhvtl daemons own queue creation.
func NewIPCController() (*IPCController, error) {
	c := &Client{}
	if err := c.OpenQueue(QKey(), false); err != nil {
		return nil, err
	}
	return &IPCController{client: c, timeoutMs: 5000}, nil
}

// command sends text to dev and returns the device's reply text.
func (c *IPCController) command(dev int64, text string) (string, error) {
	if err := c.client.Send(dev, text); err != nil {
		return "", err
	}
	_, reply, err := c.client.Receive(c.timeoutMs)
	return reply, err
}

// Load loads the cartridge in the given storage slot into drive dev
// ("load <slot>").
func (c *IPCController) Load(dev int64, slot int) error {
	_, err := c.command(dev, fmt.Sprintf("load %d", slot))
	return err
}

// LoadMap loads media from the import/export map ("load map").
func (c *IPCController) LoadMap(dev int64) error {
	_, err := c.command(dev, "load map")
	return err
}

// Unload unloads the cartridge from drive dev ("unload").
func (c *IPCController) Unload(dev int64) error {
	_, err := c.command(dev, "unload")
	return err
}

// Online brings a library online ("online").
func (c *IPCController) Online(dev int64) error {
	_, err := c.command(dev, "online")
	return err
}

// Offline takes a library offline ("offline").
func (c *IPCController) Offline(dev int64) error {
	_, err := c.command(dev, "offline")
	return err
}

// List returns the device's listing reply ("list").
func (c *IPCController) List(dev int64) (string, error) {
	return c.command(dev, "list")
}

// Call records one FakeController invocation. Arg carries the slot for Load
// and is 0 for verbs that take no numeric argument.
type Call struct {
	Method string
	Dev    int64
	Arg    int
}

// FakeController is an in-memory Controller for api-layer tests. It records
// every call and returns its configured Reply/Err.
type FakeController struct {
	Calls []Call
	Reply string
	Err   error
}

var _ Controller = (*FakeController)(nil)

func (f *FakeController) record(method string, dev int64, arg int) {
	f.Calls = append(f.Calls, Call{Method: method, Dev: dev, Arg: arg})
}

// Load records a Load call.
func (f *FakeController) Load(dev int64, slot int) error {
	f.record("Load", dev, slot)
	return f.Err
}

// LoadMap records a LoadMap call.
func (f *FakeController) LoadMap(dev int64) error {
	f.record("LoadMap", dev, 0)
	return f.Err
}

// Unload records an Unload call.
func (f *FakeController) Unload(dev int64) error {
	f.record("Unload", dev, 0)
	return f.Err
}

// Online records an Online call.
func (f *FakeController) Online(dev int64) error {
	f.record("Online", dev, 0)
	return f.Err
}

// Offline records an Offline call.
func (f *FakeController) Offline(dev int64) error {
	f.record("Offline", dev, 0)
	return f.Err
}

// List records a List call and returns the configured Reply.
func (f *FakeController) List(dev int64) (string, error) {
	f.record("List", dev, 0)
	return f.Reply, f.Err
}
