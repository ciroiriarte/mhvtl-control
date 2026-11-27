package scsi

import (
	"encoding/binary"
	"fmt"
)

// opWriteAttribute is WRITE ATTRIBUTE (8Dh, SPC-4); opReadAttribute is
// READ ATTRIBUTE (8Ch).
const (
	opWriteAttribute = 0x8d
	opReadAttribute  = 0x8c
)

// ReadAttributeCDB builds READ ATTRIBUTE (8Ch), service action ATTRIBUTE
// VALUES (00h), for partition from attribute first on, with an allocation
// length of alloc. mhVTL reads the service action from byte 1, the
// partition from byte 7, the first attribute from bytes 8-9 and the
// allocation length from bytes 10-13 (usr/ssc.c:1573, usr/cmd/vtltape.c:534
// at 2c40a93).
func ReadAttributeCDB(partition uint8, first uint16, alloc uint32) []byte {
	cdb := make([]byte, 16)
	cdb[0] = opReadAttribute
	cdb[7] = partition
	binary.BigEndian.PutUint16(cdb[8:10], first)
	binary.BigEndian.PutUint32(cdb[10:14], alloc)
	return cdb
}

// FindAttribute returns the value of attribute id in READ ATTRIBUTE
// ATTRIBUTE VALUES data b: a 4-byte AVAILABLE DATA length, then attributes
// as id (2), READ ONLY bit and FORMAT (1), length (2) and value.
func FindAttribute(b []byte, id uint16) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	end := 4 + int(binary.BigEndian.Uint32(b))
	if end > len(b) || end < 4 {
		end = len(b)
	}
	for i := 4; i+5 <= end; {
		n := int(binary.BigEndian.Uint16(b[i+3:]))
		if i+5+n > end {
			return nil, false
		}
		if binary.BigEndian.Uint16(b[i:]) == id {
			return b[i+5 : i+5+n], true
		}
		i += 5 + n
	}
	return nil, false
}

// MAM attribute formats: the FORMAT field of an attribute header (SPC-4
// MAM attribute format table), and the format column of mhVTL's attribute
// table (usr/vtllib.c:179 at 2c40a93).
const (
	AttrBinary = 0
	AttrASCII  = 1
	AttrText   = 2
)

// Attribute is one MAM attribute to write: its id, FORMAT and value as
// sent.
type Attribute struct {
	ID     uint16
	Format uint8
	Value  []byte
}

// WriteAttributeCDB builds WRITE ATTRIBUTE (8Dh) for partition with a
// parameter list of paramLen bytes, and WTC (write-through cache) clear.
// mhVTL reads the partition from byte 7 and the parameter list length
// from bytes 10-13 (usr/ssc.c:1423, usr/cmd/vtltape.c:607 at 2c40a93).
func WriteAttributeCDB(partition uint8, paramLen uint32) []byte {
	cdb := make([]byte, 16)
	cdb[0] = opWriteAttribute
	cdb[7] = partition
	binary.BigEndian.PutUint32(cdb[10:14], paramLen)
	return cdb
}

// WriteAttributeParams builds the WRITE ATTRIBUTE parameter list: a 4-byte
// PARAMETER DATA LENGTH (the bytes after it), then each attribute as id
// (2), READ ONLY bit and FORMAT (1), length (2) and value. The value is
// sent as given: a target such as mhVTL copies only the bytes sent over a
// fixed-width field (usr/cmd/vtltape.c:677 at 2c40a93), so the caller pads
// it to the full width.
func WriteAttributeParams(attrs []Attribute) ([]byte, error) {
	b := make([]byte, 4, 4+5*len(attrs))
	for _, a := range attrs {
		if len(a.Value) > 0xffff {
			return nil, fmt.Errorf("scsi: attribute 0x%04x: value of %d bytes", a.ID, len(a.Value))
		}
		b = binary.BigEndian.AppendUint16(b, a.ID)
		b = append(b, a.Format&0x03)
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.Value)))
		b = append(b, a.Value...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b, nil
}
