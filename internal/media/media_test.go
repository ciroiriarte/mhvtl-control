package media

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// rec is one id/length/value record.
type rec struct {
	id  uint16
	val []byte
}

// encode writes records the way mhvtl write_mam does: host-order id and
// length, then the value bytes.
func encode(recs []rec) []byte {
	var b []byte
	for _, r := range recs {
		b = binary.NativeEndian.AppendUint16(b, r.id)
		b = binary.NativeEndian.AppendUint16(b, uint16(len(r.val)))
		b = append(b, r.val...)
	}
	return b
}

// mamFile prefixes the records with the two host-order format versions.
func mamFile(recs []rec) []byte {
	b := binary.NativeEndian.AppendUint32(nil, 3)
	b = binary.NativeEndian.AppendUint32(b, 2)
	return append(b, encode(recs)...)
}

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func flags(f uint16) []byte { return binary.NativeEndian.AppendUint16(nil, f) }

// padded returns s in an n-byte field padded with pad.
func padded(s string, n int, pad byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = pad
	}
	copy(b, s)
	return b
}

// writeCart creates <home>/<pcl>/ with the given files; nil skips a file.
func writeCart(t *testing.T, home, pcl string, mam, vtl []byte) {
	t.Helper()
	dir := filepath.Join(home, pcl)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if mam != nil {
		if err := os.WriteFile(filepath.Join(dir, "mam"), mam, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if vtl != nil {
		if err := os.WriteFile(filepath.Join(dir, "mhvtl_data"), vtl, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func typeMAM(t byte) []byte {
	return mamFile([]rec{{attrMediumType, []byte{t}}})
}

func TestReadDataCartridge(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{
		{0x0000, u64(1000)},
		{0x0001, u64(2500000000000)},
		{0x0002, u64(0)},
		{0x0003, u64(7)},
		{0x0220, u64(123456)},
		{0x0221, u64(654321)},
		{0x0401, padded("SN123", 32, ' ')},
		{0x0408, []byte{0}},
		{0x0804, padded("202610151200", 12, ' ')},
		{0x0806, padded("OTHER", 32, ' ')},
	})
	vtl := encode([]rec{
		{0x01, []byte{0}},
		{0x02, flags(0)},
		{0x03, []byte{4}},
		{0x04, []byte{2}},
		{0x05, []byte{17}},
		{0x08, padded("LTO6", 8, 0)},
		{0x09, padded("Ultrium 6/16T", 32, 0)},
	})
	writeCart(t, home, "E01001L6", mam, vtl)

	c, err := Read(home, "E01001L6")
	if err != nil {
		t.Fatal(err)
	}
	want := Cartridge{
		Barcode:        "E01001L6",
		Backend:        "file",
		Type:           "data",
		Density:        "LTO6",
		Description:    "Ultrium 6/16T",
		CapacityBytes:  2500000000000,
		RemainingBytes: 1000,
		LoadCount:      7,
		WrittenBytes:   123456,
		ReadBytes:      654321,
		Partitions:     2,
		LastWritten:    "202610151200",
	}
	if c != want {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
}

func TestMediumTypes(t *testing.T) {
	for _, tc := range []struct {
		t    byte
		want string
	}{{1, "WORM"}, {2, "NULL"}, {6, "clean"}, {9, "unknown"}} {
		home := t.TempDir()
		writeCart(t, home, "A00001", typeMAM(tc.t), []byte{})
		c, err := Read(home, "A00001")
		if err != nil {
			t.Fatal(err)
		}
		if c.Type != tc.want || c.Error != "" {
			t.Errorf("type %d: got %q error %q, want %q", tc.t, c.Type, c.Error, tc.want)
		}
	}
}

func TestWriteProtected(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "WP0001", typeMAM(0), encode([]rec{{0x02, flags(0x0003)}}))
	c, _ := Read(home, "WP0001")
	if !c.WriteProtected {
		t.Fatalf("not write protected: %+v", c)
	}

	writeCart(t, home, "RW0001", typeMAM(0), encode([]rec{{0x02, flags(0x0001)}}))
	c, _ = Read(home, "RW0001")
	if c.WriteProtected {
		t.Fatalf("encryption-format flag read as write protect: %+v", c)
	}
}

func TestUnknownAttributesSkipped(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{
		{0x0003, u64(1)},
		{0x0abc, []byte{1, 2, 3, 4, 5}},
		{0x1500, append([]byte{0x01}, "label"...)}, // vendor: format byte + value
		{0x1623, []byte{0}},
		{0x0408, []byte{1}},
		{0x0003, u64(5)}, // later record wins
	})
	vtl := encode([]rec{
		{0x0a, make([]byte, 70)},
		{0x77, nil}, // zero length
		{0x08, padded("LTO8", 8, 0)},
	})
	writeCart(t, home, "U00001", mam, vtl)
	c, _ := Read(home, "U00001")
	if c.Error != "" || c.Type != "WORM" || c.LoadCount != 5 || c.Density != "LTO8" {
		t.Fatalf("got %+v", c)
	}
}

func TestZeroLengthMhvtlRecord(t *testing.T) {
	home := t.TempDir()
	vtl := encode([]rec{{0x01, nil}, {0x04, []byte{1}}, {0x09, nil}})
	writeCart(t, home, "Z00001", typeMAM(0), vtl)
	c, _ := Read(home, "Z00001")
	if c.Error != "" || c.Partitions != 1 || c.Description != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestTruncatedRecord(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{{0x0003, u64(9)}, {0x0408, []byte{0}}})
	mam = binary.NativeEndian.AppendUint16(mam, 0x0001)
	mam = binary.NativeEndian.AppendUint16(mam, 8)
	mam = append(mam, 0, 0, 1) // 3 of 8 bytes
	writeCart(t, home, "T00001", mam, encode([]rec{{0x08, padded("LTO5", 8, 0)}}))
	c, err := Read(home, "T00001")
	if err != nil {
		t.Fatal(err)
	}
	if c.Error == "" || c.LoadCount != 9 || c.Type != "data" || c.CapacityBytes != 0 || c.Density != "LTO5" {
		t.Fatalf("got %+v", c)
	}

	// A dangling id with no length.
	writeCart(t, home, "T00002", typeMAM(0), append(encode([]rec{{0x04, []byte{3}}}), 0x08))
	c, _ = Read(home, "T00002")
	if c.Error == "" || c.Partitions != 3 {
		t.Fatalf("got %+v", c)
	}
}

func TestShortMAMHeader(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "S00001", []byte{1, 2, 3}, nil)
	c, _ := Read(home, "S00001")
	if c.Error == "" {
		t.Fatalf("no error for short header: %+v", c)
	}
}

func TestNoMhvtlData(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "M00001", typeMAM(1), nil)
	c, _ := Read(home, "M00001")
	if c.Error != "no mhvtl_data file" || c.Type != "WORM" {
		t.Fatalf("got %+v", c)
	}
}

func TestNoMAM(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "N00001", nil, nil)
	c, err := Read(home, "N00001")
	if err != nil {
		t.Fatal(err)
	}
	if c.Error != "no mam file" || c.Backend != "file" {
		t.Fatalf("got %+v", c)
	}
}

func TestRawSymlink(t *testing.T) {
	home := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(home, "R00001")); err != nil {
		t.Fatal(err)
	}
	c, err := Read(home, "R00001")
	if err != nil {
		t.Fatal(err)
	}
	if c != (Cartridge{Barcode: "R00001", Backend: "raw"}) {
		t.Fatalf("got %+v", c)
	}
}

func TestList(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "C00003", typeMAM(0), nil)
	writeCart(t, home, "A00001", typeMAM(1), nil)
	writeCart(t, home, "B00002", nil, nil)
	writeCart(t, home, "lost+found", nil, nil)
	writeCart(t, home, ".hidden", nil, nil)
	writeCart(t, home, "TOOLONGBARCODE0001", nil, nil)
	if err := os.WriteFile(filepath.Join(home, "PLAIN1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(home, "D00004")); err != nil {
		t.Fatal(err)
	}

	carts, err := List(home)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range carts {
		got = append(got, c.Barcode+":"+c.Backend)
	}
	want := []string{"A00001:file", "B00002:file", "C00003:file", "D00004:raw"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if carts[0].Type != "WORM" || carts[1].Error != "no mam file" {
		t.Fatalf("got %+v", carts)
	}

	if _, err := List(filepath.Join(home, "missing")); err == nil {
		t.Fatal("List on missing home succeeded")
	}
}

func TestReadMissing(t *testing.T) {
	home := t.TempDir()
	for _, pcl := range []string{"X00001", "../etc", ""} {
		if _, err := Read(home, pcl); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Read(%q) = %v, want ErrNotExist", pcl, err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "PLAIN1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home, "PLAIN1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read on plain file = %v, want ErrNotExist", err)
	}
}

func TestExists(t *testing.T) {
	home := t.TempDir()
	writeCart(t, home, "E00001", nil, nil)
	if err := os.Symlink("/nonexistent/dev", filepath.Join(home, "E00002")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "PLAIN1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for pcl, want := range map[string]bool{
		"E00001": true, "E00002": true, "PLAIN1": false, "E00003": false, "../x": false,
	} {
		if got := Exists(home, pcl); got != want {
			t.Errorf("Exists(%q) = %v, want %v", pcl, got, want)
		}
	}
}
