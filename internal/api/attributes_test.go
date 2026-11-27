package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/drivestats"
	"mhvtl-control/internal/scsi"
)

// fakeDriveAttrs records WRITE ATTRIBUTE requests and, like vtltape, has
// the cartridge's mam file rewritten when one succeeds.
type fakeDriveAttrs struct {
	drives  []int
	serials []string
	attrs   [][]scsi.Attribute
	err    error
	// mam is rewritten with the attributes written, when set.
	mam string
}

func (f *fakeDriveAttrs) WriteAttributes(d config.Drive, serial string, attrs []scsi.Attribute) error {
	f.drives = append(f.drives, d.ID)
	f.serials = append(f.serials, serial)
	f.attrs = append(f.attrs, attrs)
	if f.err != nil {
		return f.err
	}
	if f.mam != "" {
		recs := []mamRec{{0x0401, pad(serial, 32, ' ')}}
		for _, a := range attrs {
			recs = append(recs, mamRec{a.ID, a.Value})
		}
		if err := os.WriteFile(f.mam, mamBytes(recs), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeSerials gives each cartridge a mam file holding only its medium
// serial number, "<barcode>_1".
func writeSerials(t *testing.T, home string, barcodes ...string) {
	t.Helper()
	for _, bc := range barcodes {
		mam := mamBytes([]mamRec{{0x0401, pad(bc+"_1", 32, ' ')}})
		if err := os.WriteFile(filepath.Join(home, bc, "mam"), mam, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type mamRec struct {
	id  uint16
	val []byte
}

// mamBytes lays out a mam file as mhvtl write_mam does: two host-order
// u32 format versions, then host-order id and length and the value.
func mamBytes(recs []mamRec) []byte {
	b := binary.NativeEndian.AppendUint32(nil, 3)
	b = binary.NativeEndian.AppendUint32(b, 4)
	for _, r := range recs {
		b = binary.NativeEndian.AppendUint16(b, r.id)
		b = binary.NativeEndian.AppendUint16(b, uint16(len(r.val)))
		b = append(b, r.val...)
	}
	return b
}

func pad(s string, n int, c byte) []byte {
	return append([]byte(s), bytes.Repeat([]byte{c}, n-len(s))...)
}

type attrsResp struct {
	Barcode    string `json:"barcode"`
	Attributes []struct {
		ID       string `json:"id"`
		Group    string `json:"group"`
		Value    string `json:"value"`
		Editable bool   `json:"editable"`
	} `json:"attributes"`
	LoadedIn  *loadedInDTO `json:"loaded_in"`
	LiveError string       `json:"live_error"`
	Note      string       `json:"note"`
}

func (r attrsResp) value(id string) string {
	for _, a := range r.Attributes {
		if a.ID == id {
			return a.Value
		}
	}
	return "<absent>"
}

func decodeAttrs(t *testing.T, body []byte) attrsResp {
	t.Helper()
	var r attrsResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return r
}

func TestMediaAttributesGet(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "A00002L8")
	home := homeOf(t, dir)
	mam := mamBytes([]mamRec{{0x0801, pad("Bareos", 32, ' ')}, {0x0408, []byte{0}}})
	if err := os.WriteFile(filepath.Join(home, "A00002L8", "mam"), mam, 0o644); err != nil {
		t.Fatal(err)
	}
	lib := mustLib(t, dir, 10)
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: lib.Drives[1].Slot, Full: true, Barcode: "A00002L8"}},
	}}
	h := NewServer(Config{ConfigDir: dir, Changers: fc}).Handler()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media/A00002L8/attributes", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	r := decodeAttrs(t, rec.Body.Bytes())
	if r.Barcode != "A00002L8" || r.value("0x0801") != "Bareos" || r.value("0x0408") != "data" || r.Note == "" {
		t.Errorf("resp = %+v", r)
	}
	if r.LoadedIn == nil || r.LoadedIn.Library != 10 || r.LoadedIn.Position != lib.Drives[1].Slot || r.LoadedIn.Drive != lib.Drives[1].ID {
		t.Errorf("loaded_in = %+v", r.LoadedIn)
	}

	// On the shelf: no loaded_in; unreadable changer: live_error.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media/A00001L8/attributes", "")
	if r := decodeAttrs(t, rec.Body.Bytes()); rec.Code != 200 || r.LoadedIn != nil || r.LiveError != "" {
		t.Errorf("shelf: %d %s", rec.Code, rec.Body.String())
	}
	h = NewServer(Config{ConfigDir: dir}).Handler()
	rec = doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media/A00002L8/attributes", "")
	if r := decodeAttrs(t, rec.Body.Bytes()); rec.Code != 200 || r.LoadedIn != nil || !strings.Contains(r.LiveError, "library 10") {
		t.Errorf("no changer: %d %s", rec.Code, rec.Body.String())
	}

	for path, status := range map[string]int{
		"/api/v1/libraries/10/media/NOPE0001/attributes": 404,
		"/api/v1/libraries/10/media/A-1/attributes":      400,
		"/api/v1/libraries/99/media/A00001L8/attributes": 404,
	} {
		if rec := doJSON(t, h, http.MethodGet, path, ""); rec.Code != status {
			t.Errorf("%s: status = %d, want %d", path, rec.Code, status)
		}
	}
}

func TestMediaAttributesEdit(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "A00002L8")
	home := homeOf(t, dir)
	lib := mustLib(t, dir, 10)
	drive := lib.Drives[2]
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: drive.Slot, Full: true, Barcode: "A00002L8"}},
	}}
	writeSerials(t, home, "A00001L8", "A00002L8")
	fa := &fakeDriveAttrs{mam: filepath.Join(home, "A00002L8", "mam")}
	h := NewServer(Config{ConfigDir: dir, Changers: fc, DriveAttributes: fa}).Handler()

	rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00002L8/attributes",
		`{"attributes":{"0x0808":"Full","0X801":"Bareos"}}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fa.drives) != 1 || fa.drives[0] != drive.ID || fa.serials[0] != "A00002L8_1" {
		t.Fatalf("drives = %v serials = %q, want [%d] [A00002L8_1]", fa.drives, fa.serials, drive.ID)
	}
	want := []scsi.Attribute{
		{ID: 0x0801, Format: scsi.AttrASCII, Value: pad("Bareos", 32, ' ')},
		{ID: 0x0808, Format: scsi.AttrText, Value: pad("Full", 160, 0)},
	}
	if got := fa.attrs[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attrs = %v\nwant %v", got, want)
	}
	r := decodeAttrs(t, rec.Body.Bytes())
	if r.value("0x0801") != "Bareos" || r.value("0x0808") != "Full" || r.LoadedIn == nil || r.LoadedIn.Drive != drive.ID {
		t.Errorf("resp = %s", rec.Body.String())
	}

	cases := []struct {
		name, path, body string
		status           int
		msg              string
	}{
		{"not loaded", "/api/v1/libraries/10/media/A00001L8/attributes", `{"attributes":{"0x0801":"x"}}`, 409, "not_loaded"},
		{"missing", "/api/v1/libraries/10/media/NOPE0001/attributes", `{"attributes":{"0x0801":"x"}}`, 404, ""},
		{"empty", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{}}`, 400, "nothing to change"},
		{"read only", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0401":"x"}}`, 400, "not editable"},
		{"binary", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0805":"0"}}`, 400, "not editable"},
		{"medium type", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0408":"x"}}`, 400, "not editable"},
		{"vendor", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x1400":"x"}}`, 400, "not editable"},
		{"too long", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0800":"123456789"}}`, 400, "at most 8"},
		{"not ascii", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0801":"café"}}`, 400, "ASCII"},
		{"bad id", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"801":"x"}}`, 400, "0x prefix"},
		{"duplicate", "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x801":"x","0x0801":"y"}}`, 400, "twice"},
		{"bad json", "/api/v1/libraries/10/media/A00002L8/attributes", `{`, 400, ""},
	}
	for _, c := range cases {
		fa.drives = nil
		rec := doJSON(t, h, http.MethodPatch, c.path, c.body)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, rec.Code, rec.Body.String(), c.status, c.msg)
		}
		if len(fa.drives) != 0 {
			t.Errorf("%s: WRITE ATTRIBUTE sent", c.name)
		}
	}

	// The cartridge in another library sharing the home is written through
	// that library's drive.
	l30 := mustLib(t, dir, 30)
	fc30 := &fakeChangers{invByLib: map[int]changer.Inventory{
		10: {},
		30: {Drives: []changer.DriveState{{Number: l30.Drives[0].Slot, Full: true, Barcode: "A00001L8"}}},
	}}
	fa = &fakeDriveAttrs{}
	h = NewServer(Config{ConfigDir: dir, Changers: fc30, DriveAttributes: fa}).Handler()
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8/attributes", `{"attributes":{"0x0801":"x"}}`); rec.Code != 200 ||
		len(fa.drives) != 1 || fa.drives[0] != l30.Drives[0].ID {
		t.Errorf("library 30: %d %s, drives %v", rec.Code, rec.Body.String(), fa.drives)
	}
}

func TestMediaAttributesEditErrors(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00002L8", "A00003L8")
	writeSerials(t, homeOf(t, dir), "A00002L8")
	lib := mustLib(t, dir, 10)
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: lib.Drives[0].Slot, Full: true, Barcode: "A00002L8"}},
	}}
	const path, body = "/api/v1/libraries/10/media/A00002L8/attributes", `{"attributes":{"0x0801":"x"}}`
	notReady := scsi.DecodeSense(scsi.FixedSense(scsi.KeyNotReady, 0x3a, 0))
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{fmt.Errorf("drive 11: %w: %w", drivestats.ErrReserved, &scsi.StatusError{Status: 0x18}), 409, "reserved by another initiator"},
		{fmt.Errorf("drive 11: %w: it holds medium serial %q", drivestats.ErrMediumChanged, "B00002L8_1"), 409, "nothing was written"},
		{fmt.Errorf("drive 11: %w: after the write it holds B00002L8_1", drivestats.ErrWriteUnverified), 500, "write_unverified"},
		{fmt.Errorf("drive 11: write attribute: %w", notReady), 422, "medium not present"},
		{fmt.Errorf("%w: drive 11: no node", drivestats.ErrUnavailable), 503, "drive_unavailable"},
		{fmt.Errorf("%w: gone", changer.ErrUnavailable), 503, "drive_unavailable"},
		{&scsi.TransportError{HostStatus: 1}, 503, "transport failure"},
		{errors.New("odd"), 500, "odd"},
	}
	for _, c := range cases {
		h := NewServer(Config{ConfigDir: dir, Changers: fc, DriveAttributes: &fakeDriveAttrs{err: c.err}}).Handler()
		if rec := doJSON(t, h, http.MethodPatch, path, body); rec.Code != c.status || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%v: %d %s, want %d %q", c.err, rec.Code, rec.Body.String(), c.status, c.msg)
		}
	}
	// A cartridge whose MAM has no medium serial number cannot be checked
	// in the drive, so nothing is sent.
	fc3 := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: lib.Drives[0].Slot, Full: true, Barcode: "A00003L8"}},
	}}
	fa := &fakeDriveAttrs{}
	h := NewServer(Config{ConfigDir: dir, Changers: fc3, DriveAttributes: fa}).Handler()
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00003L8/attributes", body); rec.Code != 500 ||
		!strings.Contains(rec.Body.String(), "medium serial number") || len(fa.drives) != 0 {
		t.Errorf("no serial: %d %s, %d writes", rec.Code, rec.Body.String(), len(fa.drives))
	}
	for name, cfg := range map[string]Config{
		"no drive access": {ConfigDir: dir, Changers: fc},
		"no changer":      {ConfigDir: dir, DriveAttributes: &fakeDriveAttrs{}},
		"no inventory":    {ConfigDir: dir, Changers: &fakeChangers{invErr: errors.New("gone")}, DriveAttributes: &fakeDriveAttrs{}},
		"no such drive": {ConfigDir: dir, DriveAttributes: &fakeDriveAttrs{}, Changers: &fakeChangers{inv: changer.Inventory{
			Drives: []changer.DriveState{{Number: 99, Full: true, Barcode: "A00002L8"}},
		}}},
	} {
		if rec := doJSON(t, NewServer(cfg).Handler(), http.MethodPatch, path, body); rec.Code != 503 {
			t.Errorf("%s: %d %s, want 503", name, rec.Code, rec.Body.String())
		}
	}
}

// A device.conf with two stanzas of one library id lists it once, so the
// handlers that lock every library sharing a home do not take its lock
// twice and hang.
func TestLibrariesWithHomeOnce(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8")
	conf := filepath.Join(dir, "device.conf")
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?ms)^Library: 10 .*?\n\n`).Find(b)
	if m == nil {
		t.Fatal("no Library 10 stanza in the fixture")
	}
	if err := os.WriteFile(conf, append(b, append([]byte("\n"), m...)...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{ConfigDir: dir})
	inv, err := config.LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	var home string
	n := 0
	for _, l := range inv.Libraries {
		if l.ID == 10 {
			home, n = s.homeFor(l), n+1
		}
	}
	if n < 2 {
		t.Skipf("config keeps %d Library 10 stanza(s); nothing to deduplicate", n)
	}
	libs, err := s.librariesWithHome(home)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for _, l := range libs {
		if seen[l.ID]++; seen[l.ID] > 1 {
			t.Errorf("library %d listed twice", l.ID)
		}
	}
}
