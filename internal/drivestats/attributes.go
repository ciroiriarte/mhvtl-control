package drivestats

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// writeAttributeTimeout bounds one WRITE ATTRIBUTE.
const writeAttributeTimeout = 30 * time.Second

// ErrMediumChanged means the drive may not hold the cartridge the caller
// expected: its medium serial number differs or is absent, or READ or
// WRITE ATTRIBUTE met UNIT ATTENTION, as after a load. Nothing was written.
var ErrMediumChanged = errors.New("the cartridge in the drive changed")

// ErrWriteUnverified means WRITE ATTRIBUTE succeeded but the drive no
// longer showed the expected cartridge right after, so the attributes may
// have gone to another one.
var ErrWriteUnverified = errors.New("the write could not be verified")

// attrSerial is the MEDIUM SERIAL NUMBER MAM attribute (0401h, SPC-4),
// which no initiator can write: mhVTL sets it when the cartridge is made
// (usr/cmd/mktape.c:235, usr/vtlcart_raw.c:1087 at 2c40a93).
const attrSerial = 0x0401

// WriteAttributes writes MAM attributes to the cartridge loaded in d with
// WRITE ATTRIBUTE, through the sg node Read uses, for partition 0 (the host
// attributes are not per partition in mhVTL: usr/cmd/vtltape.c:442 at
// 2c40a93). vtltape rewrites the cartridge's MAM files once the command
// succeeds (usr/ssc.c:1463 at 2c40a93). serial is the cartridge's medium
// serial number (0401h) as its MAM file holds it.
//
// A backup application can swap the cartridge through the changer at any
// time, so the drive's medium serial number is read before the write and
// must be serial, and read again after it. Nothing here can make the check
// and the write atomic: mhVTL keeps one pending UNIT ATTENTION per drive,
// not one per initiator (usr/ssc.c:2068 at 2c40a93), and the library's
// unload ignores PREVENT (usr/cmd/vtltape.c:1977). A swap that another
// initiator's command acknowledges between the two reads is reported as
// ErrWriteUnverified, never as success.
//
// UNIT ATTENTION is not retried: on the first read it is ErrMediumChanged
// (a cartridge was just loaded; the caller may retry), on the write
// ErrMediumChanged, and on the second read ErrWriteUnverified. Reporting it
// clears it for every initiator, as with any command mhVTL does not exempt
// (usr/cmd/vtltape.c:1399,1476 at 2c40a93).
//
// Errors are as Read's: ErrUnavailable when the node cannot be found or
// opened, ErrReserved on RESERVATION CONFLICT, and a *scsi.SenseError on
// CHECK CONDITION, such as NOT READY when no cartridge is loaded; and
// ErrMediumChanged and ErrWriteUnverified.
func (r *Reader) WriteAttributes(d config.Drive, serial string, attrs []scsi.Attribute) error {
	params, err := scsi.WriteAttributeParams(attrs)
	if err != nil {
		return err
	}
	path, err := r.Locate(d)
	if err != nil {
		return err
	}
	if r.Open == nil {
		return fmt.Errorf("%w: drive %d: no opener", ErrUnavailable, d.ID)
	}
	dev, err := r.Open(path)
	if err != nil {
		return fmt.Errorf("%w: drive %d: open %s: %w", ErrUnavailable, d.ID, path, err)
	}
	defer dev.Close()
	out, ok := dev.(scsi.DataOutDevice)
	if !ok {
		return fmt.Errorf("%w: drive %d: %s cannot send data", ErrUnavailable, d.ID, path)
	}
	wrap := func(op string, err error) error {
		var st *scsi.StatusError
		if errors.As(err, &st) && st.Status == statusReservationConflict {
			return fmt.Errorf("drive %d: %w: %w", d.ID, ErrReserved, err)
		}
		return fmt.Errorf("drive %d: %s: %w", d.ID, op, err)
	}
	if serial == "" {
		return fmt.Errorf("drive %d: %w: no medium serial number to check the cartridge against", d.ID, ErrMediumChanged)
	}
	if got, err := readSerial(dev); err != nil {
		if isUnitAttention(err) {
			return fmt.Errorf("drive %d: %w: a cartridge was just loaded (%v); retry", d.ID, ErrMediumChanged, err)
		}
		return wrap("read attribute", err)
	} else if got != serial {
		return fmt.Errorf("drive %d: %w: it holds medium serial %q, not %q", d.ID, ErrMediumChanged, got, serial)
	}
	_, err = out.DoOut(scsi.WriteAttributeCDB(0, uint32(len(params))), params, writeAttributeTimeout)
	if isUnitAttention(err) {
		return fmt.Errorf("drive %d: %w: UNIT ATTENTION before the write (%v)", d.ID, ErrMediumChanged, err)
	}
	if err != nil {
		return wrap("write attribute", err)
	}
	got, err := readSerial(dev)
	switch {
	case err != nil:
		return fmt.Errorf("drive %d: %w: reading the medium serial number after the write: %v", d.ID, ErrWriteUnverified, err)
	case got != serial:
		return fmt.Errorf("drive %d: %w: after the write it holds medium serial %q, not %q; the attributes may have been written to that cartridge",
			d.ID, ErrWriteUnverified, got, serial)
	}
	return nil
}

// readSerial returns the medium serial number of the cartridge in dev, ""
// when the drive reports none.
func readSerial(dev scsi.Device) (string, error) {
	buf := make([]byte, 512)
	n, _, err := dev.Do(scsi.ReadAttributeCDB(0, attrSerial, uint32(len(buf))), buf, writeAttributeTimeout)
	if err != nil {
		return "", err
	}
	v, _ := scsi.FindAttribute(buf[:n], attrSerial)
	return trimField(v), nil
}

// trimField cuts an ASCII attribute at its first NUL and strips the
// trailing spaces that pad it.
func trimField(v []byte) string {
	if i := bytes.IndexByte(v, 0); i >= 0 {
		v = v[:i]
	}
	return strings.TrimRight(string(v), " ")
}

func isUnitAttention(err error) bool {
	var se *scsi.SenseError
	return errors.As(err, &se) && se.Key == scsi.KeyUnitAttention
}
