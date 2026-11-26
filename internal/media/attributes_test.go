package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"mhvtl-control/internal/scsi"
)

// vendorRec is a host vendor attribute record as write_mam stores it: the
// format byte ahead of the value.
func vendorRec(id uint16, format byte, val []byte) rec {
	return rec{id, append([]byte{format}, val...)}
}

func byID(t *testing.T, set AttributeSet) map[string]Attribute {
	t.Helper()
	m := map[string]Attribute{}
	for _, a := range set.Attributes {
		if _, dup := m[a.ID]; dup {
			t.Errorf("attribute %s reported twice", a.ID)
		}
		m[a.ID] = a
	}
	return m
}

func TestAttributesFile(t *testing.T) {
	home := t.TempDir()
	coh := make([]byte, 0x46)
	coh[0] = 8
	binary.BigEndian.PutUint64(coh[1:], 5)
	binary.BigEndian.PutUint64(coh[9:], 3)
	binary.BigEndian.PutUint64(coh[17:], 0x77)
	binary.BigEndian.PutUint16(coh[25:], 4)
	copy(coh[27:], "LTFS")
	mam := mamFile([]rec{
		{0x0000, u64(1000)},
		{0x0002, u64(0x41)},
		{0x0003, u64(7)},
		{0x0007, []byte{0, 2}},
		{0x0401, padded("SN123", 32, ' ')},
		{0x0408, []byte{1}},
		{0x0409, binary.NativeEndian.AppendUint16(nil, 20)}, // host order (mktape.c:207)
		{0x0801, padded("  Backup", 32, ' ')}, // leading spaces are kept
		{0x0808, padded("Pool A", 160, 0)},
		{0x0805, []byte{0}},
		{0x080c, coh},
		{0x1623, []byte{0}},
		vendorRec(0x1400, scsi.AttrASCII, []byte("LABEL")),
		{0x0c01, []byte{0xab}}, // device vendor specific, not in the table
	})
	nonce := binary.NativeEndian.AppendUint64(nil, 99)
	vtl := encode([]rec{
		{0x01, []byte{0}},
		{0x02, flags(flagWriteProtect)},
		{0x06, binary.BigEndian.AppendUint32(nil, 19107)},
		{0x08, padded("LTO8", 8, 0)},
		{0x0d, u64(12 << 40)},
		{0x11, bytes.Repeat([]byte{0x5a}, 32)},
		{0x12, nonce},
		{0x30, []byte{1, 2}},
	})
	writeCart(t, home, "E01001L8", mam, vtl)

	set, err := Attributes(home, "E01001L8")
	if err != nil {
		t.Fatal(err)
	}
	if set.Error != "" || set.Backend != BackendFile || set.TapeFormatVersion != 3 || set.MAMFormatVersion != 2 {
		t.Errorf("set = %+v", set)
	}
	got := byID(t, set)
	want := map[string]Attribute{
		"0x0000": {Name: "Remaining capacity (bytes, whole medium)", Group: GroupDevice, Format: "binary", ReadOnly: true, Length: 8, Value: "1000"},
		"0x0002": {Group: GroupDevice, Format: "binary", ReadOnly: true, Length: 8, Value: "0000000000000041"},
		"0x0007": {Group: GroupDevice, Format: "binary", ReadOnly: true, Length: 2, Value: "2"},
		"0x0401": {Group: GroupMedium, Format: "ascii", ReadOnly: true, Length: 32, Value: "SN123"},
		"0x0408": {Group: GroupMedium, Format: "binary", Length: 1, Value: "WORM"},
		"0x0409": {Group: GroupMedium, Format: "binary", ReadOnly: true, Length: 2, Value: "20"},
		"0x0801": {Group: GroupHost, Format: "ascii", Editable: true, Length: 32, Value: "  Backup"},
		"0x0808": {Group: GroupHost, Format: "text", Editable: true, Length: 160, Value: "Pool A"},
		"0x0805": {Group: GroupHost, Format: "binary", Length: 1, Value: "0"},
		"0x080c": {Group: GroupHost, Format: "binary", Length: 0x46,
			Value: `volume change reference 0x0000000000000005, count 3, set 0x77, application information "LTFS"`},
		"0x1623": {Group: GroupHostVendor, Format: "binary", Length: 1, Value: "0"},
		"0x1400": {Group: GroupHostVendor, Format: "ascii", Length: 5, Value: "LABEL"},
		"0x0c01": {Group: GroupDeviceVendor, Format: "binary", Length: 1, Value: "ab"},
		"0x01":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 1, Value: "0"},
		"0x02":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 2, Value: "2"},
		"0x06":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 4, Value: "19107"},
		"0x08":   {Group: GroupMhvtl, Format: "ascii", ReadOnly: true, Length: 8, Value: "LTO8"},
		"0x0d":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 8, Value: strconv.Itoa(12 << 40)},
		"0x12":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 8, Value: "99"},
		"0x30":   {Group: GroupMhvtl, Format: "binary", ReadOnly: true, Length: 2, Value: "0102"},
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if w.Name == "" {
			w.Name = g.Name
		}
		w.ID, w.Raw = id, g.Raw
		if g != w {
			t.Errorf("%s:\n got  %+v\n want %+v", id, g, w)
		}
	}
	if _, ok := got["0x11"]; ok {
		t.Error("GCM salt reported")
	}
	if g := got["0x0801"]; g.Raw != "20204261636b7570"+strings.Repeat("20", 24) {
		t.Errorf("raw = %s", g.Raw)
	}
	if g := got["0x0c01"]; g.Name != "" {
		t.Errorf("unknown attribute named %q", g.Name)
	}
}

func TestAttributesTruncatedTail(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{{0x0801, padded("App", 32, ' ')}})
	mam = binary.NativeEndian.AppendUint16(mam, 0x0808)
	mam = binary.NativeEndian.AppendUint16(mam, 160)
	mam = append(mam, "short"...)
	writeCart(t, home, "E01002L8", mam, encode([]rec{{0x04, []byte{1}}}))
	set, err := Attributes(home, "E01002L8")
	if err != nil {
		t.Fatal(err)
	}
	got := byID(t, set)
	if !strings.Contains(set.Error, "mam: attribute 0x0808: truncated") || got["0x0801"].Value != "App" || got["0x04"].Value != "1" {
		t.Errorf("set = %+v", set)
	}
}

func TestAttributesMissing(t *testing.T) {
	home := t.TempDir()
	if _, err := Attributes(home, "NOPE01"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := Attributes(home, "../etc"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bad barcode: err = %v", err)
	}
	if err := os.Mkdir(filepath.Join(home, "EMPTY1"), 0o755); err != nil {
		t.Fatal(err)
	}
	set, err := Attributes(home, "EMPTY1")
	if err != nil || set.Error != "open mam: no such file or directory" || len(set.Attributes) != 0 {
		t.Errorf("no mam: %+v, %v", set, err)
	}
}

func TestAttributesRaw(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{{0x0806, padded("R00001L8", 32, ' ')}, {0x0408, []byte{0}}})
	vtl := encode([]rec{{0x04, []byte{2}}})
	linkRaw(t, home, "R00001L8", rawImage(t, "R00001L8", 1, []rawPart{{dataCap: 1 << 30}}, rawMAM(mam, vtl)))
	set, err := Attributes(home, "R00001L8")
	if err != nil {
		t.Fatal(err)
	}
	got := byID(t, set)
	if set.Backend != BackendRaw || set.Error != "" || got["0x0806"].Value != "R00001L8" || got["0x0408"].Value != "data" || got["0x04"].Value != "2" {
		t.Errorf("set = %+v", set)
	}

	// A disk pulled from its bay is not an error, nor is a fresh format.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(home, "R00002L8")); err != nil {
		t.Fatal(err)
	}
	if set, err := Attributes(home, "R00002L8"); err != nil || set.Error != "disk not attached" {
		t.Errorf("dangling: %+v, %v", set, err)
	}
	linkRaw(t, home, "R00003L8", rawImage(t, "R00003L8", 1, nil, nil))
	if set, err := Attributes(home, "R00003L8"); err != nil || set.Error != "" || len(set.Attributes) != 0 {
		t.Errorf("fresh: %+v, %v", set, err)
	}
}

func TestEditableAttributes(t *testing.T) {
	var ids []string
	for _, d := range EditableAttributes() {
		ids = append(ids, strconv.FormatUint(uint64(d.ID), 16))
	}
	if got := strings.Join(ids, " "); got != "800 801 802 803 804 806 807 808 80b" {
		t.Errorf("editable = %s", got)
	}
	for _, id := range []uint16{0x0408, 0x0805, 0x080c, 0x1623, 0x1400, 0x0401, 0x0000} {
		if _, ok := EditableAttribute(id); ok {
			t.Errorf("0x%04x editable", id)
		}
	}
}

func TestPad(t *testing.T) {
	name, _ := EditableAttribute(0x0801)
	b, err := name.Pad("Bareos", false)
	if err != nil || !bytes.Equal(b, padded("Bareos", 32, ' ')) {
		t.Errorf("ascii: % x, %v", b, err)
	}
	pool, _ := EditableAttribute(0x0808)
	b, err = pool.Pad("Pool é", true)
	if err != nil || !bytes.Equal(b, padded("Pool é", 160, 0)) {
		t.Errorf("text: % x, %v", b, err)
	}
	if b, err := name.Pad("", false); err != nil || !bytes.Equal(b, padded("", 32, ' ')) {
		t.Errorf("empty clears: % x, %v", b, err)
	}
	for _, c := range []struct {
		def   AttributeDef
		value string
	}{
		{name, strings.Repeat("x", 33)},
		{name, "tab\there"},
		{name, "café"},
		{pool, "nul\x00"},
		{pool, "\xff"},
		{pool, strings.Repeat("é", 81)},
		{staticByID[0x0805], "0"},
	} {
		if _, err := c.def.Pad(c.value, true); err == nil {
			t.Errorf("0x%04x %q accepted", c.def.ID, c.value)
		}
	}
	// Without a UTF-8 localization identifier TEXT is ASCII.
	if _, err := pool.Pad("Pool é", false); err == nil {
		t.Error("non-ASCII text accepted with identifier 00h")
	}
	if b, err := pool.Pad("Pool", false); err != nil || !bytes.Equal(b, padded("Pool", 160, 0)) {
		t.Errorf("ascii text: % x, %v", b, err)
	}
	set := AttributeSet{Attributes: []Attribute{{ID: "0x0805", Raw: "81"}}}
	if !set.TextIsUTF8() || (AttributeSet{Attributes: []Attribute{{ID: "0x0805", Raw: "00"}}}).TextIsUTF8() {
		t.Error("TextIsUTF8")
	}
}

func TestParseAttributeID(t *testing.T) {
	for s, want := range map[string]uint16{"0x0801": 0x0801, "0X80B": 0x080b, "0x1623": 0x1623} {
		if got, err := ParseAttributeID(s); err != nil || got != want {
			t.Errorf("%s = %#x, %v", s, got, err)
		}
	}
	for _, s := range []string{"0801", "801", "0x", "0x10000", "0xzz", ""} {
		if _, err := ParseAttributeID(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// mhvtlRepo locates the mhVTL source: $MHVTL_SRC, else the sibling submodule
// of the superproject.
func mhvtlRepo(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MHVTL_SRC")
	if src == "" {
		src = "../../../mhvtl"
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "usr", "vtllib.c")); err != nil {
		t.Skipf("mhvtl submodule not found at %s: %v", abs, err)
	}
	return abs
}

var initMAMAttrRe = regexp.MustCompile(`^\s*INIT_MAM_ATTR\((0x[0-9a-fA-F]+),\s*(\w+),\s*([01]),\s*([012]),`)

// TestStaticAttributesMatchMhvtl compares staticAttributes with the
// INIT_MAM_ATTR lines of init_mam in mhVTL's usr/vtllib.c: same ids in the
// same order, with the same length, read-only flag and format.
func TestStaticAttributesMatchMhvtl(t *testing.T) {
	root := mhvtlRepo(t)
	b, err := os.ReadFile(filepath.Join(root, "usr", "vtllib.c"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := os.ReadFile(filepath.Join(root, "include", "vtllib.h"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^#define MAM_COHERENCY_LEN (0x[0-9a-fA-F]+)`).FindSubmatch(h)
	if m == nil {
		t.Fatal("include/vtllib.h: no MAM_COHERENCY_LEN")
	}
	cohLen, _ := strconv.ParseUint(string(m[1]), 0, 16)
	consts := map[string]int{"MAM_COHERENCY_LEN": int(cohLen)}
	var c []AttributeDef
	for i, l := range strings.Split(string(b), "\n") {
		m := initMAMAttrRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		id, _ := strconv.ParseUint(m[1], 0, 16)
		n, err := strconv.Atoi(m[2])
		if err != nil {
			var ok bool
			if n, ok = consts[m[2]]; !ok {
				t.Fatalf("usr/vtllib.c:%d: unknown length %s", i+1, m[2])
			}
		}
		f, _ := strconv.Atoi(m[4])
		c = append(c, AttributeDef{ID: uint16(id), Length: n, ReadOnly: m[3] == "1", Format: uint8(f)})
	}
	if len(c) != len(staticAttributes) {
		t.Fatalf("usr/vtllib.c has %d static attributes, the Go table %d", len(c), len(staticAttributes))
	}
	for i, d := range staticAttributes {
		d.Name = ""
		if d != c[i] {
			t.Errorf("entry %d: Go %+v, C %+v", i, d, c[i])
		}
	}
}
