package scsi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Operation codes.
const (
	opInquiry           = 0x12
	opModeSense6        = 0x1a
	opModeSense10       = 0x5a
	opMoveMedium        = 0xa5
	opReadElementStatus = 0xb8
)

// Mode page and VPD page codes.
const (
	PageElementAddress = 0x1d // Element Address Assignment mode page (SMC)
	VPDUnitSerial      = 0x80 // Unit Serial Number VPD page
)

// Element type codes (SMC-3 table 9).
const (
	ElementAll       = 0
	ElementTransport = 1 // medium transport (MTE, the picker)
	ElementStorage   = 2
	ElementIE        = 3 // import/export (MAP)
	ElementDTE       = 4 // data transfer (drive)
)

// PeripheralMediumChanger is the INQUIRY peripheral device type of an SMC
// device.
const PeripheralMediumChanger = 0x08

// voltagIDLen is the primary volume tag identifier width inside the 36-byte
// volume tag field.
const voltagIDLen = 32

// InquiryCDB builds INQUIRY (12h). evpd selects a VPD page.
func InquiryCDB(evpd bool, page uint8, alloc uint16) []byte {
	cdb := make([]byte, 6)
	cdb[0] = opInquiry
	if evpd {
		cdb[1] = 0x01
		cdb[2] = page
	}
	binary.BigEndian.PutUint16(cdb[3:5], alloc)
	return cdb
}

// ModeSense6CDB builds MODE SENSE(6) (1Ah) for the current values of page
// with block descriptors disabled (DBD).
func ModeSense6CDB(page, alloc uint8) []byte {
	return []byte{opModeSense6, 0x08, page & 0x3f, 0, alloc, 0}
}

// ModeSense10CDB builds MODE SENSE(10) (5Ah) for the current values of page
// with block descriptors disabled (DBD).
func ModeSense10CDB(page uint8, alloc uint16) []byte {
	cdb := make([]byte, 10)
	cdb[0] = opModeSense10
	cdb[1] = 0x08
	cdb[2] = page & 0x3f
	binary.BigEndian.PutUint16(cdb[7:9], alloc)
	return cdb
}

// ReadElementStatusCDB builds READ ELEMENT STATUS (B8h) for up to count
// elements of elemType starting at start. voltag requests the primary volume
// tag (barcode). alloc is a 24-bit allocation length.
func ReadElementStatusCDB(elemType uint8, voltag bool, start, count uint16, alloc uint32) []byte {
	cdb := make([]byte, 12)
	cdb[0] = opReadElementStatus
	cdb[1] = elemType & 0x0f
	if voltag {
		cdb[1] |= 0x10
	}
	binary.BigEndian.PutUint16(cdb[2:4], start)
	binary.BigEndian.PutUint16(cdb[4:6], count)
	cdb[7] = byte(alloc >> 16)
	cdb[8] = byte(alloc >> 8)
	cdb[9] = byte(alloc)
	return cdb
}

// MoveMediumCDB builds MOVE MEDIUM (A5h) from source to dest using the
// transport element.
func MoveMediumCDB(transport, source, dest uint16) []byte {
	cdb := make([]byte, 12)
	cdb[0] = opMoveMedium
	binary.BigEndian.PutUint16(cdb[2:4], transport)
	binary.BigEndian.PutUint16(cdb[4:6], source)
	binary.BigEndian.PutUint16(cdb[6:8], dest)
	return cdb
}

// ErrShortResponse is returned when a response is too short to parse.
var ErrShortResponse = errors.New("scsi: response too short")

// StdInquiry is the subset of standard INQUIRY data mhvtld uses.
type StdInquiry struct {
	PeripheralType uint8
	Vendor         string
	Product        string
	Revision       string
}

// ParseStdInquiry parses standard INQUIRY data (at least 36 bytes).
func ParseStdInquiry(b []byte) (StdInquiry, error) {
	if len(b) < 36 {
		return StdInquiry{}, ErrShortResponse
	}
	return StdInquiry{
		PeripheralType: b[0] & 0x1f,
		Vendor:         strings.TrimSpace(string(b[8:16])),
		Product:        strings.TrimSpace(string(b[16:32])),
		Revision:       strings.TrimSpace(string(b[32:36])),
	}, nil
}

// ParseVPDSerial returns the unit serial number from a VPD page 80h response
// (or the identical sysfs vpd_pg80 blob), trimmed of padding.
func ParseVPDSerial(b []byte) (string, error) {
	if len(b) < 4 {
		return "", ErrShortResponse
	}
	if b[1] != VPDUnitSerial {
		return "", fmt.Errorf("scsi: VPD page 0x%02x, want 0x80", b[1])
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if 4+n > len(b) {
		return "", ErrShortResponse
	}
	// Devices pad with blanks, NULs or both (mhVTL: blanks, then NULs).
	return strings.Trim(string(b[4:4+n]), " \x00"), nil
}

// ElementRange is the first element address and number of elements of one
// element type.
type ElementRange struct {
	First uint16
	Count uint16
}

// Contains reports whether addr is inside the range.
func (r ElementRange) Contains(addr uint16) bool {
	return r.Count > 0 && addr >= r.First && uint32(addr) < uint32(r.First)+uint32(r.Count)
}

// Address returns the element address of 1-based ordinal n and whether n is in
// range.
func (r ElementRange) Address(n int) (uint16, bool) {
	if n < 1 || n > int(r.Count) {
		return 0, false
	}
	return uint16(int(r.First) + n - 1), true
}

// ElementMap is the Element Address Assignment mode page (1Dh).
type ElementMap struct {
	Transport ElementRange
	Storage   ElementRange
	IE        ElementRange
	DTE       ElementRange
}

// Locate returns the element type and 1-based ordinal of addr, or
// (ElementAll, 0) when addr belongs to no range.
func (m ElementMap) Locate(addr uint16) (uint8, int) {
	for _, r := range []struct {
		typ uint8
		rng ElementRange
	}{
		{ElementTransport, m.Transport},
		{ElementStorage, m.Storage},
		{ElementIE, m.IE},
		{ElementDTE, m.DTE},
	} {
		if r.rng.Contains(addr) {
			return r.typ, int(addr-r.rng.First) + 1
		}
	}
	return ElementAll, 0
}

// ParseElementAddressPage extracts page 1Dh from MODE SENSE data. tenByte
// selects the MODE SENSE(10) parameter header (8 bytes) over the (6) one
// (4 bytes); any block descriptors are skipped.
func ParseElementAddressPage(b []byte, tenByte bool) (ElementMap, error) {
	var off int
	switch {
	case tenByte && len(b) >= 8:
		off = 8 + int(binary.BigEndian.Uint16(b[6:8]))
	case !tenByte && len(b) >= 4:
		off = 4 + int(b[3])
	default:
		return ElementMap{}, ErrShortResponse
	}
	if off+20 > len(b) {
		return ElementMap{}, ErrShortResponse
	}
	p := b[off:]
	if p[0]&0x3f != PageElementAddress || p[1] < 18 {
		return ElementMap{}, fmt.Errorf("scsi: mode page 0x%02x (len %d), want 0x1d", p[0]&0x3f, p[1])
	}
	r := func(i int) ElementRange {
		return ElementRange{First: binary.BigEndian.Uint16(p[i : i+2]), Count: binary.BigEndian.Uint16(p[i+2 : i+4])}
	}
	return ElementMap{Transport: r(2), Storage: r(6), IE: r(10), DTE: r(14)}, nil
}

// Element is one element descriptor from READ ELEMENT STATUS.
type Element struct {
	Type    uint8
	Address uint16
	Full    bool
	ImpExp  bool // IE only: medium was placed by the operator
	Except  bool
	Access  bool
	ASC     uint8
	ASCQ    uint8
	// SourceValid reports whether Source holds the element the medium was
	// last moved from.
	SourceValid bool
	Source      uint16
	// Barcode is the primary volume tag identifier, blank-trimmed; empty when
	// the page carries no volume tag or the element is empty.
	Barcode string
}

// ElementStatusTotal returns the byte count the target needs for the full
// report: the 8-byte header plus the header's byte count of report.
func ElementStatusTotal(b []byte) (int, error) {
	if len(b) < 8 {
		return 0, ErrShortResponse
	}
	return 8 + (int(b[5])<<16 | int(b[6])<<8 | int(b[7])), nil
}

// ParseReadElementStatus parses READ ELEMENT STATUS data: the 8-byte element
// status header followed by one or more element status pages, each an 8-byte
// page header and whole descriptors of the page's descriptor length. Parsing
// stops at the header's byte count or the end of b, whichever is first, and
// never returns a partial descriptor.
func ParseReadElementStatus(b []byte) ([]Element, error) {
	total, err := ElementStatusTotal(b)
	if err != nil {
		return nil, err
	}
	end := min(total, len(b))
	var out []Element
	off := 8
	for off+8 <= end {
		ph := b[off : off+8]
		typ := ph[0] & 0x0f
		pvoltag := ph[1]&0x80 != 0
		dlen := int(binary.BigEndian.Uint16(ph[2:4]))
		pbytes := int(ph[5])<<16 | int(ph[6])<<8 | int(ph[7])
		off += 8
		if dlen < 12 {
			return out, fmt.Errorf("scsi: element descriptor length %d too short", dlen)
		}
		pend := min(off+pbytes, end)
		for ; off+dlen <= pend; off += dlen {
			out = append(out, parseDescriptor(typ, pvoltag, b[off:off+dlen]))
		}
		off = pend
	}
	return out, nil
}

func parseDescriptor(typ uint8, pvoltag bool, d []byte) Element {
	e := Element{
		Type:        typ,
		Address:     binary.BigEndian.Uint16(d[0:2]),
		Full:        d[2]&0x01 != 0,
		ImpExp:      d[2]&0x02 != 0,
		Except:      d[2]&0x04 != 0,
		Access:      d[2]&0x08 != 0,
		ASC:         d[4],
		ASCQ:        d[5],
		SourceValid: d[9]&0x80 != 0,
		Source:      binary.BigEndian.Uint16(d[10:12]),
	}
	if pvoltag && e.Full && len(d) >= 12+voltagIDLen {
		e.Barcode = strings.TrimRight(string(d[12:12+voltagIDLen]), " \x00")
	}
	return e
}
