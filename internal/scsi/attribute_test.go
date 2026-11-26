package scsi

import (
	"bytes"
	"errors"
	"testing"
)

func TestWriteAttribute(t *testing.T) {
	cdb := WriteAttributeCDB(1, 0x01020304)
	want := []byte{0x8d, 0, 0, 0, 0, 0, 0, 1, 0, 0, 1, 2, 3, 4, 0, 0}
	if !bytes.Equal(cdb, want) {
		t.Errorf("cdb = % x", cdb)
	}
	b, err := WriteAttributeParams([]Attribute{
		{ID: 0x0801, Format: AttrASCII, Value: []byte("ab")},
		{ID: 0x0808, Format: AttrText | 0x80, Value: []byte{'p', 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []byte{
		0, 0, 0, 14,
		0x08, 0x01, 0x01, 0, 2, 'a', 'b',
		0x08, 0x08, 0x02, 0, 2, 'p', 0, // READ ONLY bit never sent
	}
	if !bytes.Equal(b, want) {
		t.Errorf("params = % x", b)
	}
	if b, _ := WriteAttributeParams(nil); !bytes.Equal(b, []byte{0, 0, 0, 0}) {
		t.Errorf("empty params = % x", b)
	}
	if _, err := WriteAttributeParams([]Attribute{{ID: 1, Value: make([]byte, 0x10000)}}); err == nil {
		t.Error("oversized value accepted")
	}
}

func TestFakeDoOut(t *testing.T) {
	var got []byte
	f := &FakeDevice{Respond: func(cdb, data []byte) (int, error) {
		got = data
		return 0, DecodeSense(FixedSense(KeyIllegalRequest, 0x26, 0))
	}}
	data := []byte{1, 2, 3}
	sense, err := f.DoOut([]byte{0x8d}, data, 0)
	var se *SenseError
	if !errors.As(err, &se) || se.Key != KeyIllegalRequest || len(sense) != 18 {
		t.Errorf("err = %v, sense = % x", err, sense)
	}
	data[0] = 9 // the fake keeps its own copies
	if !bytes.Equal(got, []byte{1, 2, 3}) || len(f.DataOut) != 1 || !bytes.Equal(f.DataOut[0], []byte{1, 2, 3}) {
		t.Errorf("respond saw % x, recorded %v", got, f.DataOut)
	}
	if r := f.Recorded(); len(r) != 1 || r[0][0] != 0x8d {
		t.Errorf("cdbs = %v", r)
	}
}

func TestFindAttribute(t *testing.T) {
	b := []byte{0, 0, 0, 12, 0x08, 0x01, 0x01, 0, 2, 'a', 'b', 0x08, 0x06, 0x01, 0, 0}
	if v, ok := FindAttribute(b, 0x0801); !ok || string(v) != "ab" {
		t.Errorf("0x0801: %q %v", v, ok)
	}
	if v, ok := FindAttribute(b, 0x0806); !ok || len(v) != 0 {
		t.Errorf("0x0806: %q %v", v, ok)
	}
	if _, ok := FindAttribute(b, 0x0808); ok {
		t.Error("absent attribute found")
	}
	for _, bad := range [][]byte{nil, {0, 0}, {0, 0, 0, 9, 0x08, 0x01, 0x01, 0, 9, 'a'}} {
		if _, ok := FindAttribute(bad, 0x0801); ok {
			t.Errorf("% x: found", bad)
		}
	}
	if got := ReadAttributeCDB(1, 0x0806, 512); !bytes.Equal(got, []byte{0x8c, 0, 0, 0, 0, 0, 0, 1, 0x08, 0x06, 0, 0, 2, 0, 0, 0}) {
		t.Errorf("cdb % x", got)
	}
}
