package media

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mhvtl-control/internal/rawdisk"
)

// rawPart is one partition entry of a test image.
type rawPart struct{ dataCap, dataUsed, advertised uint64 }

// rawImage builds a raw cartridge image the way mhvtl usr/vtlcart_raw.c lays
// it out: the packed superblock (mhvtl include/mhvtl_raw.h) at
// rawdisk.PrimaryOff and the MAM region at mamOff. mam nil leaves mam_len 0,
// as on a freshly formatted cartridge.
func rawImage(t *testing.T, barcode string, gen uint64, parts []rawPart, mam []byte) []byte {
	t.Helper()
	const (
		mamOff  = 3 << 20
		partOff = 108
		partLen = 11 * 8
		crcOff  = 476
	)
	img := make([]byte, mamOff+len(mam))
	sb := img[rawdisk.PrimaryOff:]
	le := binary.LittleEndian
	le.PutUint32(sb[0:], rawdisk.MVTLMagic)
	le.PutUint32(sb[4:], crcOff)
	le.PutUint16(sb[8:], rawdisk.Version)
	le.PutUint16(sb[10:], 512)
	for i := range 16 {
		sb[12+i] = byte(i + 1)
	}
	copy(sb[28:60], barcode)
	copy(sb[60:68], "IBM")
	copy(sb[68:84], "ULT3580-TD8")
	le.PutUint64(sb[84:], 1<<40)
	le.PutUint64(sb[92:], gen)
	le.PutUint32(sb[100:], uint32(len(parts)))
	for i, p := range parts {
		e := sb[partOff+i*partLen:]
		le.PutUint64(e[8:], p.dataCap)
		le.PutUint64(e[16:], p.dataUsed)
		le.PutUint64(e[80:], p.advertised)
	}
	le.PutUint64(sb[460:], mamOff)
	le.PutUint64(sb[468:], uint64(len(mam)))
	le.PutUint32(sb[crcOff:], crc32.Update(0, crc32.MakeTable(crc32.Castagnoli), sb[:crcOff]))
	copy(img[mamOff:], mam)
	return img
}

// rawMAM is the MAM region container: host-order u32 lengths of the mam and
// mhvtl_data streams, then the streams.
func rawMAM(mam, vtl []byte) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(len(mam)))
	b = binary.NativeEndian.AppendUint32(b, uint32(len(vtl)))
	b = append(b, mam...)
	return append(b, vtl...)
}

// linkRaw writes img to a file standing in for the block device and links
// <home>/<barcode> to it, as the bay monitor does.
func linkRaw(t *testing.T, home, barcode string, img []byte) string {
	t.Helper()
	dev := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(dev, img, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(home, barcode)); err != nil {
		t.Fatal(err)
	}
	return dev
}

func TestRawCartridge(t *testing.T) {
	home := t.TempDir()
	mam := mamFile([]rec{
		{attrMaxCapacity, u64(12_000_000_000_000)},
		{attrRemainingCapacity, u64(11_000_000_000_000)},
		{attrLoadCount, u64(7)},
		{attrWrittenLife, u64(1_000_000_000)},
		{attrMediumType, []byte{1}},
	})
	vtl := encode([]rec{
		{vtlFlags, flags(flagWriteProtect)},
		{vtlDensityName, padded("U-832", 8, 0)},
	})
	parts := []rawPart{{dataCap: 900 << 30, dataUsed: 5 << 30, advertised: 800 << 30}}
	linkRaw(t, home, "R00001L8", rawImage(t, "R00001L8", 42, parts, rawMAM(mam, vtl)))

	c, err := Read(home, "R00001L8")
	if err != nil {
		t.Fatal(err)
	}
	want := Cartridge{
		Barcode:        "R00001L8",
		Backend:        BackendRaw,
		Type:           "WORM",
		Density:        "U-832",
		CapacityBytes:  12_000_000_000_000, // the MAM's figure wins over the partition table
		RemainingBytes: 11_000_000_000_000,
		WriteProtected: true,
		LoadCount:      7,
		WrittenBytes:   1_000_000_000,
		Partitions:     1,
		UUID:           "01020304-0506-0708-090a-0b0c0d0e0f10",
		GenerationID:   42,
		DeviceBytes:    1 << 40,
		UsedBytes:      5 << 30,
	}
	if c != want {
		t.Fatalf("got  %+v\nwant %+v", c, want)
	}
}

func TestRawFreshlyFormatted(t *testing.T) {
	home := t.TempDir()
	parts := []rawPart{{dataCap: 900, advertised: 800}, {dataCap: 90, dataUsed: 3, advertised: 80}}
	linkRaw(t, home, "R00002", rawImage(t, "R00002", 1, parts, nil))

	c, err := Read(home, "R00002")
	if err != nil {
		t.Fatal(err)
	}
	if c.Error != "" || c.CapacityBytes != 880 || c.UsedBytes != 3 || c.Partitions != 2 || c.GenerationID != 1 {
		t.Fatalf("got %+v", c)
	}
}

func TestRawDanglingSymlink(t *testing.T) {
	home := t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(home, "R00003")); err != nil {
		t.Fatal(err)
	}
	c, err := Read(home, "R00003")
	if err != nil {
		t.Fatal(err)
	}
	if c != (Cartridge{Barcode: "R00003", Backend: BackendRaw, MediaAbsent: true}) {
		t.Fatalf("got %+v", c)
	}
}

func TestRawBadCRC(t *testing.T) {
	home := t.TempDir()
	img := rawImage(t, "R00004", 1, nil, nil)
	img[rawdisk.PrimaryOff+28] = 'X' // barcode byte, covered by the CRC
	dev := linkRaw(t, home, "R00004", img)

	c, err := Read(home, "R00004")
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != BackendRaw || !strings.HasPrefix(c.Error, "superblock: ") ||
		!strings.Contains(c.Error, "CRC") || strings.Contains(c.Error, dev) {
		t.Fatalf("got %+v", c)
	}
}

func TestRawNotFormatted(t *testing.T) {
	home := t.TempDir()
	linkRaw(t, home, "R00005", make([]byte, 4<<20))
	c, err := Read(home, "R00005")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Error, "no MVTL superblock") {
		t.Fatalf("got %+v", c)
	}
}

func TestRawTruncatedMAM(t *testing.T) {
	home := t.TempDir()
	region := rawMAM(mamFile(nil), nil)
	binary.NativeEndian.PutUint32(region[4:], 1000) // mhvtl_data runs past the region
	linkRaw(t, home, "R00006", rawImage(t, "R00006", 1, nil, region))

	c, err := Read(home, "R00006")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Error, "truncated") {
		t.Fatalf("got %+v", c)
	}
}

func TestRawBrokenTargetPath(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A path component of the target is a regular file: ENOTDIR, not ENOENT.
	if err := os.Symlink(filepath.Join(file, "disk"), filepath.Join(home, "R00007")); err != nil {
		t.Fatal(err)
	}
	c, err := Read(home, "R00007")
	if err != nil {
		t.Fatal(err)
	}
	if c != (Cartridge{Barcode: "R00007", Backend: BackendRaw, MediaAbsent: true}) {
		t.Fatalf("got %+v", c)
	}
}
