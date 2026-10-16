// Package changer drives an mhVTL library's SCSI medium changer directly:
// it discovers the changer's /dev/sgN node, reads the live element map and
// element status, and moves cartridges with MOVE MEDIUM — exactly what
// `mtx load/unload` does. mhVTL has no message-queue verb for moving a tape
// between a library slot and a drive; going through the changer keeps the
// library's inventory consistent and lets the library unload the drive
// itself before a move (mhvtl usr/smc.c move_drive2slot).
package changer

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// Command timeouts. A move waits for the drive daemon to load or unload the
// cartridge, so it gets far more time than an inventory query.
const (
	queryTimeout = 30 * time.Second
	moveTimeout  = 5 * time.Minute
)

// elementStatusAlloc is the initial READ ELEMENT STATUS allocation length; a
// report that does not fit is re-read once at its exact size.
const elementStatusAlloc = 64 << 10

// maxAlloc is the largest 24-bit allocation length.
const maxAlloc = 0xffffff

// unitAttentionRetries is how many times a command is reissued after UNIT
// ATTENTION (power-on/reset, mode parameters or inquiry data changed), which
// reports a state change rather than a failure of the command.
const unitAttentionRetries = 2

var (
	// ErrUnavailable means the library's changer could not be found or opened.
	ErrUnavailable = errors.New("changer unavailable")
	// ErrBusy means another operation, typically a cartridge move, holds the
	// changer; returned by TryInventory instead of waiting.
	ErrBusy = errors.New("changer busy with another operation")
	// ErrUnknownDrive means the drive id is not a drive of this library.
	ErrUnknownDrive = errors.New("drive not in library")
	// ErrOutOfRange means a drive position, slot or MAP number is outside the
	// changer's element map.
	ErrOutOfRange = errors.New("element number out of range")
	// ErrDriveEmpty means an unmount was asked of a drive holding no cartridge.
	ErrDriveEmpty = errors.New("drive is empty")
	// ErrNoEmptySlot means an unmount found no empty storage slot to return
	// the cartridge to.
	ErrNoEmptySlot = errors.New("no empty storage slot")
	// ErrMAPFull means an export found no empty MAP slot.
	ErrMAPFull = errors.New("no empty MAP slot")
	// ErrNoMAP means the library has no import/export element.
	ErrNoMAP = errors.New("library has no MAP")
	// ErrMAPOpen means the MAP is open, so the changer cannot reach its slots.
	ErrMAPOpen = errors.New("MAP is open: close it before moving cartridges in or out")
	// ErrNotFound means no slot, MAP slot or drive of the library holds the
	// barcode asked for.
	ErrNotFound = errors.New("barcode not in library")
	// ErrBadLocation means a location names a kind of element the operation
	// does not move to or from.
	ErrBadLocation = errors.New("unsupported location")
)

// Device is an open changer node.
type Device interface {
	scsi.Device
	Close() error
}

// Opener opens the changer node at path.
type Opener func(path string) (Device, error)

// OpenSG is the production Opener.
func OpenSG(path string) (Device, error) {
	d, err := scsi.OpenSG(path)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Location names an element by kind ("drive", "slot" or "map") and its
// 1-based number within that kind.
type Location struct {
	Kind   string `json:"kind"`
	Number int    `json:"number"`
}

// Move describes a completed MOVE MEDIUM.
type Move struct {
	From    Location `json:"from"`
	To      Location `json:"to"`
	Barcode string   `json:"barcode,omitempty"`
}

// Source selects where a mount takes its cartridge from: a storage slot or,
// when Slot is zero, a MAP (import/export) slot.
type Source struct {
	Slot int
	MAP  int
}

// SlotState is the live state of one storage or MAP slot.
type SlotState struct {
	Number  int
	Address uint16
	Full    bool
	Barcode string
}

// DriveState is the live state of one data transfer element. SourceSlot or
// SourceMAP is the slot the loaded cartridge came from when the changer
// reports a valid source element (SValid).
type DriveState struct {
	Number     int
	Address    uint16
	Full       bool
	Barcode    string
	Except     bool
	SourceSlot int
	SourceMAP  int
}

// Inventory is a live READ ELEMENT STATUS snapshot, indexed by 1-based number
// (element i+1 at index i). MAPOpen is nil when the library has no MAP; mhVTL
// reports MAP slots accessible to the changer only while the MAP is closed.
type Inventory struct {
	Drives  []DriveState
	Slots   []SlotState
	MAPs    []SlotState
	MAPOpen *bool
}

// Manager serves changer operations for all libraries. Each operation
// resolves and opens the changer afresh, so an mhVTL restart (which can
// renumber the sg nodes) needs no daemon restart, and operations on one
// changer are serialized.
type Manager struct {
	locator *Locator
	open    Opener

	mu    sync.Mutex
	locks map[int]*sync.Mutex
}

// NewManager returns a Manager that finds changers with locator and opens them
// with open.
func NewManager(locator *Locator, open Opener) *Manager {
	return &Manager{locator: locator, open: open, locks: map[int]*sync.Mutex{}}
}

func (m *Manager) lock(libID int) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[libID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[libID] = l
	}
	return l
}

// with runs fn against lib's open changer while holding its lock.
func (m *Manager) with(lib config.Library, fn func(s *session) error) error {
	l := m.lock(lib.ID)
	l.Lock()
	defer l.Unlock()
	return m.run(lib, fn)
}

// tryWith is with, but returns ErrBusy instead of waiting when another
// operation holds the lock. A move can hold it for minutes.
func (m *Manager) tryWith(lib config.Library, fn func(s *session) error) error {
	l := m.lock(lib.ID)
	if !l.TryLock() {
		return ErrBusy
	}
	defer l.Unlock()
	return m.run(lib, fn)
}

// run opens lib's changer and runs fn against it; the caller holds its lock.
func (m *Manager) run(lib config.Library, fn func(s *session) error) error {
	path, err := m.locator.Find(lib)
	if err != nil {
		return err
	}
	dev, err := m.open(path)
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrUnavailable, path, err)
	}
	defer dev.Close()
	s := &session{dev: dev}
	if s.emap, err = s.elementMap(); err != nil {
		return err
	}
	return fn(s)
}

// Inventory reads the live state of lib's drives, storage slots and MAP.
func (m *Manager) Inventory(lib config.Library) (Inventory, error) {
	var inv Inventory
	err := m.with(lib, func(s *session) error {
		var err error
		inv, err = s.inventory()
		return err
	})
	return inv, err
}

// TryInventory is Inventory, but returns ErrBusy at once when another
// operation on lib's changer is in progress, for callers such as metrics
// that must not wait behind a cartridge move.
func (m *Manager) TryInventory(lib config.Library) (Inventory, error) {
	var inv Inventory
	err := m.tryWith(lib, func(s *session) error {
		var err error
		inv, err = s.inventory()
		return err
	})
	return inv, err
}

// Mount moves a cartridge from a storage slot (from.Slot) or MAP slot
// (from.MAP) into drive driveID of lib.
func (m *Manager) Mount(lib config.Library, driveID int, from Source) (Move, error) {
	ord, err := driveOrdinal(lib, driveID)
	if err != nil {
		return Move{}, err
	}
	var mv Move
	err = m.with(lib, func(s *session) error {
		dst, ok := s.emap.DTE.Address(ord)
		if !ok {
			return fmt.Errorf("%w: drive position %d (changer has %d drives)", ErrOutOfRange, ord, s.emap.DTE.Count)
		}
		var src uint16
		if from.Slot > 0 {
			if src, ok = s.emap.Storage.Address(from.Slot); !ok {
				return fmt.Errorf("%w: slot %d (changer has %d slots)", ErrOutOfRange, from.Slot, s.emap.Storage.Count)
			}
			mv.From = Location{Kind: "slot", Number: from.Slot}
		} else {
			if src, ok = s.emap.IE.Address(from.MAP); !ok {
				return fmt.Errorf("%w: MAP slot %d (changer has %d MAP slots)", ErrOutOfRange, from.MAP, s.emap.IE.Count)
			}
			mv.From = Location{Kind: "map", Number: from.MAP}
		}
		mv.To = Location{Kind: "drive", Number: ord}
		return s.move(src, dst)
	})
	return mv, err
}

// Unmount moves the cartridge in drive driveID of lib to storage slot toSlot,
// or when toSlot is zero to the slot it came from (the drive's valid source
// element, if that slot is empty) else the first empty storage slot.
func (m *Manager) Unmount(lib config.Library, driveID, toSlot int) (Move, error) {
	ord, err := driveOrdinal(lib, driveID)
	if err != nil {
		return Move{}, err
	}
	var mv Move
	err = m.with(lib, func(s *session) error {
		src, ok := s.emap.DTE.Address(ord)
		if !ok {
			return fmt.Errorf("%w: drive position %d (changer has %d drives)", ErrOutOfRange, ord, s.emap.DTE.Count)
		}
		mv.From = Location{Kind: "drive", Number: ord}
		var dst uint16
		if toSlot > 0 {
			if dst, ok = s.emap.Storage.Address(toSlot); !ok {
				return fmt.Errorf("%w: slot %d (changer has %d slots)", ErrOutOfRange, toSlot, s.emap.Storage.Count)
			}
			mv.To = Location{Kind: "slot", Number: toSlot}
		} else {
			var err error
			if dst, mv.To, mv.Barcode, err = s.defaultDestination(src); err != nil {
				return err
			}
		}
		return s.move(src, dst)
	})
	return mv, err
}

// Transfer moves a cartridge between storage and MAP slots (kinds "slot" and
// "map"). Drives are loaded and emptied with Mount and Unmount.
func (m *Manager) Transfer(lib config.Library, from, to Location) (Move, error) {
	var mv Move
	err := m.with(lib, func(s *session) error {
		src, err := s.slotAddress(from)
		if err != nil {
			return err
		}
		dst, err := s.slotAddress(to)
		if err != nil {
			return err
		}
		if from.Kind == "map" || to.Kind == "map" {
			if err := s.requireMAPClosed(); err != nil {
				return err
			}
		}
		mv = Move{From: from, To: to}
		return s.move(src, dst)
	})
	return mv, err
}

// ExportSource selects the cartridge an export takes to the MAP: the one in
// storage slot Slot, in drive Drive (a drive id), or wherever Barcode is.
// Exactly one is set.
type ExportSource struct {
	Slot    int
	Drive   int
	Barcode string
}

// Export moves a cartridge to the first empty MAP slot, from where the
// operator takes it out of the library.
func (m *Manager) Export(lib config.Library, from ExportSource) (Move, error) {
	var mv Move
	err := m.with(lib, func(s *session) error {
		if s.emap.IE.Count == 0 {
			return ErrNoMAP
		}
		inv, err := s.inventory()
		if err != nil {
			return err
		}
		if inv.MAPOpen != nil && *inv.MAPOpen {
			return ErrMAPOpen
		}
		var src uint16
		switch {
		case from.Slot > 0:
			if src, err = s.slotAddress(Location{Kind: "slot", Number: from.Slot}); err != nil {
				return err
			}
			mv.From, mv.Barcode = Location{Kind: "slot", Number: from.Slot}, inv.Slots[from.Slot-1].Barcode
		case from.Drive > 0:
			ord, err := driveOrdinal(lib, from.Drive)
			if err != nil {
				return err
			}
			if ord > len(inv.Drives) {
				return fmt.Errorf("%w: drive position %d (changer has %d drives)", ErrOutOfRange, ord, len(inv.Drives))
			}
			src = inv.Drives[ord-1].Address
			mv.From, mv.Barcode = Location{Kind: "drive", Number: ord}, inv.Drives[ord-1].Barcode
		default:
			loc, addr, ok := inv.find(from.Barcode)
			if !ok {
				return fmt.Errorf("%w: %s", ErrNotFound, from.Barcode)
			}
			if loc.Kind == "map" {
				return fmt.Errorf("%w: %s is already in MAP slot %d", ErrBadLocation, from.Barcode, loc.Number)
			}
			src, mv.From, mv.Barcode = addr, loc, from.Barcode
		}
		for _, sl := range inv.MAPs {
			if !sl.Full {
				mv.To = Location{Kind: "map", Number: sl.Number}
				return s.move(src, sl.Address)
			}
		}
		return ErrMAPFull
	})
	return mv, err
}

// Store moves the cartridge with barcode from the MAP slot holding it to the
// first empty storage slot: the second half of an import.
func (m *Manager) Store(lib config.Library, barcode string) (Move, error) {
	var mv Move
	err := m.with(lib, func(s *session) error {
		inv, err := s.inventory()
		if err != nil {
			return err
		}
		if inv.MAPOpen != nil && *inv.MAPOpen {
			return ErrMAPOpen
		}
		var src *SlotState
		for i := range inv.MAPs {
			if inv.MAPs[i].Full && inv.MAPs[i].Barcode == barcode {
				src = &inv.MAPs[i]
				break
			}
		}
		if src == nil {
			return fmt.Errorf("%w: %s is in no MAP slot", ErrNotFound, barcode)
		}
		for _, sl := range inv.Slots {
			if !sl.Full {
				mv = Move{
					From:    Location{Kind: "map", Number: src.Number},
					To:      Location{Kind: "slot", Number: sl.Number},
					Barcode: barcode,
				}
				return s.move(src.Address, sl.Address)
			}
		}
		return ErrNoEmptySlot
	})
	return mv, err
}

// find returns where barcode is: a storage slot, MAP slot or drive (by
// position), and that element's address.
func (inv Inventory) find(barcode string) (Location, uint16, bool) {
	if barcode == "" {
		return Location{}, 0, false
	}
	for _, sl := range inv.Slots {
		if sl.Full && sl.Barcode == barcode {
			return Location{Kind: "slot", Number: sl.Number}, sl.Address, true
		}
	}
	for _, sl := range inv.MAPs {
		if sl.Full && sl.Barcode == barcode {
			return Location{Kind: "map", Number: sl.Number}, sl.Address, true
		}
	}
	for _, d := range inv.Drives {
		if d.Full && d.Barcode == barcode {
			return Location{Kind: "drive", Number: d.Number}, d.Address, true
		}
	}
	return Location{}, 0, false
}

// Locate returns where barcode is in the inventory: a storage slot, MAP slot
// or drive position.
func (inv Inventory) Locate(barcode string) (Location, bool) {
	loc, _, ok := inv.find(barcode)
	return loc, ok
}

// driveOrdinal maps a drive id (the vtltape queue id) to its 1-based data
// transfer element position: the "Slot" of "Library ID: <lib> Slot: <n>".
func driveOrdinal(lib config.Library, driveID int) (int, error) {
	for _, d := range lib.Drives {
		if d.ID == driveID {
			if d.Slot <= 0 {
				return 0, fmt.Errorf("%w: drive %d has no Slot in device.conf", ErrOutOfRange, driveID)
			}
			return d.Slot, nil
		}
	}
	return 0, fmt.Errorf("%w: drive %d, library %d", ErrUnknownDrive, driveID, lib.ID)
}

// session is one opened changer with its element map.
type session struct {
	dev  scsi.Device
	emap scsi.ElementMap
}

// run issues cdb, reissuing it after UNIT ATTENTION.
func run(dev scsi.Device, cdb, buf []byte, timeout time.Duration) (int, error) {
	for i := 0; ; i++ {
		n, _, err := dev.Do(cdb, buf, timeout)
		var se *scsi.SenseError
		if i < unitAttentionRetries && errors.As(err, &se) && se.Key == scsi.KeyUnitAttention {
			continue
		}
		return n, err
	}
}

// elementMap reads mode page 1Dh, with MODE SENSE(6) and, if the changer
// rejects that, MODE SENSE(10).
func (s *session) elementMap() (scsi.ElementMap, error) {
	buf := make([]byte, 255)
	n, err := run(s.dev, scsi.ModeSense6CDB(scsi.PageElementAddress, uint8(len(buf))), buf, queryTimeout)
	var se *scsi.SenseError
	if errors.As(err, &se) && se.Key == scsi.KeyIllegalRequest {
		buf = make([]byte, 1024)
		if n, err = run(s.dev, scsi.ModeSense10CDB(scsi.PageElementAddress, uint16(len(buf))), buf, queryTimeout); err != nil {
			return scsi.ElementMap{}, err
		}
		return scsi.ParseElementAddressPage(buf[:n], true)
	}
	if err != nil {
		return scsi.ElementMap{}, err
	}
	return scsi.ParseElementAddressPage(buf[:n], false)
}

// elements reads the status of every element of typ in rng, with barcodes.
func (s *session) elements(typ uint8, rng scsi.ElementRange) ([]scsi.Element, error) {
	if rng.Count == 0 {
		return nil, nil
	}
	alloc := elementStatusAlloc
	for {
		buf := make([]byte, alloc)
		cdb := scsi.ReadElementStatusCDB(typ, true, rng.First, rng.Count, uint32(alloc))
		n, err := run(s.dev, cdb, buf, queryTimeout)
		if err != nil {
			return nil, err
		}
		total, err := scsi.ElementStatusTotal(buf[:n])
		if err != nil {
			return nil, err
		}
		if total > alloc && alloc < maxAlloc {
			alloc = min(total, maxAlloc)
			continue
		}
		return scsi.ParseReadElementStatus(buf[:n])
	}
}

func (s *session) inventory() (Inventory, error) {
	inv := Inventory{
		Drives: make([]DriveState, s.emap.DTE.Count),
		Slots:  slotStates(s.emap.Storage),
		MAPs:   slotStates(s.emap.IE),
	}
	for i := range inv.Drives {
		inv.Drives[i] = DriveState{Number: i + 1, Address: s.emap.DTE.First + uint16(i)}
	}
	for _, typ := range []uint8{scsi.ElementDTE, scsi.ElementStorage, scsi.ElementIE} {
		els, err := s.elements(typ, s.rangeOf(typ))
		if err != nil {
			return Inventory{}, err
		}
		for _, e := range els {
			kind, n := s.emap.Locate(e.Address)
			switch {
			case kind != typ || n == 0:
				continue
			case kind == scsi.ElementDTE:
				d := &inv.Drives[n-1]
				d.Full, d.Barcode, d.Except = e.Full, e.Barcode, e.Except
				if e.Full && e.SourceValid {
					switch skind, sn := s.emap.Locate(e.Source); skind {
					case scsi.ElementStorage:
						d.SourceSlot = sn
					case scsi.ElementIE:
						d.SourceMAP = sn
					}
				}
			case kind == scsi.ElementStorage:
				inv.Slots[n-1].Full, inv.Slots[n-1].Barcode = e.Full, e.Barcode
			default:
				inv.MAPs[n-1].Full, inv.MAPs[n-1].Barcode = e.Full, e.Barcode
				if inv.MAPOpen == nil {
					open := !e.Access
					inv.MAPOpen = &open
				}
			}
		}
	}
	return inv, nil
}

func slotStates(rng scsi.ElementRange) []SlotState {
	out := make([]SlotState, rng.Count)
	for i := range out {
		out[i] = SlotState{Number: i + 1, Address: rng.First + uint16(i)}
	}
	return out
}

func (s *session) rangeOf(typ uint8) scsi.ElementRange {
	switch typ {
	case scsi.ElementDTE:
		return s.emap.DTE
	case scsi.ElementStorage:
		return s.emap.Storage
	default:
		return s.emap.IE
	}
}

// defaultDestination picks where an unmount returns the cartridge in the
// drive at addr: its valid source element when that is an empty storage or
// MAP slot, else the first empty storage slot.
func (s *session) defaultDestination(addr uint16) (uint16, Location, string, error) {
	els, err := s.elements(scsi.ElementDTE, scsi.ElementRange{First: addr, Count: 1})
	if err != nil {
		return 0, Location{}, "", err
	}
	if len(els) == 0 || els[0].Address != addr || !els[0].Full {
		return 0, Location{}, "", ErrDriveEmpty
	}
	drive := els[0]
	if drive.SourceValid {
		kind, n := s.emap.Locate(drive.Source)
		if kind == scsi.ElementStorage || kind == scsi.ElementIE {
			src, err := s.elements(kind, scsi.ElementRange{First: drive.Source, Count: 1})
			if err != nil {
				return 0, Location{}, "", err
			}
			if len(src) == 1 && src[0].Address == drive.Source && !src[0].Full {
				loc := Location{Kind: "slot", Number: n}
				if kind == scsi.ElementIE {
					loc.Kind = "map"
				}
				return drive.Source, loc, drive.Barcode, nil
			}
		}
	}
	slots, err := s.elements(scsi.ElementStorage, s.emap.Storage)
	if err != nil {
		return 0, Location{}, "", err
	}
	for _, e := range slots {
		if kind, n := s.emap.Locate(e.Address); kind == scsi.ElementStorage && !e.Full {
			return e.Address, Location{Kind: "slot", Number: n}, drive.Barcode, nil
		}
	}
	return 0, Location{}, "", ErrNoEmptySlot
}

// slotAddress resolves a storage ("slot") or MAP ("map") location to its
// element address.
func (s *session) slotAddress(loc Location) (uint16, error) {
	var rng scsi.ElementRange
	var what string
	switch loc.Kind {
	case "slot":
		rng, what = s.emap.Storage, "slot"
	case "map":
		rng, what = s.emap.IE, "MAP slot"
	default:
		return 0, fmt.Errorf("%w: %q (want slot or map)", ErrBadLocation, loc.Kind)
	}
	addr, ok := rng.Address(loc.Number)
	if !ok {
		return 0, fmt.Errorf("%w: %s %d (changer has %d)", ErrOutOfRange, what, loc.Number, rng.Count)
	}
	return addr, nil
}

// requireMAPClosed fails with ErrMAPOpen when the MAP is open. mhVTL refuses
// such a move with NOT READY; checking first gives the operator a reason.
func (s *session) requireMAPClosed() error {
	if s.emap.IE.Count == 0 {
		return nil
	}
	els, err := s.elements(scsi.ElementIE, scsi.ElementRange{First: s.emap.IE.First, Count: 1})
	if err != nil {
		return err
	}
	if len(els) == 1 && !els[0].Access {
		return ErrMAPOpen
	}
	return nil
}

// move issues MOVE MEDIUM with the first medium transport element.
func (s *session) move(src, dst uint16) error {
	_, err := run(s.dev, scsi.MoveMediumCDB(s.emap.Transport.First, src, dst), nil, moveTimeout)
	return err
}
