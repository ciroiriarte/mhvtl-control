package apply

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// changerTimeout bounds one READ ELEMENT STATUS, as the daemon's changer
// queries (internal/changer queryTimeout).
const changerTimeout = 30 * time.Second

// turTimeout bounds one TEST UNIT READY.
const turTimeout = 10 * time.Second

// allowedOpcodes are the only CDBs the helper sends (plan 3.3, sgguard.go,
// item 7), with the length each must have: TEST UNIT READY, INQUIRY,
// MODE SENSE(6) and (10) and READ ELEMENT STATUS. The length is the one
// the opcode's group code fixes (6 bytes for 00h-1Fh, 10 for 20h-5Fh, 12
// for A0h-BFh) and the one internal/scsi's builders produce. Nothing that
// moves media or writes.
var allowedOpcodes = map[byte]struct {
	name string
	len  int
}{
	0x00: {"TEST UNIT READY", 6},
	0x12: {"INQUIRY", 6},
	0x1a: {"MODE SENSE(6)", 6},
	0x5a: {"MODE SENSE(10)", 10},
	0xb8: {"READ ELEMENT STATUS", 12},
}

// errOpcode is a CDB the guard refuses to send.
var errOpcode = errors.New("sgguard: opcode not allowed")

// guardedDevice passes only allowedOpcodes to the device under it; any
// other CDB fails before the ioctl.
type guardedDevice struct {
	changer.Device
}

func (g guardedDevice) Do(cdb, dataIn []byte, timeout time.Duration) (int, []byte, error) {
	if len(cdb) == 0 {
		return 0, nil, fmt.Errorf("%w: empty CDB", errOpcode)
	}
	op, ok := allowedOpcodes[cdb[0]]
	if !ok {
		return 0, nil, fmt.Errorf("%w: %02xh", errOpcode, cdb[0])
	}
	if len(cdb) != op.len {
		return 0, nil, fmt.Errorf("%w: %s CDB of %d bytes, not %d", errOpcode, op.name, len(cdb), op.len)
	}
	return g.Device.Do(cdb, dataIn, timeout)
}

var hctlRE = regexp.MustCompile(`^[0-9]+:[0-9]+:[0-9]+:[0-9]+$`)

// SCSIAccess is the helper's only way to the changers and drives (plan
// 3.3, sgguard.go). A device is named by its H:C:T:L, which survives a
// restart of the mhVTL daemons, and resolved to its sg node afresh on
// every open: the node changes on every restart (Phase 0 amendment 5), so
// a path taken before a stop is never reused. The sysfs type must be 1
// (tape) or 8 (changer) and equal to the type asked for, INQUIRY byte 0 is
// checked again after the open, and the opened device passes only
// allowedOpcodes.
type SCSIAccess struct {
	SysRoot, DevRoot string
	// Open opens an sg node; changer.OpenSG in production.
	Open changer.Opener
}

// Locate returns the H:C:T:L of the device of type typ with unit serial
// number serial.
func (a SCSIAccess) Locate(serial string, typ uint8) (string, error) {
	loc := changer.Locator{SysRoot: a.SysRoot, DevRoot: a.DevRoot, Open: a.discoveryOpen}
	path, err := loc.FindBySerial(serial, typ)
	if err != nil {
		return "", err
	}
	return a.hctlOf(filepath.Base(path))
}

// hctlOf reads the H:C:T:L of sg node name from sysfs: the target of
// class/scsi_generic/<name>/device.
func (a SCSIAccess) hctlOf(name string) (string, error) {
	target, err := os.Readlink(filepath.Join(a.SysRoot, "class", "scsi_generic", name, "device"))
	if err != nil {
		return "", err
	}
	hctl := filepath.Base(target)
	if !hctlRE.MatchString(hctl) {
		return "", fmt.Errorf("sgguard: %s: device link %q names no H:C:T:L", name, target)
	}
	return hctl, nil
}

// resolve returns the sg node of hctl now, and its sysfs type.
func (a SCSIAccess) resolve(hctl string) (node string, typ uint8, err error) {
	if !hctlRE.MatchString(hctl) {
		return "", 0, fmt.Errorf("sgguard: %q is not an H:C:T:L", hctl)
	}
	dir := filepath.Join(a.SysRoot, "bus", "scsi", "devices", hctl)
	b, err := os.ReadFile(filepath.Join(dir, "type"))
	if err != nil {
		return "", 0, err
	}
	switch strings.TrimSpace(string(b)) {
	case "1":
		typ = scsi.PeripheralSequential
	case "8":
		typ = scsi.PeripheralMediumChanger
	default:
		return "", 0, fmt.Errorf("sgguard: %s has SCSI type %s, neither a tape drive (1) nor a medium changer (8)", hctl, strings.TrimSpace(string(b)))
	}
	ents, err := os.ReadDir(filepath.Join(dir, "scsi_generic"))
	if err != nil {
		return "", 0, err
	}
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "sg") {
		return "", 0, fmt.Errorf("sgguard: %s has no single sg node", hctl)
	}
	return filepath.Join(a.DevRoot, ents[0].Name()), typ, nil
}

// open opens the device at hctl, which must be of type typ.
func (a SCSIAccess) open(hctl string, typ uint8) (changer.Device, error) {
	node, got, err := a.resolve(hctl)
	if err != nil {
		return nil, err
	}
	if got != typ {
		return nil, fmt.Errorf("sgguard: %s is of SCSI type %d, not %d", hctl, got, typ)
	}
	dev, err := a.Open(node)
	if err != nil {
		return nil, err
	}
	// The node may have been renumbered to another device between the
	// resolve and the open: it must still name hctl.
	if again, err := a.hctlOf(filepath.Base(node)); err != nil || again != hctl {
		dev.Close()
		return nil, fmt.Errorf("sgguard: %s no longer names %s (now %q): %v", node, hctl, again, err)
	}
	g := guardedDevice{dev}
	buf := make([]byte, 36)
	n, _, err := g.Do(scsi.InquiryCDB(false, 0, uint16(len(buf))), buf, turTimeout)
	if err == nil && n < 1 {
		err = scsi.ErrShortResponse
	}
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("sgguard: %s: INQUIRY: %w", hctl, err)
	}
	if buf[0]&0x1f != typ {
		dev.Close()
		return nil, fmt.Errorf("sgguard: %s: INQUIRY reports type %d, sysfs %d", hctl, buf[0]&0x1f, typ)
	}
	return g, nil
}

// discoveryOpen is the Locator's INQUIRY fallback, through the guard: the
// node's sysfs type decides the type checked.
func (a SCSIAccess) discoveryOpen(path string) (changer.Device, error) {
	hctl, err := a.hctlOf(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	_, typ, err := a.resolve(hctl)
	if err != nil {
		return nil, err
	}
	return a.open(hctl, typ)
}

// Inventory reads the element status of lib's changer at hctl, retrying
// once on a unit attention for a reset (06/29/xx): after a start the
// kernel's ch probe or another initiator may leave one (Phase 0 amendment
// 4).
func (a SCSIAccess) Inventory(lib config.Library, hctl string) (changer.Inventory, error) {
	node, _, err := a.resolve(hctl)
	if err != nil {
		return changer.Inventory{}, err
	}
	m := changer.NewManager(&changer.Locator{Overrides: map[int]string{lib.ID: node}}, func(string) (changer.Device, error) {
		return a.open(hctl, scsi.PeripheralMediumChanger)
	})
	inv, err := m.Inventory(lib)
	if resetAttention(err) {
		inv, err = m.Inventory(lib)
	}
	return inv, err
}

// Loaded reports whether the drive at hctl holds a cartridge: TEST UNIT
// READY is GOOD with one, NOT READY / MEDIUM NOT PRESENT (02/3A) without.
// A unit attention is retried once.
func (a SCSIAccess) Loaded(hctl string) (bool, error) {
	dev, err := a.open(hctl, scsi.PeripheralSequential)
	if err != nil {
		return false, err
	}
	defer dev.Close()
	tur := make([]byte, 6)
	_, _, err = dev.Do(tur, nil, turTimeout)
	var se *scsi.SenseError
	if errors.As(err, &se) && se.Key == 0x06 {
		_, _, err = dev.Do(tur, nil, turTimeout)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &se) && se.Key == 0x02 && se.ASC == 0x3a:
		return false, nil
	}
	return false, fmt.Errorf("%s: TEST UNIT READY: %w", hctl, err)
}

// resetAttention is a UNIT ATTENTION for a power on or reset (06/29/xx).
func resetAttention(err error) bool {
	var se *scsi.SenseError
	return errors.As(err, &se) && se.Key == 0x06 && se.ASC == 0x29
}
