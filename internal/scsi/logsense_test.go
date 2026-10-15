package scsi

import (
	"bytes"
	"errors"
	"testing"
)

// logPage builds a log page from (code, value) pairs with control byte 0x40.
func logPage(page uint8, params ...LogParam) []byte {
	var body []byte
	for _, p := range params {
		body = append(body, byte(p.Code>>8), byte(p.Code), p.Control, byte(len(p.Value)))
		body = append(body, p.Value...)
	}
	return append([]byte{page, 0, byte(len(body) >> 8), byte(len(body))}, body...)
}

func TestLogSenseCDB(t *testing.T) {
	got := LogSenseCDB(0x6c, 0x01, 0xfffe) // page bits above 0x3f are masked
	want := []byte{0x4d, 0, 0x6c, 0x01, 0, 0, 0, 0xff, 0xfe, 0}
	if !bytes.Equal(got, want) {
		t.Errorf("LogSenseCDB = % x, want % x", got, want)
	}
}

func TestParseLogPage(t *testing.T) {
	b := logPage(0x0c,
		LogParam{Code: 0x0000, Control: 0x40, Value: []byte{0, 0, 0, 0, 0, 0, 0x10, 0}},
		LogParam{Code: 0x0004, Control: 0x40, Value: []byte{0, 0, 0x01, 0x02}},
		LogParam{Code: 0x0042, Control: 0x01, Value: []byte("VOL001L8")},
		LogParam{Code: 0x0050, Control: 0x03, Value: nil}, // zero-length
		LogParam{Code: 0x0100, Control: 0x40, Value: []byte{1}},
	)
	b[0] |= 0x40 // DS/SPF bits must not leak into the page code
	b[1] = 0x00
	p, err := ParseLogPage(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Page != 0x0c || len(p.Params) != 5 {
		t.Fatalf("page 0x%02x with %d params, want 0x0c with 5", p.Page, len(p.Params))
	}
	if v, ok := p.Uint(0x0000); !ok || v != 0x1000 {
		t.Errorf("Uint(0) = %d, %v", v, ok)
	}
	if v, ok := p.Uint(0x0004); !ok || v != 0x0102 {
		t.Errorf("Uint(4) = %d, %v", v, ok)
	}
	if v, ok := p.Uint(0x0100); !ok || v != 1 {
		t.Errorf("Uint(0x100) = %d, %v", v, ok)
	}
	if v, ok := p.Param(0x0042); !ok || string(v) != "VOL001L8" {
		t.Errorf("Param(0x42) = %q, %v", v, ok)
	}
	if v, ok := p.Param(0x0050); !ok || len(v) != 0 {
		t.Errorf("zero-length Param = %q, %v", v, ok)
	}
	if _, ok := p.Uint(0x0050); ok {
		t.Error("Uint of zero-length param reported ok")
	}
	if _, ok := p.Param(0x0099); ok {
		t.Error("absent param reported present")
	}
	if p.Params[2].Control != 0x01 {
		t.Errorf("control = 0x%02x, want 0x01", p.Params[2].Control)
	}
}

func TestParseLogPageUintTooLong(t *testing.T) {
	p, err := ParseLogPage(logPage(0x17, LogParam{Code: 1, Value: make([]byte, 9)}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Uint(1); ok {
		t.Error("Uint of 9-byte param reported ok")
	}
}

func TestParseLogPageTruncatedParam(t *testing.T) {
	b := logPage(0x02,
		LogParam{Code: 0x0000, Value: []byte{0, 0, 0, 7}},
		LogParam{Code: 0x0005, Value: []byte{0, 0, 0, 0, 0, 0, 0, 9}},
	)
	p, err := ParseLogPage(b[:len(b)-3]) // value of 0x0005 cut short
	if !errors.Is(err, ErrShortResponse) {
		t.Fatalf("err = %v, want ErrShortResponse", err)
	}
	if len(p.Params) != 1 || p.Params[0].Code != 0 {
		t.Fatalf("params = %+v, want only 0x0000", p.Params)
	}
	// Cut inside the second parameter's header.
	p, err = ParseLogPage(b[:4+8+2])
	if !errors.Is(err, ErrShortResponse) || len(p.Params) != 1 {
		t.Errorf("header cut: %d params, err %v", len(p.Params), err)
	}
}

func TestParseLogPagePageLengthBounds(t *testing.T) {
	b := logPage(0x0d, LogParam{Code: 0, Value: []byte{0, 31}})
	// Trailing bytes beyond the page length are ignored.
	p, err := ParseLogPage(append(b, 0xde, 0xad, 0xbe, 0xef, 0xff))
	if err != nil || len(p.Params) != 1 {
		t.Errorf("trailing junk: %+v, %v", p, err)
	}
}

func TestParseLogPageShortHeader(t *testing.T) {
	if _, err := ParseLogPage([]byte{0x0c, 0, 0}); !errors.Is(err, ErrShortResponse) {
		t.Errorf("err = %v, want ErrShortResponse", err)
	}
}

func TestParseSupportedLogPages(t *testing.T) {
	b := []byte{0x00, 0, 0, 8, 0x00, 0x02, 0x03, 0x0c, 0x0d, 0x14, 0x17, 0x6e, 0x99}
	got, err := ParseSupportedLogPages(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint8{0x00, 0x02, 0x03, 0x0c, 0x0d, 0x14, 0x17, 0x2e}
	if !bytes.Equal(got, want) {
		t.Errorf("pages = % x, want % x", got, want)
	}
	if _, err := ParseSupportedLogPages([]byte{0, 0}); !errors.Is(err, ErrShortResponse) {
		t.Errorf("short: err = %v", err)
	}
	if _, err := ParseSupportedLogPages([]byte{0x0c, 0, 0, 0}); err == nil {
		t.Error("wrong page code accepted")
	}
}
