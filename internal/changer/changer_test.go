package changer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// sim is a medium changer simulated at the CDB level, answering the commands
// Manager issues the way mhvtl's smc.c does.
type sim struct {
	mu     sync.Mutex
	emap   scsi.ElementMap
	full   map[uint16]string // element address -> barcode of the cartridge in it
	source map[uint16]uint16 // address -> element the cartridge was moved from
	// uaLeft makes the next commands fail with UNIT ATTENTION.
	uaLeft int
	// noModeSense6 rejects MODE SENSE(6) with ILLEGAL REQUEST.
	noModeSense6 bool
	// noTransportStatus rejects READ ELEMENT STATUS of medium transport
	// elements with ILLEGAL REQUEST.
	noTransportStatus bool
	// mapOpen models an open MAP: its slots lose the access bit and moves
	// into or out of them fail NOT READY, as in mhvtl's smc.c.
	mapOpen bool
	// busy detects overlapping commands (serialization check).
	busy     int32
	overlaps int32
	delay    time.Duration
}

// ibmMap is an IBM 3584-like element map: none of the ranges start at 1.
var ibmMap = scsi.ElementMap{
	Transport: scsi.ElementRange{First: 0x0001, Count: 1},
	Storage:   scsi.ElementRange{First: 0x0401, Count: 20},
	IE:        scsi.ElementRange{First: 0x0301, Count: 4},
	DTE:       scsi.ElementRange{First: 0x0101, Count: 4},
}

func newSim(emap scsi.ElementMap) *sim {
	return &sim{emap: emap, full: map[uint16]string{}, source: map[uint16]uint16{}}
}

func illegal(asc, ascq uint8) error {
	return scsi.DecodeSense(scsi.FixedSense(scsi.KeyIllegalRequest, asc, ascq))
}

func (s *sim) device() *scsi.FakeDevice { return &scsi.FakeDevice{Respond: s.respond} }

func (s *sim) respond(cdb, buf []byte) (int, error) {
	if atomic.AddInt32(&s.busy, 1) > 1 {
		atomic.AddInt32(&s.overlaps, 1)
	}
	defer atomic.AddInt32(&s.busy, -1)
	time.Sleep(s.delay)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uaLeft > 0 {
		s.uaLeft--
		return 0, scsi.DecodeSense(scsi.FixedSense(scsi.KeyUnitAttention, 0x29, 0x00))
	}
	switch cdb[0] {
	case 0x1a:
		if s.noModeSense6 {
			return 0, illegal(0x20, 0x00)
		}
		return copy(buf, append([]byte{23, 0, 0, 0}, s.page1D()...)), nil
	case 0x5a:
		return copy(buf, append([]byte{0, 26, 0, 0, 0, 0, 0, 0}, s.page1D()...)), nil
	case 0xb8:
		if s.noTransportStatus && cdb[1]&0x0f == scsi.ElementTransport {
			return 0, illegal(0x24, 0x00)
		}
		return s.readElementStatus(cdb, buf), nil
	case 0xa5:
		return 0, s.moveMedium(cdb)
	}
	return 0, illegal(0x20, 0x00)
}

func (s *sim) page1D() []byte {
	p := []byte{0x1d, 18}
	for _, r := range []scsi.ElementRange{s.emap.Transport, s.emap.Storage, s.emap.IE, s.emap.DTE} {
		p = binary.BigEndian.AppendUint16(p, r.First)
		p = binary.BigEndian.AppendUint16(p, r.Count)
	}
	return append(p, 0, 0) // reserved bytes 18-19
}

func (s *sim) rangeOf(typ uint8) scsi.ElementRange {
	switch typ {
	case scsi.ElementDTE:
		return s.emap.DTE
	case scsi.ElementStorage:
		return s.emap.Storage
	case scsi.ElementIE:
		return s.emap.IE
	}
	return s.emap.Transport
}

// readElementStatus builds one element status page of the CDB's type with
// voltag descriptors (DTEs carry a 34-byte identifier like STK's dvcid
// layout), truncated to the allocation length as smc.c does.
func (s *sim) readElementStatus(cdb, buf []byte) int {
	typ := cdb[1] & 0x0f
	start := binary.BigEndian.Uint16(cdb[2:4])
	count := int(binary.BigEndian.Uint16(cdb[4:6]))
	alloc := int(cdb[7])<<16 | int(cdb[8])<<8 | int(cdb[9])
	rng := s.rangeOf(typ)
	dlen := 16 + 36
	if typ == scsi.ElementDTE {
		dlen += 34
	}
	var descs []byte
	n := 0
	for a := max(start, rng.First); rng.Contains(a) && n < count; a++ {
		d := make([]byte, dlen)
		binary.BigEndian.PutUint16(d[0:2], a)
		if typ != scsi.ElementIE || !s.mapOpen {
			d[2] |= 0x08 // access
		}
		if bc, ok := s.full[a]; ok {
			d[2] |= 0x01
			copy(d[12:44], bc+strings.Repeat(" ", 32-len(bc)))
			if src, ok := s.source[a]; ok {
				d[9] = 0x80
				binary.BigEndian.PutUint16(d[10:12], src)
			}
		}
		descs = append(descs, d...)
		n++
	}
	ph := []byte{typ, 0x80, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(ph[2:4], uint16(dlen))
	ph[5], ph[6], ph[7] = byte(len(descs)>>16), byte(len(descs)>>8), byte(len(descs))
	body := append(ph, descs...)
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint16(hdr[0:2], start)
	binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
	hdr[5], hdr[6], hdr[7] = byte(len(body)>>16), byte(len(body)>>8), byte(len(body))
	out := append(hdr, body...)
	return copy(buf[:min(alloc, len(buf))], out)
}

func (s *sim) valid(a uint16) bool {
	return s.emap.Storage.Contains(a) || s.emap.IE.Contains(a) || s.emap.DTE.Contains(a)
}

func (s *sim) moveMedium(cdb []byte) error {
	tr := binary.BigEndian.Uint16(cdb[2:4])
	src := binary.BigEndian.Uint16(cdb[4:6])
	dst := binary.BigEndian.Uint16(cdb[6:8])
	if !s.emap.Transport.Contains(tr) || !s.valid(src) || !s.valid(dst) {
		return illegal(0x21, 0x01)
	}
	if s.mapOpen && (s.emap.IE.Contains(src) || s.emap.IE.Contains(dst)) {
		return scsi.DecodeSense(scsi.FixedSense(scsi.KeyNotReady, 0x3a, 0x02))
	}
	bc, ok := s.full[src]
	if !ok {
		return illegal(0x3b, 0x0e)
	}
	if _, ok := s.full[dst]; ok {
		return illegal(0x3b, 0x0d)
	}
	delete(s.full, src)
	delete(s.source, src)
	s.full[dst] = bc
	s.source[dst] = src
	return nil
}

// testLib is library 10 with drive ids 11..14 at DTE positions 1..4, except
// drive 13 which sits at position 4 and 14 at position 3 (ids and positions
// are independent).
func testLib() config.Library {
	return config.Library{
		ID:     10,
		Serial: "XYZZY_A",
		Drives: []config.Drive{
			{ID: 11, LibraryID: 10, Slot: 1},
			{ID: 12, LibraryID: 10, Slot: 2},
			{ID: 13, LibraryID: 10, Slot: 4},
			{ID: 14, LibraryID: 10, Slot: 3},
			{ID: 15, LibraryID: 10},
		},
	}
}

// managerFor returns a Manager whose library 10 resolves to a fake node
// served by s.
func managerFor(s *sim) (*Manager, *scsi.FakeDevice) {
	dev := s.device()
	loc := &Locator{Overrides: map[int]string{10: "/dev/sg9"}}
	return NewManager(loc, func(string) (Device, error) { return dev, nil }), dev
}

// moves returns the MOVE MEDIUM CDBs the fake device received.
func moves(dev *scsi.FakeDevice) [][]byte {
	var out [][]byte
	for _, c := range dev.Recorded() {
		if c[0] == 0xa5 {
			out = append(out, c)
		}
	}
	return out
}

func TestMountFromSlotAddressMath(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0403] = "A00003L8"
	m, dev := managerFor(s)

	mv, err := m.Mount(testLib(), 13, Source{Slot: 3})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := scsi.MoveMediumCDB(0x0001, 0x0403, 0x0104) // slot 3 -> drive position 4
	if got := moves(dev); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("MOVE MEDIUM = % x, want % x", got, want)
	}
	if mv.From != (Location{"slot", 3}) || mv.To != (Location{"drive", 4}) {
		t.Errorf("move = %+v", mv)
	}
	if !dev.Closed {
		t.Error("device not closed after the operation")
	}
}

func TestMountFromMAP(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0302] = "IMPORT01"
	m, dev := managerFor(s)

	if _, err := m.Mount(testLib(), 11, Source{MAP: 2}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	want := scsi.MoveMediumCDB(0x0001, 0x0302, 0x0101)
	if got := moves(dev); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("MOVE MEDIUM = % x, want % x", got, want)
	}
}

func TestMountValidation(t *testing.T) {
	cases := []struct {
		name  string
		drive int
		from  Source
		want  error
	}{
		{"unknown drive", 99, Source{Slot: 1}, ErrUnknownDrive},
		{"drive without slot", 15, Source{Slot: 1}, ErrOutOfRange},
		{"slot beyond map", 11, Source{Slot: 21}, ErrOutOfRange},
		{"MAP beyond map", 11, Source{MAP: 5}, ErrOutOfRange},
	}
	for _, c := range cases {
		s := newSim(ibmMap)
		m, dev := managerFor(s)
		if _, err := m.Mount(testLib(), c.drive, c.from); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if len(moves(dev)) != 0 {
			t.Errorf("%s: MOVE MEDIUM issued for an invalid request", c.name)
		}
	}

	// A drive position the changer does not have.
	small := ibmMap
	small.DTE.Count = 2
	m, _ := managerFor(newSim(small))
	if _, err := m.Mount(testLib(), 13, Source{Slot: 1}); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("drive position 4 of 2: err = %v, want ErrOutOfRange", err)
	}
}

func TestMountSenseIsReturned(t *testing.T) {
	m, _ := managerFor(newSim(ibmMap))
	_, err := m.Mount(testLib(), 11, Source{Slot: 1})
	var se *scsi.SenseError
	if !errors.As(err, &se) || se.Code() != scsi.ASCMediumSrcEmpty {
		t.Fatalf("mount from empty slot: err = %v, want sense 3B/0E", err)
	}
}

func TestUnmountDefaultsToSourceSlot(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0102] = "A00007L8"
	s.source[0x0102] = 0x0407
	m, dev := managerFor(s)

	mv, err := m.Unmount(testLib(), 12, 0)
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	want := scsi.MoveMediumCDB(0x0001, 0x0102, 0x0407)
	if got := moves(dev); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("MOVE MEDIUM = % x, want % x", got, want)
	}
	if mv.To != (Location{"slot", 7}) || mv.Barcode != "A00007L8" {
		t.Errorf("move = %+v", mv)
	}
}

func TestUnmountReturnsToSourceMAP(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0101] = "IMPORT01"
	s.source[0x0101] = 0x0303
	m, _ := managerFor(s)

	mv, err := m.Unmount(testLib(), 11, 0)
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if mv.To != (Location{"map", 3}) || s.full[0x0303] != "IMPORT01" {
		t.Errorf("move = %+v, MAP 3 holds %q", mv, s.full[0x0303])
	}
}

func TestUnmountFallsBackToFirstEmptySlot(t *testing.T) {
	// Source slot 1 has since been refilled; slot 2 is full; slot 3 is free.
	s := newSim(ibmMap)
	s.full[0x0101] = "A00001L8"
	s.source[0x0101] = 0x0401
	s.full[0x0401] = "OTHER1"
	s.full[0x0402] = "OTHER2"
	m, _ := managerFor(s)

	mv, err := m.Unmount(testLib(), 11, 0)
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if mv.To != (Location{"slot", 3}) || s.full[0x0403] != "A00001L8" {
		t.Errorf("move = %+v, slot 3 holds %q", mv, s.full[0x0403])
	}

	// No valid source element at all.
	s = newSim(ibmMap)
	s.full[0x0101] = "A00001L8"
	m, _ = managerFor(s)
	if mv, err = m.Unmount(testLib(), 11, 0); err != nil || mv.To != (Location{"slot", 1}) {
		t.Errorf("no SValid: move = %+v, %v; want slot 1", mv, err)
	}
}

func TestUnmountErrors(t *testing.T) {
	// Drive empty.
	m, dev := managerFor(newSim(ibmMap))
	if _, err := m.Unmount(testLib(), 11, 0); !errors.Is(err, ErrDriveEmpty) {
		t.Errorf("empty drive: err = %v, want ErrDriveEmpty", err)
	}
	if len(moves(dev)) != 0 {
		t.Error("MOVE MEDIUM issued for an empty drive")
	}

	// Every storage slot full.
	s := newSim(ibmMap)
	s.full[0x0101] = "A00001L8"
	for i := uint16(0); i < ibmMap.Storage.Count; i++ {
		s.full[ibmMap.Storage.First+i] = "FULL"
	}
	m, _ = managerFor(s)
	if _, err := m.Unmount(testLib(), 11, 0); !errors.Is(err, ErrNoEmptySlot) {
		t.Errorf("library full: err = %v, want ErrNoEmptySlot", err)
	}

	// Explicit destination is honoured and range-checked.
	s = newSim(ibmMap)
	s.full[0x0101] = "A00001L8"
	m, dev = managerFor(s)
	if _, err := m.Unmount(testLib(), 11, 21); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("slot 21: err = %v, want ErrOutOfRange", err)
	}
	if mv, err := m.Unmount(testLib(), 11, 9); err != nil || mv.To != (Location{"slot", 9}) {
		t.Errorf("explicit slot 9: move = %+v, %v", mv, err)
	}
	want := scsi.MoveMediumCDB(0x0001, 0x0101, 0x0409)
	if got := moves(dev); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Errorf("MOVE MEDIUM = % x, want % x", got, want)
	}
}

func TestInventory(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0401] = "A00001L8"
	s.full[0x0102] = "A00005L8"
	s.source[0x0102] = 0x0405
	s.full[0x0104] = "IMPORT01"
	s.source[0x0104] = 0x0302
	s.full[0x0301] = "IMPORT02"
	s.full[0x0001] = "PICKED1L"
	s.uaLeft = 1 // first command after a "reset"
	m, _ := managerFor(s)

	inv, err := m.Inventory(testLib())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Pickers) != 1 || inv.Pickers[0] != (SlotState{Number: 1, Address: 0x0001, Full: true, Barcode: "PICKED1L"}) {
		t.Errorf("pickers = %+v", inv.Pickers)
	}
	if loc, ok := inv.Locate("PICKED1L"); !ok || loc != (Location{Kind: "picker", Number: 1}) {
		t.Errorf("Locate(PICKED1L) = %+v, %v", loc, ok)
	}
	if len(inv.Drives) != 4 || len(inv.Slots) != 20 || len(inv.MAPs) != 4 {
		t.Fatalf("sizes = %d/%d/%d", len(inv.Drives), len(inv.Slots), len(inv.MAPs))
	}
	if d := inv.Drives[1]; !d.Full || d.Barcode != "A00005L8" || d.SourceSlot != 5 || d.Number != 2 || d.Address != 0x0102 {
		t.Errorf("drive 2 = %+v", d)
	}
	if d := inv.Drives[3]; d.SourceMAP != 2 || d.SourceSlot != 0 {
		t.Errorf("drive 4 = %+v", d)
	}
	if d := inv.Drives[0]; d.Full || d.Barcode != "" {
		t.Errorf("drive 1 = %+v", d)
	}
	if sl := inv.Slots[0]; !sl.Full || sl.Barcode != "A00001L8" || sl.Number != 1 {
		t.Errorf("slot 1 = %+v", sl)
	}
	if sl := inv.Slots[1]; sl.Full || sl.Number != 2 || sl.Address != 0x0402 {
		t.Errorf("slot 2 = %+v", sl)
	}
	if mp := inv.MAPs[0]; !mp.Full || mp.Barcode != "IMPORT02" {
		t.Errorf("MAP 1 = %+v", mp)
	}
}

func TestModeSense10Fallback(t *testing.T) {
	s := newSim(ibmMap)
	s.noModeSense6 = true
	m, dev := managerFor(s)
	if _, err := m.Inventory(testLib()); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	cdbs := dev.Recorded()
	if cdbs[0][0] != 0x1a || cdbs[1][0] != 0x5a {
		t.Errorf("first CDBs = %x %x, want MODE SENSE(6) then (10)", cdbs[0][0], cdbs[1][0])
	}
}

func TestLargeElementReportIsReread(t *testing.T) {
	big := ibmMap
	big.Storage = scsi.ElementRange{First: 0x1000, Count: 1500} // 1500*52 bytes > 64 KiB
	s := newSim(big)
	s.full[0x1000+1499] = "LAST01L8"
	m, dev := managerFor(s)

	inv, err := m.Inventory(testLib())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if sl := inv.Slots[1499]; !sl.Full || sl.Barcode != "LAST01L8" {
		t.Errorf("slot 1500 = %+v", sl)
	}
	var storageReads int
	for _, c := range dev.Recorded() {
		if c[0] == 0xb8 && c[1]&0x0f == scsi.ElementStorage {
			storageReads++
		}
	}
	if storageReads != 2 {
		t.Errorf("storage READ ELEMENT STATUS issued %d times, want 2 (initial + exact size)", storageReads)
	}
}

func TestOperationsAreSerialized(t *testing.T) {
	s := newSim(ibmMap)
	s.delay = time.Millisecond
	m, _ := managerFor(s)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Inventory(testLib())
		}()
	}
	wg.Wait()
	if s.overlaps != 0 {
		t.Errorf("%d commands overlapped on one changer", s.overlaps)
	}
}

func TestManagerUnavailable(t *testing.T) {
	loc := &Locator{SysRoot: t.TempDir(), DevRoot: "/dev"}
	m := NewManager(loc, func(string) (Device, error) { return nil, errors.New("unused") })
	if _, err := m.Inventory(testLib()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no sysfs class dir: err = %v, want ErrUnavailable", err)
	}

	loc = &Locator{Overrides: map[int]string{10: "/dev/sg9"}}
	m = NewManager(loc, func(string) (Device, error) { return nil, os.ErrPermission })
	if _, err := m.Mount(testLib(), 11, Source{Slot: 1}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("open failure: err = %v, want ErrUnavailable", err)
	}
}

// fakeSysfs builds <root>/class/scsi_generic/<name>/device/{type,vpd_pg80}.
// An empty typ or serial omits that file.
func fakeSysfs(t *testing.T, root, name, typ, serial string) {
	t.Helper()
	dir := filepath.Join(root, "class", "scsi_generic", name, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if typ != "" {
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if serial != "" {
		if err := os.WriteFile(filepath.Join(dir, "vpd_pg80"), vpd80(serial), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func vpd80(serial string) []byte {
	return append([]byte{0x08, 0x80, 0, byte(len(serial))}, serial...)
}

func TestLocatorFind(t *testing.T) {
	root := t.TempDir()
	fakeSysfs(t, root, "sg0", "1", "XYZZY_A1") // tape drive of library A
	fakeSysfs(t, root, "sg1", "8", "XYZZY_B")  // library B, serial in sysfs
	fakeSysfs(t, root, "sg2", "8", "")         // library A, no vpd_pg80: INQUIRY fallback
	fakeSysfs(t, root, "sg3", "", "")          // no type file: INQUIRY says tape (01h)

	opened := map[string]int{}
	open := func(path string) (Device, error) {
		opened[path]++
		return &scsi.FakeDevice{Respond: func(cdb, buf []byte) (int, error) {
			switch {
			case path == "/dev/sg2" && cdb[1]&1 == 1 && cdb[2] == 0x80:
				return copy(buf, vpd80("XYZZY_A  ")), nil
			case path == "/dev/sg3" && cdb[1]&1 == 0:
				inq := make([]byte, 36)
				inq[0] = 0x01
				return copy(buf, inq), nil
			}
			return 0, illegal(0x24, 0x00)
		}}, nil
	}
	loc := &Locator{SysRoot: root, DevRoot: "/dev", Open: open}

	if p, err := loc.Find(config.Library{ID: 10, Serial: "XYZZY_A"}); err != nil || p != "/dev/sg2" {
		t.Errorf("library A = %q, %v; want /dev/sg2", p, err)
	}
	if p, err := loc.Find(config.Library{ID: 30, Serial: "XYZZY_B"}); err != nil || p != "/dev/sg1" {
		t.Errorf("library B = %q, %v; want /dev/sg1", p, err)
	}
	if opened["/dev/sg0"] != 0 || opened["/dev/sg1"] != 0 {
		t.Errorf("opened nodes sysfs already described: %v", opened)
	}
	if _, err := loc.Find(config.Library{ID: 50, Serial: "NOPE"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unknown serial: err = %v, want ErrUnavailable", err)
	}
	if _, err := loc.Find(config.Library{ID: 50}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty serial: err = %v, want ErrUnavailable", err)
	}

	loc.Overrides = map[int]string{10: "/dev/sg7"}
	if p, err := loc.Find(config.Library{ID: 10, Serial: "XYZZY_A"}); err != nil || p != "/dev/sg7" {
		t.Errorf("override = %q, %v; want /dev/sg7", p, err)
	}
}

// An IBM 03584 changer does not report its bare serial in VPD page 80h:
// mhVTL writes it "%-10.10s", two NULs, then the first storage element
// address. sg0 holds the page read on a test VM for XYZZY_40; sg1 has
// XYZZY_4, a prefix of it, which must not match.
func TestLocatorFindReportedSerial(t *testing.T) {
	root := t.TempDir()
	fakeSysfs(t, root, "sg0", "8", "")
	fakeSysfs(t, root, "sg1", "8", "")
	page := func(s string) []byte { return append([]byte{0x08, 0x80, 0, byte(len(s))}, s...) }
	for name, b := range map[string][]byte{
		"sg0": page("XYZZY_40  \x00\x000400\x00\x00\x00\x00\x00\x00"),
		"sg1": page("XYZZY_4   \x00\x000400\x00\x00\x00\x00\x00\x00"),
	} {
		if err := os.WriteFile(filepath.Join(root, "class", "scsi_generic", name, "device", "vpd_pg80"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loc := &Locator{SysRoot: root, DevRoot: "/dev"}
	for serial, want := range map[string]string{"XYZZY_40": "/dev/sg0", "XYZZY_4": "/dev/sg1"} {
		lib := config.Library{ID: 10, Vendor: "IBM", Product: "03584L32", Serial: serial}
		if p, err := loc.Find(lib); err != nil || p != want {
			t.Errorf("03584 %s = %q, %v; want %s", serial, p, err, want)
		}
	}
}

// Two changers that report the same unit serial number, such as two IBM
// 03584 libraries whose serials share their first 10 characters, or a
// changer and a re-export of it, are refused rather than the first taken.
func TestLocatorFindAmbiguous(t *testing.T) {
	root := t.TempDir()
	fakeSysfs(t, root, "sg0", "8", "XYZZY_4000")
	fakeSysfs(t, root, "sg1", "8", "XYZZY_4000")
	fakeSysfs(t, root, "sg2", "1", "XYZZY_4000") // a tape drive does not count
	loc := &Locator{SysRoot: root, DevRoot: "/dev"}
	_, err := loc.FindBySerial("XYZZY_4000", scsi.PeripheralMediumChanger)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "/dev/sg0") || !strings.Contains(err.Error(), "/dev/sg1") {
		t.Errorf("two changers: err = %v", err)
	}
	if p, err := loc.FindBySerial("XYZZY_4000", scsi.PeripheralSequential); err != nil || p != "/dev/sg2" {
		t.Errorf("tape = %q, %v; want /dev/sg2", p, err)
	}
}

// hostedSysfs adds sg node name for a device at hctl on a SCSI host whose
// proc_name is driver, laid out as sysfs does: the scsi_generic device
// link leads to the H:C:T:L directory of the device.
func hostedSysfs(t *testing.T, root, name, hctl, driver, typ, serial string) {
	t.Helper()
	host := hctl[:strings.Index(hctl, ":")]
	dev := filepath.Join(root, "devices", "platform", "host"+host, "target"+hctl[:strings.LastIndex(hctl, ":")], hctl)
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "type"), []byte(typ+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "vpd_pg80"), vpd80(serial), 0o644); err != nil {
		t.Fatal(err)
	}
	sg := filepath.Join(root, "class", "scsi_generic", name)
	if err := os.MkdirAll(sg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(sg, "device")); err != nil {
		t.Fatal(err)
	}
	sh := filepath.Join(root, "class", "scsi_host", "host"+host)
	if err := os.MkdirAll(sh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sh, "proc_name"), []byte(driver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A local re-export of an mhVTL device (tcm_loop) reports the same serial:
// the device on the mhvtl HBA is the one meant. Two on the mhvtl HBA, or
// none there, stay ambiguous.
func TestLocatorFindPrefersMhvtlHBA(t *testing.T) {
	root := t.TempDir()
	hostedSysfs(t, root, "sg0", "3:0:0:0", "mhvtl", "8", "XYZZY_A")
	hostedSysfs(t, root, "sg5", "7:0:0:0", "tcm_loopback", "8", "XYZZY_A")
	hostedSysfs(t, root, "sg1", "3:0:1:0", "mhvtl", "1", "00XYZZY_A1")
	hostedSysfs(t, root, "sg6", "7:0:1:0", "tcm_loopback", "1", "00XYZZY_A1")
	hostedSysfs(t, root, "sg7", "7:0:2:0", "tcm_loopback", "1", "LOOP_ONLY")
	hostedSysfs(t, root, "sg8", "8:0:2:0", "iscsi_tcp", "1", "LOOP_ONLY")
	hostedSysfs(t, root, "sg2", "3:0:2:0", "mhvtl", "8", "XYZZY_4000")
	hostedSysfs(t, root, "sg3", "3:0:3:0", "mhvtl", "8", "XYZZY_4000")
	loc := &Locator{SysRoot: root, DevRoot: "/dev"}
	for _, tc := range []struct {
		serial string
		typ    uint8
		want   string
	}{
		{"XYZZY_A", scsi.PeripheralMediumChanger, "/dev/sg0"},
		{"00XYZZY_A1", scsi.PeripheralSequential, "/dev/sg1"},
	} {
		if p, err := loc.FindBySerial(tc.serial, tc.typ); err != nil || p != tc.want {
			t.Errorf("%s: %q, %v; want %s", tc.serial, p, err, tc.want)
		}
	}
	for _, serial := range []string{"LOOP_ONLY", "XYZZY_4000"} {
		var typ uint8 = scsi.PeripheralSequential
		if serial == "XYZZY_4000" {
			typ = scsi.PeripheralMediumChanger
		}
		if p, err := loc.FindBySerial(serial, typ); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "cannot tell which") {
			t.Errorf("%s: %q, %v; want ambiguous", serial, p, err)
		}
	}
}

// A changer that rejects READ ELEMENT STATUS of its pickers still has an
// inventory, with every picker reported empty.
func TestInventoryPickerRejected(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0001] = "PICKED1L"
	s.full[0x0401] = "A00001L8"
	s.noTransportStatus = true
	m, _ := managerFor(s)
	inv, err := m.Inventory(testLib())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Pickers) != 1 || inv.Pickers[0].Full || !inv.Slots[0].Full {
		t.Errorf("pickers %+v, slot 1 %+v", inv.Pickers, inv.Slots[0])
	}
}

// A changer whose element map has no medium transport element.
func TestInventoryNoPicker(t *testing.T) {
	emap := ibmMap
	emap.Transport = scsi.ElementRange{}
	m, _ := managerFor(newSim(emap))
	inv, err := m.Inventory(testLib())
	if err != nil || len(inv.Pickers) != 0 {
		t.Errorf("Inventory: %+v, %v", inv.Pickers, err)
	}
}
