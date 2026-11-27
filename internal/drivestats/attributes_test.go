package drivestats

import (
	"bytes"
	"errors"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/scsi"
)

// serialData is READ ATTRIBUTE data holding MEDIUM SERIAL NUMBER sn,
// NUL-padded as vtltape answers it; no attribute at all when sn is "".
func serialData(sn string) []byte {
	if sn == "" {
		return []byte{0, 0, 0, 0}
	}
	v := make([]byte, 32)
	copy(v, sn)
	b := []byte{0, 0, 0, 5 + 32, 0x04, 0x01, 0x81, 0, 32}
	return append(b, v...)
}

// tape answers each READ ATTRIBUTE with the next of reads (the last one
// repeating), where "UA" is UNIT ATTENTION, and WRITE ATTRIBUTE with
// writeErr.
func tape(writeErr error, reads ...string) *scsi.FakeDevice {
	return &scsi.FakeDevice{Respond: func(cdb, data []byte) (int, error) {
		if cdb[0] != 0x8c {
			return 0, writeErr
		}
		r := reads[0]
		if len(reads) > 1 {
			reads = reads[1:]
		}
		if r == "UA" {
			return 0, scsi.DecodeSense(scsi.FixedSense(scsi.KeyUnitAttention, 0x28, 0))
		}
		return copy(data, serialData(r)), nil
	}}
}

const sn = "A00001L8_1792082732"

func TestWriteAttributes(t *testing.T) {
	dev := tape(nil, sn)
	attrs := []scsi.Attribute{{ID: 0x0801, Format: scsi.AttrASCII, Value: []byte("app ")}}
	if err := reader(dev).WriteAttributes(config.Drive{ID: 11}, sn, attrs); err != nil {
		t.Fatal(err)
	}
	cdbs := dev.Recorded()
	// READ ATTRIBUTE, the write, READ ATTRIBUTE again.
	if len(cdbs) != 3 || len(dev.DataOut) != 1 {
		t.Fatalf("%d commands, %d data-out, want 3 and 1", len(cdbs), len(dev.DataOut))
	}
	wantRead := []byte{0x8c, 0, 0, 0, 0, 0, 0, 0, 0x04, 0x01, 0, 0, 0x02, 0, 0, 0}
	wantCDB := []byte{0x8d, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 13, 0, 0}
	wantData := []byte{0, 0, 0, 9, 0x08, 0x01, 0x01, 0, 4, 'a', 'p', 'p', ' '}
	if !bytes.Equal(cdbs[0], wantRead) || !bytes.Equal(cdbs[1], wantCDB) || !bytes.Equal(cdbs[2], wantRead) ||
		!bytes.Equal(dev.DataOut[0], wantData) {
		t.Errorf("read % x write % x read % x data % x", cdbs[0], cdbs[1], cdbs[2], dev.DataOut[0])
	}
	if !dev.Closed {
		t.Error("node left open")
	}
}

func TestWriteAttributesErrors(t *testing.T) {
	attrs := []scsi.Attribute{{ID: 0x0801, Format: scsi.AttrASCII, Value: []byte("x")}}
	d := config.Drive{ID: 11}
	ua := scsi.DecodeSense(scsi.FixedSense(scsi.KeyUnitAttention, 0x28, 0))
	// Nothing is written when the drive holds another cartridge, one with
	// no serial number, when no serial number is expected, or after UNIT
	// ATTENTION on the first read, which is not retried.
	for name, c := range map[string]struct {
		want  string
		reads []string
	}{
		"other cartridge": {sn, []string{"B00002L8_1792082999"}},
		"no serial":       {sn, []string{""}},
		"none expected":   {"", []string{""}},
		"just loaded":     {sn, []string{"UA", sn}},
	} {
		dev := tape(nil, c.reads...)
		if err := reader(dev).WriteAttributes(d, c.want, attrs); !errors.Is(err, ErrMediumChanged) || len(dev.DataOut) != 0 {
			t.Errorf("%s: err = %v, %d writes", name, err, len(dev.DataOut))
		}
	}
	// UNIT ATTENTION on the write is not retried.
	w := tape(ua, sn)
	if err := reader(w).WriteAttributes(d, sn, attrs); !errors.Is(err, ErrMediumChanged) || len(w.DataOut) != 1 {
		t.Errorf("write UNIT ATTENTION: err = %v, %d writes", err, len(w.DataOut))
	}
	// A swap seen only after the write is not success.
	for name, reads := range map[string][]string{
		"swapped": {sn, "B00002L8_1792082999"},
		"loaded":  {sn, "UA"},
	} {
		if err := reader(tape(nil, reads...)).WriteAttributes(d, sn, attrs); !errors.Is(err, ErrWriteUnverified) {
			t.Errorf("%s after the write: err = %v", name, err)
		}
	}
	if err := reader(tape(&scsi.StatusError{Status: 0x18}, sn)).WriteAttributes(d, sn, attrs); !errors.Is(err, ErrReserved) {
		t.Errorf("reservation conflict on write: err = %v", err)
	}
	fail := func(err error) *scsi.FakeDevice {
		return &scsi.FakeDevice{Respond: func(_, _ []byte) (int, error) { return 0, err }}
	}
	if err := reader(fail(&scsi.StatusError{Status: 0x18})).WriteAttributes(d, sn, attrs); !errors.Is(err, ErrReserved) {
		t.Errorf("reservation conflict on read: err = %v", err)
	}
	notReady := scsi.DecodeSense(scsi.FixedSense(scsi.KeyNotReady, 0x3a, 0))
	var se *scsi.SenseError
	if err := reader(fail(notReady)).WriteAttributes(d, sn, attrs); !errors.As(err, &se) || se.Key != scsi.KeyNotReady {
		t.Errorf("not ready: err = %v", err)
	}
	if err := reader(fail(nil)).WriteAttributes(config.Drive{ID: 12}, sn, attrs); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no node: err = %v", err)
	}
	// A node that cannot send data is unavailable for writing.
	r := &Reader{Overrides: map[int]string{11: "/dev/sg5"}, Open: func(string) (changer.Device, error) { return inOnly{}, nil }}
	if err := r.WriteAttributes(d, sn, attrs); !errors.Is(err, ErrUnavailable) {
		t.Errorf("data-in only node: err = %v", err)
	}
}

// inOnly is a changer.Device without the data-out direction.
type inOnly struct{ scsi.Device }

func (inOnly) Close() error { return nil }
