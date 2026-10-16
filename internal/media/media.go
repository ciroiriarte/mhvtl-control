// Package media reads mhVTL's on-disk cartridge metadata. Each cartridge
// (PCL, i.e. barcode) is a directory <home>/<PCL>/ holding the data, index
// and meta streams plus the MAM in two files: mam (SCSI MAM attributes) and
// mhvtl_data (mhVTL private attributes), written by mhvtl usr/vtlcart.c
// write_mam. A cartridge on the raw-disk backend is instead a symlink to a
// block device holding an MVTL superblock and a MAM region with the same two
// streams (mhvtl usr/vtlcart_raw.c raw_be_save_mam); it is opened read-only.
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"mhvtl-control/internal/rawdisk"
)

// Backends.
const (
	BackendFile = "file"
	BackendRaw  = "raw"
)

// maxFileSize caps how much of a metadata file is read; real ones are a few
// KiB.
const maxFileSize = 1 << 20

// SCSI MAM attribute ids (SPC-4 table 334) that are reported. Their values
// are stored as on the wire: numbers big-endian, strings space/NUL padded.
const (
	attrRemainingCapacity = 0x0000
	attrMaxCapacity       = 0x0001
	attrLoadCount         = 0x0003
	attrWrittenLife       = 0x0220
	attrReadLife          = 0x0221
	attrMediumType        = 0x0408
	attrLastWritten       = 0x0804
)

// mhVTL private attribute ids (mhvtl usr/vtllib.c init_mam). Values are
// copied from host-order struct fields.
const (
	vtlFlags         = 0x02
	vtlNumPartitions = 0x04
	vtlDensityName   = 0x08
	vtlDescription   = 0x09
)

// flagWriteProtect is MAM_FLAGS_MEDIA_WRITE_PROTECT in the mhVTL Flags word.
const flagWriteProtect = 0x0002

// mamHeaderLen is the two u32 format versions ahead of the mam records.
const mamHeaderLen = 8

// Cartridge is one virtual cartridge as recorded on disk.
type Cartridge struct {
	Barcode        string `json:"barcode"`
	Backend        string `json:"backend"`
	Type           string `json:"type,omitempty"` // data, WORM, NULL, clean, unknown
	Density        string `json:"density,omitempty"`
	Description    string `json:"description,omitempty"`
	CapacityBytes  uint64 `json:"capacity_bytes,omitempty"`
	RemainingBytes uint64 `json:"remaining_bytes,omitempty"` // as of the last MAM rewrite
	WriteProtected bool   `json:"write_protected"`
	LoadCount      uint64 `json:"load_count"`
	WrittenBytes   uint64 `json:"written_bytes"`
	ReadBytes      uint64 `json:"read_bytes"`
	Partitions     int    `json:"partitions,omitempty"`
	LastWritten    string `json:"last_written,omitempty"`

	// Raw backend only, from the MVTL superblock.
	UUID         string `json:"uuid,omitempty"`
	GenerationID uint64 `json:"generation_id,omitempty"`
	DeviceBytes  uint64 `json:"device_bytes,omitempty"` // total_capacity_bytes
	UsedBytes    uint64 `json:"used_bytes,omitempty"`   // payload bytes on the disk
	MediaAbsent  bool   `json:"media_absent,omitempty"` // the symlink's disk is not attached

	Error string `json:"error,omitempty"` // metadata unreadable; other fields best effort
}

// Exists reports whether <home>/<barcode> is a cartridge directory or a raw
// device symlink.
func Exists(home, barcode string) bool {
	if !validBarcode(barcode) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(home, barcode))
	return err == nil && isCartridge(fi)
}

// Read returns the cartridge <home>/<barcode>. A missing cartridge yields an
// error wrapping os.ErrNotExist; unreadable metadata is reported in
// Cartridge.Error instead.
func Read(home, barcode string) (Cartridge, error) {
	if !validBarcode(barcode) {
		return Cartridge{}, fmt.Errorf("cartridge %q: %w", barcode, os.ErrNotExist)
	}
	fi, err := os.Lstat(filepath.Join(home, barcode))
	if err != nil {
		return Cartridge{}, fmt.Errorf("cartridge %s: %w", barcode, err)
	}
	if !isCartridge(fi) {
		return Cartridge{}, fmt.Errorf("cartridge %s: not a directory or symlink: %w", barcode, os.ErrNotExist)
	}
	return read(home, fi), nil
}

// List returns every cartridge under home, sorted by barcode. Entries whose
// name is not a plausible barcode are skipped.
func List(home string) ([]Cartridge, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("read media home: %w", err)
	}
	carts := []Cartridge{}
	for _, e := range entries {
		if !validBarcode(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !isCartridge(fi) {
			continue
		}
		carts = append(carts, read(home, fi))
	}
	sort.Slice(carts, func(i, j int) bool { return carts[i].Barcode < carts[j].Barcode })
	return carts, nil
}

// validBarcode reports whether name is 1-16 ASCII letters and digits.
func validBarcode(name string) bool {
	if len(name) == 0 || len(name) > 16 {
		return false
	}
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// isCartridge reports whether an Lstat result is a cartridge directory or a
// raw backend symlink.
func isCartridge(fi os.FileInfo) bool {
	return fi.IsDir() || fi.Mode()&os.ModeSymlink != 0
}

// read decodes the cartridge described by fi, an Lstat of <home>/<name>.
func read(home string, fi os.FileInfo) Cartridge {
	c := Cartridge{Barcode: fi.Name(), Backend: BackendFile}
	if fi.Mode()&os.ModeSymlink != 0 {
		c.Backend = BackendRaw
		readRaw(&c, filepath.Join(home, fi.Name()))
		return c
	}
	dir := filepath.Join(home, fi.Name())

	mam, err := readFile(filepath.Join(dir, "mam"))
	if errors.Is(err, os.ErrNotExist) {
		c.Error = "no mam file"
		return c
	}
	if err != nil {
		c.Error = err.Error()
		return c
	}
	if err := parseMAM(&c, mam); err != nil {
		c.Error = fmt.Sprintf("mam: %v", err)
	}

	vtl, err := readFile(filepath.Join(dir, "mhvtl_data"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = errors.New("no mhvtl_data file")
	case err == nil:
		if err = parseMhvtl(&c, vtl); err != nil {
			err = fmt.Errorf("mhvtl_data: %w", err)
		}
	}
	if err != nil && c.Error == "" {
		c.Error = err.Error()
	}
	return c
}

// readRaw fills c from the superblock and MAM region of the disk the raw
// cartridge symlink link points to. A symlink whose target is gone is a disk
// that was removed from its bay: that is reported as MediaAbsent, not an
// error.
//
// vtltape rewrites the MAM region before the superblock that records its
// length, so a read between the two can see a truncated MAM; that is retried
// once.
func readRaw(c *Cartridge, link string) {
	if _, err := os.Stat(link); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		c.MediaAbsent = true
		return
	}
	base := *c
	if err := readRawOnce(c, link); errors.Is(err, errTruncated) {
		*c = base
		_ = readRawOnce(c, link)
	}
}

// readRawOnce reads the superblock and MAM region of the disk at link once.
// It records any failure in c.Error and also returns it.
func readRawOnce(c *Cartridge, link string) error {
	sb, err := rawdisk.ReadSuperblock(link)
	if err != nil {
		c.Error = "superblock: " + errText(err)
		return err
	}
	c.UUID = uuidString(sb.CartridgeUUID)
	c.GenerationID = sb.GenerationID
	c.DeviceBytes = sb.TotalCapacityBytes
	c.Partitions = len(sb.Partitions)
	for _, p := range sb.Partitions {
		c.CapacityBytes += p.AdvertisedCapacity
		c.UsedBytes += p.DataUsed
	}

	b, err := rawdisk.ReadMAM(link, sb)
	if err != nil {
		c.Error = "mam: " + errText(err)
		return err
	}
	if b == nil {
		return nil // freshly formatted: no MAM written yet
	}
	if err := parseRawMAM(c, b); err != nil {
		c.Error = err.Error()
		return err
	}
	return nil
}

// parseRawMAM decodes the raw backend's MAM region: two host-order u32
// lengths, then the mam and mhvtl_data streams.
func parseRawMAM(c *Cartridge, b []byte) error {
	if len(b) < 8 {
		return errors.New("mam: short header")
	}
	mlen := uint64(binary.NativeEndian.Uint32(b))
	hlen := uint64(binary.NativeEndian.Uint32(b[4:]))
	if 8+mlen+hlen > uint64(len(b)) {
		return fmt.Errorf("mam: %w", errTruncated)
	}
	if err := parseMAM(c, b[8:8+mlen]); err != nil {
		return fmt.Errorf("mam: %w", err)
	}
	if err := parseMhvtl(c, b[8+mlen:8+mlen+hlen]); err != nil {
		return fmt.Errorf("mhvtl_data: %w", err)
	}
	return nil
}

// errText is err without the path a *os.PathError carries, since it ends up
// in Cartridge.Error.
func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// uuidString formats a 16-byte UUID in the canonical 8-4-4-4-12 form.
func uuidString(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// readFile reads at most maxFileSize bytes of path. Errors name only the
// file, not the full path, since they end up in Cartridge.Error.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return b, nil
}

// errTruncated means a record runs past the end of the file.
var errTruncated = errors.New("truncated record")

// walkRecords calls fn for each u16 id, u16 length, value record in b, all
// in host byte order. Fields decoded before a truncated record are kept.
func walkRecords(b []byte, fn func(id uint16, v []byte)) error {
	for len(b) > 0 {
		if len(b) < 4 {
			return errTruncated
		}
		id := binary.NativeEndian.Uint16(b[0:])
		n := int(binary.NativeEndian.Uint16(b[2:]))
		b = b[4:]
		if n > len(b) {
			return fmt.Errorf("attribute 0x%04x: %w", id, errTruncated)
		}
		fn(id, b[:n])
		b = b[n:]
	}
	return nil
}

// parseMAM decodes the mam file: two u32 format versions, then records.
func parseMAM(c *Cartridge, b []byte) error {
	if len(b) < mamHeaderLen {
		return errors.New("short header")
	}
	return walkRecords(b[mamHeaderLen:], func(id uint16, v []byte) {
		switch id {
		case attrRemainingCapacity:
			c.RemainingBytes = be64(v)
		case attrMaxCapacity:
			c.CapacityBytes = be64(v)
		case attrLoadCount:
			c.LoadCount = be64(v)
		case attrWrittenLife:
			c.WrittenBytes = be64(v)
		case attrReadLife:
			c.ReadBytes = be64(v)
		case attrMediumType:
			if len(v) >= 1 {
				c.Type = mediumType(v[0])
			}
		case attrLastWritten:
			c.LastWritten = trimASCII(v)
		}
	})
}

// parseMhvtl decodes the mhvtl_data file: records with no header.
func parseMhvtl(c *Cartridge, b []byte) error {
	return walkRecords(b, func(id uint16, v []byte) {
		switch id {
		case vtlFlags:
			if len(v) >= 2 {
				c.WriteProtected = binary.NativeEndian.Uint16(v)&flagWriteProtect != 0
			}
		case vtlNumPartitions:
			if len(v) >= 1 {
				c.Partitions = int(v[0])
			}
		case vtlDensityName:
			c.Density = trimASCII(v)
		case vtlDescription:
			c.Description = trimASCII(v)
		}
	})
}

// be64 decodes a big-endian 8-byte MAM number; other widths decode as 0.
func be64(v []byte) uint64 {
	if len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// mediumType names a MAM MEDIUM TYPE value (mhvtl include/vtllib.h
// MEDIA_TYPE_*).
func mediumType(t byte) string {
	switch t {
	case 0:
		return "data"
	case 1:
		return "WORM"
	case 2:
		return "NULL"
	case 6:
		return "clean"
	}
	return "unknown"
}

// trimASCII cuts a fixed-width field at its first NUL and strips padding.
func trimASCII(v []byte) string {
	if i := strings.IndexByte(string(v), 0); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(string(v))
}
