package scsi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// TestSgIOHdrLayout pins struct sg_io_hdr for linux/amd64 (include/scsi/sg.h):
// sizeof 88 and every field offset as the C compiler lays it out.
func TestSgIOHdrLayout(t *testing.T) {
	var h sgIOHdr
	if got := unsafe.Sizeof(h); got != 88 {
		t.Fatalf("sizeof(sg_io_hdr) = %d, want 88", got)
	}
	offsets := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"interface_id", unsafe.Offsetof(h.InterfaceID), 0},
		{"dxfer_direction", unsafe.Offsetof(h.DxferDirection), 4},
		{"cmd_len", unsafe.Offsetof(h.CmdLen), 8},
		{"mx_sb_len", unsafe.Offsetof(h.MxSbLen), 9},
		{"iovec_count", unsafe.Offsetof(h.IovecCount), 10},
		{"dxfer_len", unsafe.Offsetof(h.DxferLen), 12},
		{"dxferp", unsafe.Offsetof(h.Dxferp), 16},
		{"cmdp", unsafe.Offsetof(h.Cmdp), 24},
		{"sbp", unsafe.Offsetof(h.Sbp), 32},
		{"timeout", unsafe.Offsetof(h.Timeout), 40},
		{"flags", unsafe.Offsetof(h.Flags), 44},
		{"pack_id", unsafe.Offsetof(h.PackID), 48},
		{"usr_ptr", unsafe.Offsetof(h.UsrPtr), 56},
		{"status", unsafe.Offsetof(h.Status), 64},
		{"masked_status", unsafe.Offsetof(h.MaskedStatus), 65},
		{"msg_status", unsafe.Offsetof(h.MsgStatus), 66},
		{"sb_len_wr", unsafe.Offsetof(h.SbLenWr), 67},
		{"host_status", unsafe.Offsetof(h.HostStatus), 68},
		{"driver_status", unsafe.Offsetof(h.DriverStatus), 70},
		{"resid", unsafe.Offsetof(h.Resid), 72},
		{"duration", unsafe.Offsetof(h.Duration), 76},
		{"info", unsafe.Offsetof(h.Info), 80},
	}
	for _, o := range offsets {
		if o.got != o.want {
			t.Errorf("offsetof(%s) = %d, want %d", o.name, o.got, o.want)
		}
	}
	if sgIO != 0x2285 || sgInterfaceID != 'S' {
		t.Errorf("SG_IO = %#x / interface id %q", sgIO, rune(sgInterfaceID))
	}
	if sgDxferNone != -1 || sgDxferToDev != -2 || sgDxferFromDev != -3 || sgDxferToFromDev != -4 {
		t.Error("dxfer_direction constants do not match include/scsi/sg.h")
	}
}

func TestCheckHdr(t *testing.T) {
	sense := make([]byte, senseBufLen)
	copy(sense, FixedSense(KeyIllegalRequest, 0x3b, 0x0e))

	n, sb, err := checkHdr(&sgIOHdr{Resid: 10}, 64, sense)
	if err != nil || n != 54 || sb != nil {
		t.Fatalf("GOOD: n=%d sb=%v err=%v, want 54/nil/nil", n, sb, err)
	}

	_, sb, err = checkHdr(&sgIOHdr{Status: statusCheckCondition, SbLenWr: 18, DriverStatus: driverSense}, 0, sense)
	var se *SenseError
	if !errors.As(err, &se) {
		t.Fatalf("CHECK CONDITION err = %v, want *SenseError", err)
	}
	if se.Key != KeyIllegalRequest || se.Code() != ASCMediumSrcEmpty || len(sb) != 18 {
		t.Errorf("sense = %+v (sb %d bytes), want ILLEGAL REQUEST 3B/0E", se, len(sb))
	}

	_, _, err = checkHdr(&sgIOHdr{Status: 0x18}, 0, sense)
	var ste *StatusError
	if !errors.As(err, &ste) || ste.Status != 0x18 {
		t.Errorf("RESERVATION CONFLICT err = %v, want *StatusError 0x18", err)
	}

	for _, h := range []sgIOHdr{{HostStatus: 0x01}, {DriverStatus: 0x06}} {
		_, _, err = checkHdr(&h, 0, sense)
		var te *TransportError
		if !errors.As(err, &te) {
			t.Errorf("host %#x driver %#x: err = %v, want *TransportError", h.HostStatus, h.DriverStatus, err)
		}
	}

	if n, _, _ := checkHdr(&sgIOHdr{Resid: 100}, 64, sense); n != 0 {
		t.Errorf("resid > len: n = %d, want 0", n)
	}
}

func TestCDBBuilders(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"inquiry std", InquiryCDB(false, 0, 96), []byte{0x12, 0, 0, 0, 96, 0}},
		{"inquiry vpd80", InquiryCDB(true, VPDUnitSerial, 0x0104), []byte{0x12, 1, 0x80, 0x01, 0x04, 0}},
		{"mode sense 6", ModeSense6CDB(PageElementAddress, 0xff), []byte{0x1a, 0x08, 0x1d, 0, 0xff, 0}},
		{"mode sense 10", ModeSense10CDB(PageElementAddress, 0x0400), []byte{0x5a, 0x08, 0x1d, 0, 0, 0, 0, 0x04, 0x00, 0}},
		{"read element status voltag", ReadElementStatusCDB(ElementStorage, true, 0x03e8, 0x0014, 0x010000),
			[]byte{0xb8, 0x12, 0x03, 0xe8, 0x00, 0x14, 0, 0x01, 0x00, 0x00, 0, 0}},
		{"read element status no voltag", ReadElementStatusCDB(ElementDTE, false, 0x01f4, 2, 0xabcdef),
			[]byte{0xb8, 0x04, 0x01, 0xf4, 0x00, 0x02, 0, 0xab, 0xcd, 0xef, 0, 0}},
		{"move medium", MoveMediumCDB(0x0001, 0x03e8, 0x01f4),
			[]byte{0xa5, 0, 0x00, 0x01, 0x03, 0xe8, 0x01, 0xf4, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s = % x, want % x", c.name, c.got, c.want)
		}
	}
}

func TestParseStdInquiry(t *testing.T) {
	b := make([]byte, 36)
	b[0] = 0x08
	copy(b[8:], "STK     L700            0107")
	inq, err := ParseStdInquiry(b)
	if err != nil {
		t.Fatal(err)
	}
	if inq.PeripheralType != PeripheralMediumChanger || inq.Vendor != "STK" || inq.Product != "L700" || inq.Revision != "0107" {
		t.Errorf("inquiry = %+v", inq)
	}
	if _, err := ParseStdInquiry(b[:20]); !errors.Is(err, ErrShortResponse) {
		t.Errorf("short inquiry err = %v", err)
	}
}

func TestParseVPDSerial(t *testing.T) {
	b := append([]byte{0x08, 0x80, 0x00, 0x0a}, []byte("XYZZY_A   ")...)
	s, err := ParseVPDSerial(b)
	if err != nil || s != "XYZZY_A" {
		t.Fatalf("serial = %q, %v; want XYZZY_A", s, err)
	}
	// Captured from mhVTL's STK L700 sysfs vpd_pg80: blanks, then NULs.
	live := append([]byte{0x08, 0x80, 0x00, 0x12}, []byte("XYZZY_A     \x00\x00\x00\x00\x00\x00")...)
	if s, err := ParseVPDSerial(live); err != nil || s != "XYZZY_A" {
		t.Fatalf("live serial = %q, %v; want XYZZY_A", s, err)
	}
	if _, err := ParseVPDSerial([]byte{0x08, 0x83, 0, 0}); err == nil {
		t.Error("page 0x83 accepted as 0x80")
	}
	if _, err := ParseVPDSerial([]byte{0x08, 0x80, 0, 20, 'A'}); !errors.Is(err, ErrShortResponse) {
		t.Errorf("truncated serial err = %v", err)
	}
}

// page1D builds an Element Address Assignment page as mhvtl's
// add_mode_element_address_assignment() does.
func page1D(mte, nmte, st, nst, ie, nie, dte, ndte uint16) []byte {
	p := make([]byte, 20)
	p[0], p[1] = 0x1d, 18
	for i, v := range []uint16{mte, nmte, st, nst, ie, nie, dte, ndte} {
		binary.BigEndian.PutUint16(p[2+2*i:], v)
	}
	return p
}

func TestParseElementAddressPage(t *testing.T) {
	// IBM 3584 style addresses: MTE 1, storage 0x0401.., I/E 0x0301.., DTE 0x0101..
	pg := page1D(0x0001, 1, 0x0401, 20, 0x0301, 4, 0x0101, 2)
	want := ElementMap{
		Transport: ElementRange{1, 1},
		Storage:   ElementRange{0x0401, 20},
		IE:        ElementRange{0x0301, 4},
		DTE:       ElementRange{0x0101, 2},
	}

	six := append([]byte{23, 0, 0, 0}, pg...)
	if got, err := ParseElementAddressPage(six, false); err != nil || got != want {
		t.Errorf("mode sense(6) = %+v, %v; want %+v", got, err, want)
	}

	// MODE SENSE(10) with an 8-byte block descriptor that must be skipped.
	ten := append([]byte{0, 34, 0, 0, 0, 0, 0, 8}, make([]byte, 8)...)
	ten = append(ten, pg...)
	if got, err := ParseElementAddressPage(ten, true); err != nil || got != want {
		t.Errorf("mode sense(10) = %+v, %v; want %+v", got, err, want)
	}

	bad := append([]byte{23, 0, 0, 0}, page1D(0, 0, 0, 0, 0, 0, 0, 0)...)
	bad[4] = 0x1f
	if _, err := ParseElementAddressPage(bad, false); err == nil {
		t.Error("wrong page code accepted")
	}
	if _, err := ParseElementAddressPage(six[:10], false); !errors.Is(err, ErrShortResponse) {
		t.Errorf("short page err = %v", err)
	}
}

func TestElementMapLocateAndAddress(t *testing.T) {
	m := ElementMap{
		Transport: ElementRange{1, 1},
		Storage:   ElementRange{1000, 20},
		IE:        ElementRange{10, 4},
		DTE:       ElementRange{500, 2},
	}
	cases := []struct {
		addr uint16
		typ  uint8
		n    int
	}{
		{1, ElementTransport, 1},
		{1000, ElementStorage, 1},
		{1019, ElementStorage, 20},
		{1020, ElementAll, 0},
		{13, ElementIE, 4},
		{501, ElementDTE, 2},
		{2, ElementAll, 0},
	}
	for _, c := range cases {
		if typ, n := m.Locate(c.addr); typ != c.typ || n != c.n {
			t.Errorf("Locate(%d) = %d/%d, want %d/%d", c.addr, typ, n, c.typ, c.n)
		}
	}
	if a, ok := m.Storage.Address(3); !ok || a != 1002 {
		t.Errorf("Storage.Address(3) = %d, %v", a, ok)
	}
	for _, n := range []int{0, 21, -1} {
		if _, ok := m.Storage.Address(n); ok {
			t.Errorf("Storage.Address(%d) accepted", n)
		}
	}
}

// desc builds one element descriptor the way mhvtl's fill_ed() does: 12 fixed
// bytes, the 36-byte volume tag when voltag, then pad bytes (the device
// identifier area for a DTE, reserved bytes otherwise).
func desc(addr uint16, status byte, svalid bool, src uint16, voltag bool, barcode string, pad int) []byte {
	d := make([]byte, 12)
	binary.BigEndian.PutUint16(d[0:2], addr)
	d[2] = status
	if svalid {
		d[9] = 0x80 | 0x01
	}
	binary.BigEndian.PutUint16(d[10:12], src)
	if voltag {
		vt := make([]byte, 36)
		if barcode != "" {
			copy(vt, barcode+strings.Repeat(" ", 32-len(barcode)))
		}
		d = append(d, vt...)
	}
	return append(d, make([]byte, pad)...)
}

// page wraps descriptors in an element status page header.
func page(typ byte, voltag bool, descs ...[]byte) []byte {
	dl := len(descs[0])
	h := make([]byte, 8)
	h[0] = typ
	if voltag {
		h[1] = 0x80
	}
	binary.BigEndian.PutUint16(h[2:4], uint16(dl))
	n := dl * len(descs)
	h[5], h[6], h[7] = byte(n>>16), byte(n>>8), byte(n)
	for _, d := range descs {
		h = append(h, d...)
	}
	return h
}

// report wraps pages in the element status data header.
func report(first, count uint16, pages ...[]byte) []byte {
	var body []byte
	for _, p := range pages {
		body = append(body, p...)
	}
	h := make([]byte, 8)
	binary.BigEndian.PutUint16(h[0:2], first)
	binary.BigEndian.PutUint16(h[2:4], count)
	n := len(body)
	h[5], h[6], h[7] = byte(n>>16), byte(n>>8), byte(n)
	return append(h, body...)
}

func TestParseReadElementStatus(t *testing.T) {
	// DTE page: 16+36 voltag + 34-byte device identifier (STK dvcid layout).
	dte := page(ElementDTE, true,
		desc(500, 0x01, true, 1002, true, "E01001L8", 4+34),
		desc(501, 0x08, false, 0, true, "", 4+34),
	)
	st := page(ElementStorage, true,
		desc(1000, 0x09, false, 0, true, "E01002L8", 4),
		desc(1001, 0x08, false, 0, true, "", 4),
		desc(1002, 0x08, false, 0, true, "", 4),
	)
	// I/E page without volume tags.
	ie := page(ElementIE, false, desc(10, 0x0b, false, 0, false, "", 4))
	b := report(10, 6, dte, st, ie)

	els, err := ParseReadElementStatus(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(els) != 6 {
		t.Fatalf("parsed %d elements, want 6: %+v", len(els), els)
	}
	d0 := els[0]
	if d0.Type != ElementDTE || d0.Address != 500 || !d0.Full || !d0.SourceValid || d0.Source != 1002 || d0.Barcode != "E01001L8" {
		t.Errorf("drive 0 = %+v", d0)
	}
	if d1 := els[1]; d1.Full || d1.Barcode != "" || !d1.Access {
		t.Errorf("drive 1 = %+v", d1)
	}
	if s0 := els[2]; s0.Type != ElementStorage || s0.Address != 1000 || !s0.Full || s0.Barcode != "E01002L8" {
		t.Errorf("slot 1 = %+v", s0)
	}
	if m := els[5]; m.Type != ElementIE || !m.Full || !m.ImpExp || m.Barcode != "" {
		t.Errorf("map 1 = %+v", m)
	}

	// Except + ASC/ASCQ (mhvtl identity-only DTE: drive not present 82/00).
	ex := desc(502, 0x04, false, 0, false, "", 4)
	ex[4], ex[5] = 0x82, 0x00
	els, err = ParseReadElementStatus(report(502, 1, page(ElementDTE, false, ex)))
	if err != nil || len(els) != 1 || !els[0].Except || els[0].ASC != 0x82 {
		t.Errorf("except element = %+v, %v", els, err)
	}
}

func TestParseReadElementStatusTruncated(t *testing.T) {
	st := page(ElementStorage, true,
		desc(1000, 0x09, false, 0, true, "A00001L8", 4),
		desc(1001, 0x09, false, 0, true, "A00002L8", 4),
	)
	b := report(1000, 2, st)
	// Cut the second descriptor in half: only the first, whole one is returned.
	cut := b[:len(b)-20]
	els, err := ParseReadElementStatus(cut)
	if err != nil || len(els) != 1 || els[0].Barcode != "A00001L8" {
		t.Fatalf("truncated parse = %+v, %v; want only the first descriptor", els, err)
	}
	total, _ := ElementStatusTotal(cut)
	if total != len(b) {
		t.Errorf("ElementStatusTotal = %d, want %d", total, len(b))
	}
	if _, err := ParseReadElementStatus(b[:5]); !errors.Is(err, ErrShortResponse) {
		t.Errorf("short header err = %v", err)
	}
	// Byte count above 0xff exercises all three byte-count bytes.
	big := make([]byte, 8)
	big[6], big[7] = 0x01, 0x08
	if total, _ := ElementStatusTotal(big); total != 8+0x108 {
		t.Errorf("ElementStatusTotal(0x108) = %d", total)
	}
}

func TestDecodeSense(t *testing.T) {
	cases := []struct {
		name            string
		b               []byte
		key, asc, ascq  uint8
		wantSubstrInErr string
	}{
		{"fixed current", FixedSense(KeyIllegalRequest, 0x3b, 0x0d), KeyIllegalRequest, 0x3b, 0x0d, "destination element full"},
		{"fixed deferred", append([]byte{0x71}, FixedSense(KeyNotReady, 0x04, 0x12)[1:]...), KeyNotReady, 0x04, 0x12, "logical unit offline"},
		{"descriptor", []byte{0x72, 0x05, 0x21, 0x01, 0, 0, 0, 0}, KeyIllegalRequest, 0x21, 0x01, "invalid element address"},
		{"unknown asc", FixedSense(KeyHardwareError, 0x44, 0x00), KeyHardwareError, 0x44, 0, "HARDWARE ERROR"},
		{"empty", nil, 0, 0, 0, "NO SENSE"},
	}
	for _, c := range cases {
		e := DecodeSense(c.b)
		if e.Key != c.key || e.ASC != c.asc || e.ASCQ != c.ascq {
			t.Errorf("%s: decoded %x/%02x/%02x, want %x/%02x/%02x", c.name, e.Key, e.ASC, e.ASCQ, c.key, c.asc, c.ascq)
		}
		if !strings.Contains(e.Error(), c.wantSubstrInErr) {
			t.Errorf("%s: Error() = %q, want it to contain %q", c.name, e.Error(), c.wantSubstrInErr)
		}
	}
}
