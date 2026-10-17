package apply

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// fakeSG is an sg node: it answers INQUIRY with its peripheral type and
// TEST UNIT READY with the errors queued in tur, and records every CDB.
type fakeSG struct {
	path   string
	typ    uint8
	tur    []error
	cdbs   *[]byte
	closed *bool
}

func (f *fakeSG) Do(cdb, dataIn []byte, _ time.Duration) (int, []byte, error) {
	*f.cdbs = append(*f.cdbs, cdb[0])
	switch cdb[0] {
	case 0x12:
		dataIn[0] = f.typ
		return len(dataIn), nil, nil
	case 0x00:
		if len(f.tur) == 0 {
			return 0, nil, nil
		}
		err := f.tur[0]
		f.tur = f.tur[1:]
		return 0, nil, err
	}
	return 0, nil, nil
}

func (f *fakeSG) Close() error { *f.closed = true; return nil }

// sgTree is a fake sysfs: hctl -> (sg node name, sysfs type, serial).
type sgTree struct {
	sys, dev string
	opened   []string
	cdbs     []byte
	closed   bool
	inqType  map[string]uint8 // INQUIRY type per node, when it differs
	tur      []error
	onOpen   func(path string)
}

func newSGTree(t *testing.T) *sgTree {
	root := t.TempDir()
	return &sgTree{sys: filepath.Join(root, "sys"), dev: "/dev", inqType: map[string]uint8{}}
}

// add puts a device at hctl with sg node name, sysfs type typ and unit
// serial number serial. Adding the same hctl again moves it to another
// node, as a restart does.
func (s *sgTree) add(t *testing.T, hctl, name, typ, serial string) {
	t.Helper()
	dev := filepath.Join(s.sys, "bus", "scsi", "devices", hctl)
	os.RemoveAll(filepath.Join(dev, "scsi_generic"))
	for _, d := range []string{filepath.Join(dev, "scsi_generic", name), filepath.Join(s.sys, "class", "scsi_generic")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dev, "type"), []byte(typ+"\n"), 0o644)
	page := append([]byte{0, 0x80, 0, byte(len(serial))}, serial...)
	os.WriteFile(filepath.Join(dev, "vpd_pg80"), page, 0o644)
	link := filepath.Join(s.sys, "class", "scsi_generic", name)
	os.MkdirAll(link, 0o755)
	os.Remove(filepath.Join(link, "device"))
	if err := os.Symlink(dev, filepath.Join(link, "device")); err != nil {
		t.Fatal(err)
	}
}

func (s *sgTree) access() SCSIAccess {
	return SCSIAccess{SysRoot: s.sys, DevRoot: s.dev, Open: func(path string) (changer.Device, error) {
		s.opened = append(s.opened, path)
		if s.onOpen != nil {
			s.onOpen(path)
		}
		typ := uint8(scsi.PeripheralMediumChanger)
		if b, err := os.ReadFile(filepath.Join(s.sys, "class", "scsi_generic", filepath.Base(path), "device", "type")); err == nil && string(b) == "1\n" {
			typ = scsi.PeripheralSequential
		}
		if t, ok := s.inqType[filepath.Base(path)]; ok {
			typ = t
		}
		return &fakeSG{path: path, typ: typ, tur: s.tur, cdbs: &s.cdbs, closed: &s.closed}, nil
	}}
}

func TestGuardOpcodes(t *testing.T) {
	var cdbs []byte
	var closed bool
	g := guardedDevice{&fakeSG{cdbs: &cdbs, closed: &closed}}
	for _, op := range []byte{0x2a, 0x0a, 0xa5, 0x1b, 0x01, 0x15, 0x55, 0x3b, 0x04} {
		// Refused whatever the length.
		if _, _, err := g.Do([]byte{op, 0, 0, 0, 0, 0}, nil, time.Second); !errors.Is(err, errOpcode) {
			t.Errorf("%02xh: %v", op, err)
		}
	}
	if _, _, err := g.Do(nil, nil, time.Second); !errors.Is(err, errOpcode) {
		t.Errorf("empty CDB: %v", err)
	}
	if len(cdbs) != 0 {
		t.Errorf("refused CDBs reached the device: % x", cdbs)
	}
	for op, a := range allowedOpcodes {
		cdb := make([]byte, a.len+2)
		cdb[0] = op
		if _, _, err := g.Do(cdb, nil, time.Second); !errors.Is(err, errOpcode) {
			t.Errorf("%s of %d bytes: %v", a.name, len(cdb), err)
		}
		cdb = make([]byte, a.len)
		cdb[0] = op
		if _, _, err := g.Do(cdb, make([]byte, 8), time.Second); err != nil {
			t.Errorf("%02xh: %v", op, err)
		}
	}
	if len(cdbs) != len(allowedOpcodes) {
		t.Errorf("allowed CDBs sent: % x", cdbs)
	}
}

func TestGuardOpenTypes(t *testing.T) {
	s := newSGTree(t)
	s.add(t, "3:0:0:0", "sg1", "8", "LIB10")
	s.add(t, "3:0:1:0", "sg2", "1", "DRV11")
	s.add(t, "3:0:9:0", "sg9", "0", "DISK")
	a := s.access()

	// A disk is refused before it is opened.
	if _, err := a.open("3:0:9:0", scsi.PeripheralSequential); err == nil {
		t.Error("disk opened")
	}
	// A changer asked for as a tape drive, and the reverse.
	if _, err := a.open("3:0:0:0", scsi.PeripheralSequential); err == nil {
		t.Error("changer opened as a tape drive")
	}
	if _, err := a.open("3:0:1:0", scsi.PeripheralMediumChanger); err == nil {
		t.Error("tape drive opened as a changer")
	}
	if len(s.opened) != 0 {
		t.Errorf("opened %v", s.opened)
	}
	// INQUIRY disagreeing with sysfs: opened, checked, closed, refused.
	s.inqType["sg1"] = 0
	if _, err := a.open("3:0:0:0", scsi.PeripheralMediumChanger); err == nil || !s.closed {
		t.Errorf("INQUIRY type mismatch: %v, closed %v", err, s.closed)
	}
	delete(s.inqType, "sg1")
	d, err := a.open("3:0:0:0", scsi.PeripheralMediumChanger)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Do(scsi.MoveMediumCDB(0, 1, 2), nil, time.Second); !errors.Is(err, errOpcode) {
		t.Errorf("MOVE MEDIUM through an opened changer: %v", err)
	}
	for _, bad := range []string{"../3:0:0:0", "3:0:0", "sg1", ""} {
		if _, err := a.open(bad, scsi.PeripheralMediumChanger); err == nil {
			t.Errorf("%q opened", bad)
		}
	}
}

// TestGuardReresolve: a device is found by H:C:T:L on every open, so after
// a restart moves it to another sg node the new node is opened, never the
// old path (amendment 5).
func TestGuardReresolve(t *testing.T) {
	s := newSGTree(t)
	s.add(t, "3:0:1:0", "sg2", "1", "DRV11")
	a := s.access()
	hctl, err := a.Locate("DRV11", scsi.PeripheralSequential)
	if err != nil || hctl != "3:0:1:0" {
		t.Fatalf("Locate: %q, %v", hctl, err)
	}
	os.RemoveAll(filepath.Join(s.sys, "class", "scsi_generic", "sg2"))
	s.add(t, "3:0:1:0", "sg5", "1", "DRV11")
	if _, err := a.Loaded(hctl); err != nil {
		t.Fatal(err)
	}
	if len(s.opened) != 1 || s.opened[0] != "/dev/sg5" {
		t.Errorf("opened %v, want /dev/sg5", s.opened)
	}
	if _, err := a.Locate("NOSUCH", scsi.PeripheralSequential); err == nil {
		t.Error("unknown serial located")
	}
}

func TestLoaded(t *testing.T) {
	notReady := &scsi.SenseError{Key: 0x02, ASC: 0x3a}
	reset := &scsi.SenseError{Key: 0x06, ASC: 0x29}
	for _, tc := range []struct {
		name   string
		tur    []error
		loaded bool
		fail   bool
	}{
		{"good", nil, true, false},
		{"no medium", []error{notReady}, false, false},
		{"unit attention, then good", []error{reset}, true, false},
		{"unit attention, then no medium", []error{reset, notReady}, false, false},
		{"other sense", []error{&scsi.SenseError{Key: 0x04}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSGTree(t)
			s.add(t, "3:0:1:0", "sg2", "1", "DRV11")
			s.tur = tc.tur
			loaded, err := s.access().Loaded("3:0:1:0")
			if loaded != tc.loaded || (err != nil) != tc.fail {
				t.Errorf("loaded %v, %v", loaded, err)
			}
		})
	}
}

// TestInventoryThroughGuard: READ ELEMENT STATUS goes to the node the
// H:C:T:L resolves to now, through the guard. The fake answers nothing
// useful, so the read fails; what matters is where and what it sent.
func TestInventoryThroughGuard(t *testing.T) {
	s := newSGTree(t)
	s.add(t, "3:0:0:0", "sg4", "8", "LIB10")
	s.access().Inventory(config.Library{ID: 10, Serial: "LIB10"}, "3:0:0:0")
	if len(s.opened) == 0 {
		t.Fatal("nothing opened")
	}
	for _, p := range s.opened {
		if p != "/dev/sg4" {
			t.Errorf("opened %s", p)
		}
	}
	for _, op := range s.cdbs {
		if _, ok := allowedOpcodes[op]; !ok {
			t.Errorf("sent %02xh", op)
		}
	}
}

// TestGuardRenumberedAtOpen: a node renumbered to another device between
// the resolve and the open is closed and refused.
func TestGuardRenumberedAtOpen(t *testing.T) {
	s := newSGTree(t)
	s.add(t, "3:0:0:0", "sg1", "8", "LIB10")
	s.add(t, "3:0:5:0", "sg5", "8", "LIB30")
	s.onOpen = func(string) {
		link := filepath.Join(s.sys, "class", "scsi_generic", "sg1", "device")
		os.Remove(link)
		os.Symlink(filepath.Join(s.sys, "bus", "scsi", "devices", "3:0:5:0"), link)
	}
	if _, err := s.access().open("3:0:0:0", scsi.PeripheralMediumChanger); err == nil {
		t.Fatal("opened a renumbered node")
	}
	if !s.closed || len(s.cdbs) != 0 {
		t.Errorf("closed %v, CDBs sent % x", s.closed, s.cdbs)
	}
}
