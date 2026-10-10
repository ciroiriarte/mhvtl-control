// Package rawdisk reads mhvtl raw-disk (MVTL) superblocks and enumerates
// candidate block devices.
//
// The on-disk format is defined by struct mhvtl_raw_superblock in the C
// submodule (mhvtl/include/mhvtl_raw.h). The struct is __attribute__((packed)),
// so this reader lays out every field at an explicit byte offset using
// encoding/binary little-endian. The CRC is CRC-32C (Castagnoli) seeded with 0
// over the first header_len bytes, matching mhvtl/usr/utils/crc32c.c.
package rawdisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
)

// On-disk constants, mirrored from mhvtl/include/mhvtl_raw.h.
const (
	// MVTLMagic is MVTL_MAGIC ("MVTL", little-endian).
	MVTLMagic uint32 = 0x4D56544C
	// Version is MVTL_VERSION, the supported on-disk layout version.
	Version uint16 = 2
	// PrimaryOff is MVTL_SB_PRIMARY_OFF, the primary superblock byte offset.
	PrimaryOff int64 = 1 << 20
	// MirrorOff is MVTL_SB_MIRROR_OFF, the mirror superblock byte offset.
	MirrorOff int64 = 2 << 20

	// maxPartitions is MVTL_MAX_PARTITIONS.
	maxPartitions = 4
	// partitionSize is sizeof(struct mhvtl_raw_partition): 11 * uint64.
	partitionSize = 11 * 8
	// sbSize is sizeof(struct mhvtl_raw_superblock).
	sbSize = 480
	// crcFieldOff is offsetof(struct mhvtl_raw_superblock, header_crc32c),
	// i.e. MVTL_SB_CRC_LEN.
	crcFieldOff = 476
	// sbWindow is the number of bytes read from disk to cover one superblock.
	sbWindow = 4096
)

// Field byte offsets within the packed superblock.
const (
	offMagic         = 0                                     // uint32
	offHeaderLen     = 4                                     // uint32
	offVersion       = 8                                     // uint16
	offLogicalSector = 10                                    // uint16
	offCartridgeUUID = 12                                    // [16]byte
	offBarcode       = 28                                    // [32]char
	offVendorID      = 60                                    // [8]char
	offProductID     = 68                                    // [16]char
	offTotalCapacity = 84                                    // uint64
	offGenerationID  = 92                                    // uint64
	offNumPartitions = 100                                   // uint32
	offReserved0     = 104                                   // uint32
	offPart          = 108                                   // [maxPartitions]mhvtl_raw_partition
	offMAMOff        = offPart + maxPartitions*partitionSize // 460, uint64
	offMAMLen        = offMAMOff + 8                         // 468, uint64
	offHeaderCRC32C  = offMAMLen + 8                         // 476, uint32
)

// ErrNotFormatted is returned when no MVTL superblock is present (magic
// mismatch) at the probed offsets.
var ErrNotFormatted = errors.New("no MVTL superblock")

// ErrBadCRC is returned when a superblock has the MVTL magic but its CRC-32C
// does not match the stored header_crc32c.
var ErrBadCRC = errors.New("MVTL superblock CRC mismatch")

// ErrUnsupportedVersion is returned when a superblock's version is not Version.
var ErrUnsupportedVersion = errors.New("unsupported MVTL superblock version")

// Superblock is the decoded, host-friendly view of a mhvtl_raw_superblock.
// Fixed-width char arrays are trimmed to their NUL-terminated string.
type Superblock struct {
	Magic              uint32
	HeaderLen          uint32
	Version            uint16
	LogicalSector      uint16
	CartridgeUUID      [16]byte
	Barcode            string
	VendorID           string
	ProductID          string
	TotalCapacityBytes uint64
	GenerationID       uint64
	NumPartitions      uint32
	MAMOff             uint64
	MAMLen             uint64
	HeaderCRC32C       uint32
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// cstr trims a fixed-width C char array at its first NUL byte.
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// ParseSuperblock decodes and validates a superblock located at offset 0 of b.
//
// It returns ErrNotFormatted on a magic mismatch, ErrUnsupportedVersion on a
// version mismatch, and ErrBadCRC when the CRC-32C over the first HeaderLen
// bytes does not equal the stored header_crc32c.
func ParseSuperblock(b []byte) (*Superblock, error) {
	if len(b) < sbSize {
		return nil, fmt.Errorf("rawdisk: short superblock buffer: %d < %d", len(b), sbSize)
	}

	sb := &Superblock{
		Magic:              binary.LittleEndian.Uint32(b[offMagic:]),
		HeaderLen:          binary.LittleEndian.Uint32(b[offHeaderLen:]),
		Version:            binary.LittleEndian.Uint16(b[offVersion:]),
		LogicalSector:      binary.LittleEndian.Uint16(b[offLogicalSector:]),
		Barcode:            cstr(b[offBarcode : offBarcode+32]),
		VendorID:           cstr(b[offVendorID : offVendorID+8]),
		ProductID:          cstr(b[offProductID : offProductID+16]),
		TotalCapacityBytes: binary.LittleEndian.Uint64(b[offTotalCapacity:]),
		GenerationID:       binary.LittleEndian.Uint64(b[offGenerationID:]),
		NumPartitions:      binary.LittleEndian.Uint32(b[offNumPartitions:]),
		MAMOff:             binary.LittleEndian.Uint64(b[offMAMOff:]),
		MAMLen:             binary.LittleEndian.Uint64(b[offMAMLen:]),
		HeaderCRC32C:       binary.LittleEndian.Uint32(b[offHeaderCRC32C:]),
	}
	copy(sb.CartridgeUUID[:], b[offCartridgeUUID:offCartridgeUUID+16])

	if sb.Magic != MVTLMagic {
		return nil, ErrNotFormatted
	}
	if sb.Version != Version {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, sb.Version, Version)
	}
	// The C reader (sb_is_valid) requires header_len == MVTL_SB_CRC_LEN and
	// computes crc32c(0, s, header_len). Guard header_len before slicing.
	if sb.HeaderLen != crcFieldOff {
		return nil, fmt.Errorf("%w: header_len=%d, want %d", ErrBadCRC, sb.HeaderLen, crcFieldOff)
	}
	if crc32.Update(0, crc32cTable, b[:sb.HeaderLen]) != sb.HeaderCRC32C {
		return nil, ErrBadCRC
	}
	return sb, nil
}

// ReadSuperblock reads and validates the superblock of the cartridge at path.
// It tries the primary copy at PrimaryOff and, if that is absent or corrupt,
// falls back to the mirror at MirrorOff. Only the needed window is read.
func ReadSuperblock(path string) (*Superblock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sb, primaryErr := readSuperblockAt(f, PrimaryOff)
	if primaryErr == nil {
		return sb, nil
	}
	// Fall back to the mirror only on absence/corruption, not on I/O errors
	// like permission denied, which should surface to the caller.
	if errors.Is(primaryErr, ErrNotFormatted) || errors.Is(primaryErr, ErrBadCRC) ||
		errors.Is(primaryErr, ErrUnsupportedVersion) {
		if sb, mirrorErr := readSuperblockAt(f, MirrorOff); mirrorErr == nil {
			return sb, nil
		}
	}
	return nil, primaryErr
}

func readSuperblockAt(f *os.File, off int64) (*Superblock, error) {
	buf := make([]byte, sbWindow)
	n, err := f.ReadAt(buf, off)
	if n < sbSize {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("rawdisk: short read at offset %d: %d bytes", off, n)
	}
	return ParseSuperblock(buf)
}
