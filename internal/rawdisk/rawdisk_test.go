package rawdisk

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cWriterSrc writes a valid MVTL superblock using the SAME C header and
// crc32c implementation the kernel/userspace reader uses. This is the
// cross-language fixture: it proves the Go reader decodes a genuinely
// C-produced image and that Go's CRC-32C matches the C crc32c().
//
// argv: <out> <barcode> <vendor> <product> <capacity> <writeMirror 0|1>
const cWriterSrc = `
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <unistd.h>
#include <fcntl.h>
#include "mhvtl_raw.h"
#include "ccan/crc32c/crc32c.h"

int main(int argc, char **argv) {
    if (argc != 7) { fprintf(stderr, "bad argc\n"); return 2; }
    const char *out = argv[1];
    const char *barcode = argv[2];
    const char *vendor = argv[3];
    const char *product = argv[4];
    uint64_t capacity = strtoull(argv[5], NULL, 10);
    int write_mirror = atoi(argv[6]);

    struct mhvtl_raw_superblock sb;
    memset(&sb, 0, sizeof(sb));
    sb.magic = MVTL_MAGIC;
    sb.header_len = MVTL_SB_CRC_LEN;
    sb.version = MVTL_VERSION;
    sb.logical_sector = 512;
    for (int i = 0; i < 16; i++) sb.cartridge_uuid[i] = (uint8_t)(i + 1);
    strncpy(sb.barcode, barcode, sizeof(sb.barcode) - 1);
    strncpy(sb.vendor_id, vendor, sizeof(sb.vendor_id));
    strncpy(sb.product_id, product, sizeof(sb.product_id));
    sb.total_capacity_bytes = capacity;
    sb.generation_id = 7;
    sb.num_partitions = 1;
    sb.mam_off = MVTL_MAM_OFF;
    sb.mam_len = 0;
    sb.header_crc32c = crc32c(0, &sb, MVTL_SB_CRC_LEN);

    int fd = open(out, O_RDWR | O_CREAT | O_TRUNC, 0644);
    if (fd < 0) { perror("open"); return 1; }
    if (ftruncate(fd, (off_t)(4ull << 20)) != 0) { perror("ftruncate"); return 1; }
    if (pwrite(fd, &sb, sizeof(sb), (off_t)MVTL_SB_PRIMARY_OFF) != (ssize_t)sizeof(sb)) {
        perror("pwrite primary"); return 1;
    }
    if (write_mirror) {
        if (pwrite(fd, &sb, sizeof(sb), (off_t)MVTL_SB_MIRROR_OFF) != (ssize_t)sizeof(sb)) {
            perror("pwrite mirror"); return 1;
        }
    }
    close(fd);
    /* Emit the C-computed CRC so the Go test can assert equivalence. */
    printf("%u\n", sb.header_crc32c);
    return 0;
}
`

// mhvtlRepo locates the C submodule relative to this package.
func mhvtlRepo(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../../mhvtl")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "include", "mhvtl_raw.h")); err != nil {
		t.Skipf("mhvtl submodule not found at %s: %v", abs, err)
	}
	return abs
}

// buildCWriter compiles the C fixture against the real header + crc32c.c.
// Returns the writer binary path. Skips only if cc is genuinely absent.
func buildCWriter(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc not found; cannot build cross-language CRC fixture")
	}
	repo := mhvtlRepo(t)

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "sbwriter.c")
	if err := os.WriteFile(srcPath, []byte(cWriterSrc), 0644); err != nil {
		t.Fatalf("write C source: %v", err)
	}
	bin := filepath.Join(dir, "sbwriter")
	crcSrc := filepath.Join(repo, "usr", "utils", "crc32c.c")
	cmd := exec.Command(cc,
		"-O2", "-Wall",
		"-I", filepath.Join(repo, "include"),
		"-I", filepath.Join(repo, "ccan"),
		srcPath, crcSrc,
		"-o", bin,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling C fixture failed: %v\n%s", err, out)
	}
	return bin
}

// writeImage runs the C writer and returns the image path plus the
// C-computed CRC it printed.
func writeImage(t *testing.T, writer, barcode, vendor, product string, capacity uint64, mirror bool) (string, uint32) {
	t.Helper()
	img := filepath.Join(t.TempDir(), "cart.img")
	mstr := "0"
	if mirror {
		mstr = "1"
	}
	cmd := exec.Command(writer, img, barcode, vendor, product,
		strconv.FormatUint(capacity, 10), mstr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("C writer failed: %v\n%s", err, out)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
	if err != nil {
		t.Fatalf("parsing C CRC from %q: %v", out, err)
	}
	return img, uint32(v)
}

// TestCRCCrossLanguage is the key correctness proof: a superblock written by
// the C code (real crc32c.c) is decoded by Go, and Go's CRC-32C over the same
// header bytes equals the C-computed header_crc32c that the reader accepted.
func TestCRCCrossLanguage(t *testing.T) {
	writer := buildCWriter(t)

	const (
		barcode  = "VOL001L5"
		vendor   = "IBM"
		product  = "ULT3580-TD5"
		capacity = uint64(1500000000000)
	)
	img, cCRC := writeImage(t, writer, barcode, vendor, product, capacity, false)

	sb, err := ReadSuperblock(img)
	if err != nil {
		t.Fatalf("ReadSuperblock returned error (CRC or decode failure): %v", err)
	}
	if sb.Barcode != barcode {
		t.Errorf("barcode = %q, want %q", sb.Barcode, barcode)
	}
	if sb.VendorID != vendor {
		t.Errorf("vendor = %q, want %q", sb.VendorID, vendor)
	}
	if sb.ProductID != product {
		t.Errorf("product = %q, want %q", sb.ProductID, product)
	}
	if sb.TotalCapacityBytes != capacity {
		t.Errorf("capacity = %d, want %d", sb.TotalCapacityBytes, capacity)
	}

	// Independently recompute CRC-32C in Go over the exact same header bytes
	// the C writer covered, and assert it equals BOTH the stored field and the
	// value the C program printed. This is the explicit C/Go equivalence proof.
	raw := make([]byte, sbWindow)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.ReadAt(raw, PrimaryOff); err != nil {
		t.Fatal(err)
	}
	storedCRC := binary.LittleEndian.Uint32(raw[offHeaderCRC32C:])
	goCRC := crc32.Update(0, crc32.MakeTable(crc32.Castagnoli), raw[:crcFieldOff])

	t.Logf("CRC-32C evidence: C-printed=0x%08x stored-in-sb=0x%08x go-computed=0x%08x (header_len=%d)",
		cCRC, storedCRC, goCRC, sb.HeaderLen)

	if goCRC != cCRC {
		t.Fatalf("Go CRC 0x%08x != C CRC 0x%08x", goCRC, cCRC)
	}
	if goCRC != storedCRC {
		t.Fatalf("Go CRC 0x%08x != stored CRC 0x%08x", goCRC, storedCRC)
	}
	if sb.HeaderCRC32C != goCRC {
		t.Fatalf("decoded HeaderCRC32C 0x%08x != Go CRC 0x%08x", sb.HeaderCRC32C, goCRC)
	}
}

func TestNotFormatted(t *testing.T) {
	img := filepath.Join(t.TempDir(), "zero.img")
	if err := os.WriteFile(img, make([]byte, 4<<20), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSuperblock(img); err != ErrNotFormatted {
		t.Fatalf("ReadSuperblock on zero image = %v, want ErrNotFormatted", err)
	}
}

func TestMirrorFallback(t *testing.T) {
	writer := buildCWriter(t)
	img, _ := writeImage(t, writer, "MIRROR01", "IBM", "ULT3580-TD5", 500, true)

	// Corrupt the primary superblock's header region (break its CRC) while
	// leaving the mirror at MirrorOff intact.
	f, err := os.OpenFile(img, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the primary barcode field (offset covered by CRC).
	if _, err := f.WriteAt([]byte{0xFF}, PrimaryOff+int64(offBarcode)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Primary must now fail CRC on its own.
	pf, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := readSuperblockAt(pf, PrimaryOff); perr != ErrBadCRC {
		t.Fatalf("primary read after corruption = %v, want ErrBadCRC", perr)
	}
	pf.Close()

	// ReadSuperblock must transparently fall back to the mirror.
	sb, err := ReadSuperblock(img)
	if err != nil {
		t.Fatalf("ReadSuperblock mirror fallback failed: %v", err)
	}
	if sb.Barcode != "MIRROR01" {
		t.Fatalf("mirror barcode = %q, want %q", sb.Barcode, "MIRROR01")
	}
}

func TestListDisksScanPaths(t *testing.T) {
	writer := buildCWriter(t)
	img, _ := writeImage(t, writer, "SCAN0001", "IBM", "ULT3580-TD5", 1234, false)

	disks, err := ListDisks(Options{
		SysBlockDir: filepath.Join(t.TempDir(), "no-such-sysfs"),
		ScanPaths:   []string{img},
	})
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("got %d disks, want 1", len(disks))
	}
	d := disks[0]
	if !d.MVTLFormatted {
		t.Fatalf("ScanPaths disk not detected as MVTL-formatted")
	}
	if d.MVTL == nil || d.MVTL.Barcode != "SCAN0001" {
		t.Fatalf("disk barcode = %v, want SCAN0001", d.MVTL)
	}
	if d.Path != img {
		t.Fatalf("disk path = %q, want %q", d.Path, img)
	}
}
