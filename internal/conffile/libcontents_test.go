package conffile

import (
	"errors"
	"strings"
	"testing"
)

// TestLibContentsAsMhvtlReads covers lines vtllibrary reads differently from
// their shape (sscanf "Slot %d: %s", add_barcode): each is an ambiguous
// Problem, the entry is modelled as mhVTL loads it, and the file round-trips.
func TestLibContentsAsMhvtlReads(t *testing.T) {
	cases := []struct {
		name, in string
		line     int
		msg      string
		entries  int    // entries modelled
		barcode  string // of the last entry
	}{
		{"indented", "Slot 1: A\n  Slot 2: B\n", 2, "indented Slot 2", 1, "A"},
		{"inner white space", "Slot 1: ABC DEF\n", 1, `reads only "ABC"`, 1, "ABC"},
		{"blank before colon", "Slot 6 : SP\n", 1, "reads Slot 6 as empty", 1, ""},
		{"too long", "Slot 1: ABCDEFGHIJKLMNOPQ\n", 1, "longer than 16", 1, "ABCDEFGHIJKLMNOPQ"},
		{"duplicate", "MAP 1: A\nSlot 1: B\nSlot 2: A\n", 3, "loads both as one cartridge", 3, "A"},
		{"duplicate of 16", "MAP 1: ABCDEFGHIJKLMNOP\nSlot 1: ABCDEFGHIJKLMNOP\n", 2, "exits on start", 2, "ABCDEFGHIJKLMNOP"},
		{"MAP twice", "MAP 1:\nMAP 1: A\n", 2, "MAP 1 listed twice", 2, "A"},
		{"Picker twice", "Picker 1:\nPicker 1:\n", 2, "Picker 1 listed twice", 2, ""},
		{"out of order", "Slot 1:\nSlot 3:\nSlot 2:\n", 3, "Slot 2 after Slot 3", 3, ""},
		{"long line", "Slot 1: " + strings.Repeat("#", 520) + "\n", 1, "longer than 510", 1, strings.Repeat("#", 520)},
		{"NUL", "Slot 1: A\x00B\n", 1, "NUL", 1, "A\x00B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseLibContents([]byte(tc.in))
			if string(c.Render()) != tc.in {
				t.Error("not round-tripped")
			}
			found := false
			for _, p := range c.Problems {
				if p.Line == tc.line && strings.Contains(p.Msg, tc.msg) {
					found = true
					if !p.Ambiguous {
						t.Errorf("%v: not ambiguous", p)
					}
				}
			}
			if !found {
				t.Errorf("problems %v, want %q on line %d", c.Problems, tc.msg, tc.line)
			}
			es := c.Entries()
			if len(es) != tc.entries || es[len(es)-1].Barcode != tc.barcode {
				t.Errorf("entries = %+v", es)
			}
		})
	}
	// A Drive line's value is a serial number, so it is not a barcode
	// duplicate of a cartridge with the same text.
	c := ParseLibContents([]byte("Drive 1: XYZZY_A1\nSlot 1: XYZZY_A1\n"))
	if len(c.Problems) != 0 {
		t.Errorf("serial counted as a barcode: %v", c.Problems)
	}
	if sn, ok := c.DriveSerial(1); !ok || sn != "XYZZY_A1" {
		t.Errorf("DriveSerial(1) = %q, %v", sn, ok)
	}
	if _, ok := ParseLibContents([]byte("Drive 1:\n")).DriveSerial(1); ok {
		t.Error("empty Drive line has a serial")
	}
}

func TestLibContentsEditRefusals(t *testing.T) {
	c := ParseLibContents([]byte("Drive 1:\nSlot 1:\n"))
	if err := c.SetBarcode("Slot", 1, "ABCDEFGHIJKLMNOPQ"); err == nil || !strings.Contains(err.Error(), "longer than 16") {
		t.Errorf("17-character barcode: err = %v", err)
	}
	if err := c.SetBarcode("Slot", 1, "ABCDEFGHIJKLMNOP"); err != nil {
		t.Errorf("16-character barcode: %v", err)
	}
	if err := c.SetBarcode("Drive", 1, "SN1"); !errors.Is(err, ErrDriveBarcode) || !strings.Contains(err.Error(), "serial") {
		t.Errorf("Drive value: err = %v", err)
	}
}

// AppendEntries writes what AddEntry of each next number would, in one
// pass.
func TestAppendEntries(t *testing.T) {
	for _, in := range []string{
		"VERSION: 2\n\nDrive 1:\n\nPicker 1:\n\nMAP 1:\n\nSlot 1: A00001L8\nSlot 2:\n# end\n",
		"Slot 5:\nSlot 2:\n\nMAP 3:\n",
		"VERSION: 2\r\nDrive 1:\r\n",
		"Drive 1:",
		"",
	} {
		for _, kind := range []string{"Slot", "MAP"} {
			want := ParseLibContents([]byte(in))
			highest := 0
			for _, e := range want.Entries() {
				if e.Kind == kind {
					highest = max(highest, e.Number)
				}
			}
			for n := highest + 1; n <= highest+3; n++ {
				if err := want.AddEntry(kind, n, ""); err != nil {
					t.Fatal(err)
				}
			}
			got := ParseLibContents([]byte(in))
			if err := got.AppendEntries(kind, 3); err != nil {
				t.Fatalf("%q %s: %v", in, kind, err)
			}
			if string(got.Render()) != string(want.Render()) || len(got.Entries()) != len(want.Entries()) {
				t.Errorf("%q %s:\n%q\nwant\n%q", in, kind, got.Render(), want.Render())
			}
		}
	}
	c := ParseLibContents([]byte("Slot 1:\n"))
	if err := c.AppendEntries("Drive", 1); err == nil {
		t.Error("Drive lines appended")
	}
	if err := c.AppendEntries("Slot", 0); err == nil {
		t.Error("count 0 accepted")
	}
}
