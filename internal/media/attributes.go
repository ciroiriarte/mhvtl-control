package media

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"mhvtl-control/internal/rawdisk"
	"mhvtl-control/internal/scsi"
)

// AttributeDef is one attribute of mhVTL's static MAM table: the id, the
// field width vtltape stores and reports, whether an initiator may write it
// and its format (scsi.AttrBinary, AttrASCII or AttrText).
type AttributeDef struct {
	ID       uint16
	Name     string
	Length   int
	ReadOnly bool
	Format   uint8
}

// staticAttributes is mhVTL's static MAM table, init_mam
// (usr/vtllib.c:179 at 2c40a93), in its order, which write_mam keeps on
// disk. Names are those of SPC-4's MAM attribute tables; 0x1623 is in the
// host vendor specific range, where LTFS keeps its volume lock state.
// TestStaticAttributesMatchMhvtl checks id, length, read-only and format
// against the C source. SPC-4 counts capacity and traffic in MiB, and
// capacity per partition; mhVTL keeps bytes for the whole medium in the
// file (usr/vtllib.c:2317, usr/cmd/vtltape.c:1381) and converts only when
// READ ATTRIBUTE answers (usr/cmd/vtltape.c:442), so the names say bytes.
var staticAttributes = []AttributeDef{
	{0x0000, "Remaining capacity (bytes, whole medium)", 8, true, scsi.AttrBinary},
	{0x0001, "Maximum capacity (bytes, whole medium)", 8, true, scsi.AttrBinary},
	{0x0002, "TapeAlert flags", 8, true, scsi.AttrBinary},
	{0x0003, "Load count", 8, true, scsi.AttrBinary},
	{0x0004, "MAM space remaining", 8, true, scsi.AttrBinary},
	{0x0005, "Assigning organization", 8, true, scsi.AttrASCII},
	{0x0006, "Formatted density code", 1, true, scsi.AttrBinary},
	{0x0007, "Initialization count", 2, true, scsi.AttrBinary},
	{0x0009, "Volume change reference", 4, true, scsi.AttrBinary},
	{0x020a, "Device vendor/serial number at last load", 40, true, scsi.AttrASCII},
	{0x020b, "Device vendor/serial number at load-1", 40, true, scsi.AttrASCII},
	{0x020c, "Device vendor/serial number at load-2", 40, true, scsi.AttrASCII},
	{0x020d, "Device vendor/serial number at load-3", 40, true, scsi.AttrASCII},
	{0x0220, "Bytes written in medium life", 8, true, scsi.AttrBinary},
	{0x0221, "Bytes read in medium life", 8, true, scsi.AttrBinary},
	{0x0222, "Bytes written in current/last load", 8, true, scsi.AttrBinary},
	{0x0223, "Bytes read in current/last load", 8, true, scsi.AttrBinary},
	{0x0400, "Medium manufacturer", 8, true, scsi.AttrASCII},
	{0x0401, "Medium serial number", 32, true, scsi.AttrASCII},
	{0x0402, "Medium length", 4, true, scsi.AttrBinary},
	{0x0403, "Medium width", 4, true, scsi.AttrBinary},
	{0x0404, "Assigning organization", 8, true, scsi.AttrASCII},
	{0x0405, "Medium density code", 1, true, scsi.AttrBinary},
	{0x0406, "Medium manufacture date", 12, true, scsi.AttrASCII},
	{0x0407, "MAM capacity", 8, true, scsi.AttrBinary},
	{0x0408, "Medium type", 1, false, scsi.AttrBinary},
	{0x0409, "Medium type information", 2, true, scsi.AttrBinary},
	{0x0800, "Application vendor", 8, false, scsi.AttrASCII},
	{0x0801, "Application name", 32, false, scsi.AttrASCII},
	{0x0802, "Application version", 8, false, scsi.AttrASCII},
	{0x0803, "User medium text label", 160, false, scsi.AttrText},
	{0x0804, "Date and time last written", 12, false, scsi.AttrASCII},
	{0x0805, "Text localization identifier", 1, false, scsi.AttrBinary},
	{0x0806, "Barcode", 32, false, scsi.AttrASCII},
	{0x0807, "Owning host textual name", 80, false, scsi.AttrText},
	{0x0808, "Media pool", 160, false, scsi.AttrText},
	{0x080b, "Application format version", 16, false, scsi.AttrASCII},
	{0x080c, "Volume coherency information", 0x46, false, scsi.AttrBinary},
	{0x1623, "Volume lock", 1, false, scsi.AttrBinary},
}

// staticByID indexes staticAttributes.
var staticByID = func() map[uint16]AttributeDef {
	m := make(map[uint16]AttributeDef, len(staticAttributes))
	for _, d := range staticAttributes {
		m[d.ID] = d
	}
	return m
}()

// Host vendor specific attribute range (mhvtl include/vtllib.h:205 at
// 2c40a93): ids outside the static table are stored with their format
// byte ahead of the value (usr/vtlcart.c:917 at 2c40a93).
const (
	vendorAttrFirst = 0x1400
	vendorAttrLast  = 0x17ff
)

// How a private mhvtl_data value decodes.
const (
	privNumber = iota // big-endian number, as vtltape stores it
	privHost          // host-order number
	privASCII         // NUL-padded string
	privHex           // opaque bytes
)

// privateAttr is one mhvtl_data record kind (init_mam, the INIT_VTL_ATTR
// lines). Their values are struct MAM fields copied as they are: some are
// set with put_unaligned_be* (partition capacity, bits per mm, tracks;
// usr/vtllib.c:1952 at 2c40a93), Flags and the nonce limit are plain
// host-order fields.
type privateAttr struct {
	name   string
	decode int
}

var privateAttrs = map[uint16]privateAttr{
	0x01: {"Record dirty", privNumber},
	0x02: {"Flags", privHost},
	0x03: {"Maximum partitions", privNumber},
	0x04: {"Partitions", privNumber},
	0x05: {"Media type", privNumber},
	0x06: {"Bits per mm", privNumber},
	0x07: {"Tracks", privNumber},
	0x08: {"Density name", privASCII},
	0x09: {"Description", privASCII},
	0x0a: {"Volume coherency, partition 1", privHex},
	0x0b: {"Volume coherency, partition 2", privHex},
	0x0c: {"Volume coherency, partition 3", privHex},
	0x0d: {"Partition 0 capacity (bytes)", privNumber},
	0x0e: {"Partition 1 capacity (bytes)", privNumber},
	0x0f: {"Partition 2 capacity (bytes)", privNumber},
	0x10: {"Partition 3 capacity (bytes)", privNumber},
	0x12: {"AES-GCM nonce limit", privHost},
}

// vtlGCMSalt is the AES-GCM key salt record. It is not reported: it is not
// secret, but it is key material and of no use to an operator.
const vtlGCMSalt = 0x11

// Attribute groups.
const (
	GroupDevice       = "device"
	GroupMedium       = "medium"
	GroupHost         = "host"
	GroupDeviceVendor = "device_vendor"
	GroupMediumVendor = "medium_vendor"
	GroupHostVendor   = "host_vendor"
	GroupMhvtl        = "mhvtl"
)

// Attribute is one MAM attribute as recorded on disk.
type Attribute struct {
	ID       string `json:"id"` // "0x0801"
	Name     string `json:"name,omitempty"`
	Group    string `json:"group"`
	Format   string `json:"format"` // binary, ascii, text
	ReadOnly bool   `json:"read_only"`
	// Editable is whether PATCH .../attributes may set it (EditableAttribute).
	Editable bool `json:"editable"`
	Length   int  `json:"length"`
	// Value is the decoded value: a number in decimal, a string without its
	// padding, a medium type name, a summary or hex.
	Value string `json:"value"`
	Raw   string `json:"raw"` // hex
}

// AttributeSet is a cartridge's MAM attributes as of the last MAM write.
type AttributeSet struct {
	Barcode           string      `json:"barcode"`
	Backend           string      `json:"backend"`
	TapeFormatVersion uint32      `json:"tape_format_version,omitempty"`
	MAMFormatVersion  uint32      `json:"mam_format_version,omitempty"`
	Attributes        []Attribute `json:"attributes"`
	// Error says why the attributes are incomplete or absent; those read
	// before the failure are kept.
	Error string `json:"error,omitempty"`
}

// EditableAttribute returns the definition of attribute id when it is one
// that may be edited: a host attribute of the static table that an
// initiator may write, in ASCII or TEXT format. Binary ones (text
// localization, volume coherency, volume lock) belong to the backup
// application's own bookkeeping, vendor attributes have no fixed width, and
// 0x0408 written as 80h turns the medium into WORM for good
// (usr/cmd/vtltape.c:657 at 2c40a93).
func EditableAttribute(id uint16) (AttributeDef, bool) {
	d, ok := staticByID[id]
	if !ok || d.ReadOnly || group(id) != GroupHost || (d.Format != scsi.AttrASCII && d.Format != scsi.AttrText) {
		return AttributeDef{}, false
	}
	return d, true
}

// EditableAttributes lists the editable attribute definitions in table
// order.
func EditableAttributes() []AttributeDef {
	var out []AttributeDef
	for _, d := range staticAttributes {
		if _, ok := EditableAttribute(d.ID); ok {
			out = append(out, d)
		}
	}
	return out
}

// ParseAttributeID parses an attribute id written as hex with a 0x prefix,
// such as "0x0801".
func ParseAttributeID(s string) (uint16, error) {
	h, ok := strings.CutPrefix(strings.ToLower(s), "0x")
	if !ok || h == "" {
		return 0, fmt.Errorf("attribute id %q: want hex with a 0x prefix, such as 0x0801", s)
	}
	v, err := strconv.ParseUint(h, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("attribute id %q: want hex with a 0x prefix, such as 0x0801", s)
	}
	return uint16(v), nil
}

// Pad validates value for d and returns it padded to the field width.
// WRITE ATTRIBUTE copies only the bytes sent (usr/cmd/vtltape.c:677 at
// 2c40a93), so a shorter value would leave the tail of the old one: ASCII
// is left-aligned and space-padded, TEXT NUL-padded (SPC-4, MAM attribute
// formats). ASCII must be printable 20h-7Eh. TEXT is in the character set
// the text localization identifier (0805h) names: UTF-8 without NUL when
// utf8Text (identifier 81h), else printable ASCII, as identifier 00h (no
// code specified, mhVTL's default) is taken (SPC-4 table 340).
func (d AttributeDef) Pad(value string, utf8Text bool) ([]byte, error) {
	if len(value) > d.Length {
		return nil, fmt.Errorf("attribute 0x%04x (%s): %d bytes, at most %d fit", d.ID, d.Name, len(value), d.Length)
	}
	pad := byte(' ')
	switch d.Format {
	case scsi.AttrASCII:
		for i := 0; i < len(value); i++ {
			if c := value[i]; c < 0x20 || c > 0x7e {
				return nil, fmt.Errorf("attribute 0x%04x (%s): ASCII allows only printable characters 0x20-0x7E", d.ID, d.Name)
			}
		}
	case scsi.AttrText:
		if !utf8Text {
			for i := 0; i < len(value); i++ {
				if c := value[i]; c < 0x20 || c > 0x7e {
					return nil, fmt.Errorf("attribute 0x%04x (%s): the cartridge's text localization identifier (0x0805) is not UTF-8 (81h), so text allows only printable ASCII 0x20-0x7E", d.ID, d.Name)
				}
			}
		} else if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("attribute 0x%04x (%s): text must be UTF-8 without NUL", d.ID, d.Name)
		}
		pad = 0
	default:
		return nil, fmt.Errorf("attribute 0x%04x (%s): binary attributes are not editable", d.ID, d.Name)
	}
	b := make([]byte, d.Length)
	for i := range b {
		b[i] = pad
	}
	copy(b, value)
	return b, nil
}

// Value returns the decoded value of attribute id ("0x0401") and whether
// the set holds it.
func (s AttributeSet) Value(id string) (string, bool) {
	for _, a := range s.Attributes {
		if a.ID == id {
			return a.Value, true
		}
	}
	return "", false
}

// TextIsUTF8 reports whether the cartridge's text localization identifier
// (0805h) is 81h, UTF-8 (SPC-4 table 340), so its TEXT attributes may hold
// UTF-8 rather than ASCII.
func (s AttributeSet) TextIsUTF8() bool {
	for _, a := range s.Attributes {
		if a.ID == "0x0805" {
			return a.Raw == "81"
		}
	}
	return false
}

// Attributes returns the MAM attributes of cartridge <home>/<barcode>, from
// the mam and mhvtl_data files or the raw disk's MAM region. vtltape
// rewrites them after every WRITE ATTRIBUTE and on unload; while the
// cartridge is loaded its counters may be ahead of them. A missing
// cartridge yields an error wrapping os.ErrNotExist; unreadable metadata is
// reported in AttributeSet.Error.
func Attributes(home, barcode string) (AttributeSet, error) {
	if !validBarcode(barcode) {
		return AttributeSet{}, fmt.Errorf("cartridge %q: %w", barcode, os.ErrNotExist)
	}
	path := filepath.Join(home, barcode)
	fi, err := os.Lstat(path)
	if err != nil {
		return AttributeSet{}, fmt.Errorf("cartridge %s: %w", barcode, err)
	}
	if !isCartridge(fi) {
		return AttributeSet{}, fmt.Errorf("cartridge %s: not a directory or symlink: %w", barcode, os.ErrNotExist)
	}
	set := AttributeSet{Barcode: barcode, Backend: BackendFile, Attributes: []Attribute{}}
	if fi.Mode()&os.ModeSymlink != 0 {
		set.Backend = BackendRaw
		rawAttributes(&set, path)
		return set, nil
	}
	mam, err := readFile(filepath.Join(path, "mam"))
	if err != nil {
		set.Error = err.Error()
		return set, nil
	}
	if err := set.parseMAM(mam); err != nil {
		set.Error = fmt.Sprintf("mam: %v", err)
	}
	vtl, err := readFile(filepath.Join(path, "mhvtl_data"))
	if err == nil {
		if err = set.parseMhvtl(vtl); err != nil {
			err = fmt.Errorf("mhvtl_data: %w", err)
		}
	}
	if err != nil && set.Error == "" {
		set.Error = err.Error()
	}
	return set, nil
}

// rawAttributes fills set from the MAM region of the raw cartridge's disk
// at link, retrying once a read that raced a MAM rewrite (readRaw).
func rawAttributes(set *AttributeSet, link string) {
	if _, err := os.Stat(link); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		set.Error = "disk not attached"
		return
	}
	base := *set
	if err := rawAttributesOnce(set, link); errors.Is(err, errTruncated) {
		*set = base
		_ = rawAttributesOnce(set, link)
	}
}

func rawAttributesOnce(set *AttributeSet, link string) error {
	sb, err := rawdisk.ReadSuperblock(link)
	if err != nil {
		set.Error = "superblock: " + errText(err)
		return err
	}
	b, err := rawdisk.ReadMAM(link, sb)
	if err != nil {
		set.Error = "mam: " + errText(err)
		return err
	}
	if b == nil {
		return nil // freshly formatted: no MAM written yet
	}
	mam, vtl, err := splitRawMAM(b)
	if err == nil {
		if err = set.parseMAM(mam); err != nil {
			err = fmt.Errorf("mam: %w", err)
		}
	}
	if err == nil {
		if err = set.parseMhvtl(vtl); err != nil {
			err = fmt.Errorf("mhvtl_data: %w", err)
		}
	}
	if err != nil {
		set.Error = err.Error()
	}
	return err
}

// parseMAM decodes the mam stream: two u32 format versions, then records.
func (set *AttributeSet) parseMAM(b []byte) error {
	if len(b) < mamHeaderLen {
		return errors.New("short header")
	}
	set.TapeFormatVersion = binary.NativeEndian.Uint32(b)
	set.MAMFormatVersion = binary.NativeEndian.Uint32(b[4:])
	return walkRecords(b[mamHeaderLen:], func(id uint16, v []byte) {
		set.Attributes = append(set.Attributes, mamAttribute(id, v))
	})
}

// parseMhvtl decodes the mhvtl_data stream.
func (set *AttributeSet) parseMhvtl(b []byte) error {
	return walkRecords(b, func(id uint16, v []byte) {
		if id == vtlGCMSalt {
			return
		}
		p := privateAttrs[id]
		a := Attribute{
			ID: fmt.Sprintf("0x%02x", id), Name: p.name, Group: GroupMhvtl,
			Format: "binary", ReadOnly: true, Length: len(v), Raw: hex.EncodeToString(v),
		}
		switch {
		case p.name == "":
		case p.decode == privASCII:
			a.Format, a.Value = "ascii", trimASCII(v)
		case p.decode == privHost:
			a.Value = hostNumber(v)
		case p.decode == privNumber:
			a.Value = number(v)
		}
		if a.Value == "" && a.Format == "binary" {
			a.Value = a.Raw
		}
		set.Attributes = append(set.Attributes, a)
	})
}

// mamAttribute decodes one mam record: a static attribute, a host vendor
// attribute (format byte, then value) or one this table does not know.
func mamAttribute(id uint16, v []byte) Attribute {
	a := Attribute{ID: fmt.Sprintf("0x%04x", id), Group: group(id), Format: "binary"}
	d, static := staticByID[id]
	switch {
	case static:
		_, a.Editable = EditableAttribute(id)
		a.Name, a.ReadOnly, a.Format = d.Name, d.ReadOnly, formatName(d.Format)
	case id >= vendorAttrFirst && id <= vendorAttrLast && len(v) > 0:
		a.Format = formatName(v[0] & 0x03)
		v = v[1:]
	}
	a.Length, a.Raw = len(v), hex.EncodeToString(v)
	switch {
	case a.Format == "ascii" || a.Format == "text":
		// Only the padding goes: leading spaces are part of the value.
		a.Value = trimPadding(v)
	case id == attrMediumTypeInfo:
		// A uint16_t assigned directly (usr/cmd/mktape.c:207) and copied
		// as it is by write_mam, so host order, unlike the other numbers.
		if a.Value = hostNumber(v); a.Value == "" {
			a.Value = a.Raw
		}
	case id == attrMediumType && len(v) == 1:
		a.Value = mediumType(v[0])
	case id == attrTapeAlert || !static:
		a.Value = a.Raw
	case id == attrVolumeCoherency:
		a.Value = coherency(v)
	default:
		if a.Value = number(v); a.Value == "" {
			a.Value = a.Raw
		}
	}
	return a
}

// attrMediumTypeInfo is MEDIUM TYPE INFORMATION (0409h, SPC-4).
const attrMediumTypeInfo = 0x0409

// More SCSI MAM attribute ids (SPC-4).
const (
	attrTapeAlert       = 0x0002
	attrVolumeCoherency = 0x080c
)

// group names the SPC-4 range attribute id is in.
func group(id uint16) string {
	switch {
	case id < 0x0400:
		return GroupDevice
	case id < 0x0800:
		return GroupMedium
	case id < 0x0c00:
		return GroupHost
	case id < 0x1000:
		return GroupDeviceVendor
	case id < 0x1400:
		return GroupMediumVendor
	default:
		return GroupHostVendor
	}
}

func formatName(f uint8) string {
	switch f {
	case scsi.AttrASCII:
		return "ascii"
	case scsi.AttrText:
		return "text"
	}
	return "binary"
}

// number decodes a big-endian number of 1 to 8 bytes in decimal, "" for
// other widths.
func number(v []byte) string {
	if len(v) == 0 || len(v) > 8 {
		return ""
	}
	var n uint64
	for _, c := range v {
		n = n<<8 | uint64(c)
	}
	return strconv.FormatUint(n, 10)
}

// hostNumber decodes a host-order number of 1, 2, 4 or 8 bytes in decimal,
// "" for other widths.
func hostNumber(v []byte) string {
	switch len(v) {
	case 1:
		return strconv.Itoa(int(v[0]))
	case 2:
		return strconv.FormatUint(uint64(binary.NativeEndian.Uint16(v)), 10)
	case 4:
		return strconv.FormatUint(uint64(binary.NativeEndian.Uint32(v)), 10)
	case 8:
		return strconv.FormatUint(binary.NativeEndian.Uint64(v), 10)
	}
	return ""
}

// coherency summarizes a VOLUME COHERENCY INFORMATION value (SSC-4): a
// VOLUME CHANGE REFERENCE VALUE LENGTH n, the n-byte value, the 8-byte
// VOLUME COHERENCY COUNT and SET IDENTIFIER, then the application client
// specific information with its 2-byte length (mhvtl include/vtllib.h:190
// at 2c40a93). An all-zero value is "" (never written).
func coherency(v []byte) string {
	zero := true
	for _, c := range v {
		zero = zero && c == 0
	}
	if zero {
		return ""
	}
	if len(v) < 1 || 1+int(v[0])+18 > len(v) {
		return hex.EncodeToString(v)
	}
	n := int(v[0])
	vcr := v[1 : 1+n]
	rest := v[1+n:]
	info := int(binary.BigEndian.Uint16(rest[16:18]))
	info = min(info, len(rest)-18)
	return fmt.Sprintf("volume change reference 0x%x, count %d, set 0x%x, application information %q",
		vcr, binary.BigEndian.Uint64(rest[0:8]), binary.BigEndian.Uint64(rest[8:16]), printable(rest[18:18+info]))
}

// printable keeps the bytes of b from 20h to 7Eh, replacing others with '.'.
func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}

// trimPadding cuts a fixed-width ASCII or TEXT field at its first NUL and
// strips the trailing spaces that pad it.
func trimPadding(v []byte) string {
	if i := bytes.IndexByte(v, 0); i >= 0 {
		v = v[:i]
	}
	return strings.TrimRight(string(v), " ")
}
