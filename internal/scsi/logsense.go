package scsi

import (
	"encoding/binary"
	"fmt"
)

// opLogSense is LOG SENSE (4Dh, SPC).
const opLogSense = 0x4d

// Log page codes (SPC, SSC).
const (
	LogPageSupported = 0x00 // Supported Log Pages
	LogPageTapeAlert = 0x2e // TapeAlert (SSC); reading it clears the flags
)

// LogSenseCDB builds LOG SENSE (4Dh) for the cumulative values (PC 01b) of
// page/subpage, without parameter pointer or save-parameters.
func LogSenseCDB(page, subpage uint8, alloc uint16) []byte {
	cdb := make([]byte, 10)
	cdb[0] = opLogSense
	cdb[2] = 0x40 | page&0x3f
	cdb[3] = subpage
	binary.BigEndian.PutUint16(cdb[7:9], alloc)
	return cdb
}

// LogParam is one log parameter: its code, control byte and value.
type LogParam struct {
	Code    uint16
	Control uint8
	Value   []byte
}

// LogPage is a parsed log page.
type LogPage struct {
	Page    uint8
	Subpage uint8
	Params  []LogParam
}

// ParseLogPage parses LOG SENSE data: the 4-byte page header followed by
// log parameters (2-byte code, control byte, length byte, value). Parsing
// stops at the header's page length or the end of b, whichever is first,
// and never returns a partial parameter: when a parameter's value runs past
// the end it is dropped and the parameters before it are returned with an
// error wrapping ErrShortResponse.
func ParseLogPage(b []byte) (LogPage, error) {
	if len(b) < 4 {
		return LogPage{}, ErrShortResponse
	}
	p := LogPage{Page: b[0] & 0x3f, Subpage: b[1]}
	end := min(4+int(binary.BigEndian.Uint16(b[2:4])), len(b))
	off := 4
	for off < end {
		if off+4 > end {
			return p, fmt.Errorf("log page 0x%02x: parameter header at offset %d: %w", p.Page, off, ErrShortResponse)
		}
		code := binary.BigEndian.Uint16(b[off : off+2])
		n := int(b[off+3])
		if off+4+n > end {
			return p, fmt.Errorf("log page 0x%02x: parameter 0x%04x (%d bytes) at offset %d: %w",
				p.Page, code, n, off, ErrShortResponse)
		}
		p.Params = append(p.Params, LogParam{
			Code:    code,
			Control: b[off+2],
			Value:   b[off+4 : off+4+n],
		})
		off += 4 + n
	}
	return p, nil
}

// Param returns the value of the first parameter with code and whether the
// page carries it.
func (p LogPage) Param(code uint16) ([]byte, bool) {
	for _, prm := range p.Params {
		if prm.Code == code {
			return prm.Value, true
		}
	}
	return nil, false
}

// Uint returns parameter code as a big-endian unsigned integer of 1 to 8
// bytes. It reports false when the parameter is absent, empty or longer than
// 8 bytes.
func (p LogPage) Uint(code uint16) (uint64, bool) {
	v, ok := p.Param(code)
	if !ok || len(v) == 0 || len(v) > 8 {
		return 0, false
	}
	var u uint64
	for _, c := range v {
		u = u<<8 | uint64(c)
	}
	return u, true
}

// ParseSupportedLogPages parses the Supported Log Pages page (00h): after
// the 4-byte header each byte is a page code.
func ParseSupportedLogPages(b []byte) ([]uint8, error) {
	if len(b) < 4 {
		return nil, ErrShortResponse
	}
	if b[0]&0x3f != LogPageSupported {
		return nil, fmt.Errorf("scsi: log page 0x%02x, want 0x00", b[0]&0x3f)
	}
	end := min(4+int(binary.BigEndian.Uint16(b[2:4])), len(b))
	pages := make([]uint8, 0, end-4)
	for _, c := range b[4:end] {
		pages = append(pages, c&0x3f)
	}
	return pages, nil
}
