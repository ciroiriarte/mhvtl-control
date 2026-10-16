package conffile

import (
	"strings"
	"testing"
)

// TestDeviceConfAsMhvtlReads covers lines whose meaning to mhVTL differs
// from their shape: each must be an ambiguous Problem, and round-trip.
func TestDeviceConfAsMhvtlReads(t *testing.T) {
	const lib = "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n"
	const drv = "Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n"
	cases := []struct {
		name, in string
		line     int
		msg      string
	}{
		{"other kind adjacent", lib + " Unit serial number: L\n" + drv + " Unit serial number: D\n", 3, "no blank line after Library 10"},
		{"duplicate stanza", drv + " Unit serial number: A\n\n" + drv + " Unit serial number: B\n", 4, "duplicate Drive 11"},
		{"blank before colon", drv + " Unit serial number : X\n", 2, "blank before ':'"},
		{"two-byte line", lib + " #\n Home directory: /x\n", 2, "two-byte line"},
		{"tab comment two bytes", lib + "\t#\n", 2, "two-byte line"},
		{"long line", lib + " Vendor identification: " + strings.Repeat("x", 500) + "\n", 2, "longer than 510"},
		{"NUL", lib + " Vendor identification: A\x00B\n", 2, "NUL"},
		{"text after LUN", "Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00 # x\n", 1, "text after LUN"},
		{"empty Compression", drv + " Compression:\n", 2, "partial or empty Compression"},
		{"Compression without enabled", drv + " Compression: factor 1\n", 2, "partial or empty Compression"},
		{"Compression ignored", drv + " Compression: bogus\n", 2, "vtltape ignores it"},
		{"white space in a Drive", drv + "  \n", 2, "Compression sscanf"},
		{"short NAA", lib + " NAA: 10:22\n", 2, "NAA has 2 of 8"},
		{"Library ID without Slot", drv + " Library ID: 10\n", 2, "without 'Slot: N'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseDeviceConf([]byte(tc.in))
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
		})
	}

	// The forms vtltape and vtllibrary read as written.
	if c := ParseDeviceConf([]byte(drv + " Compression: 5\n NAA: 1:2:3:4:5:6:7:8\n NAA:\n" +
		" Library ID: 10 Slot: 01\n Compression: factor 1 enabled 0\n")); len(c.Problems) != 0 {
		t.Errorf("valid sscanf fields flagged: %v", c.Problems)
	}

	// Adjacent Libraries read as written: each header switches vtllibrary
	// to the new library (vtllibrary.c:1283).
	if c := ParseDeviceConf([]byte(lib + " Unit serial number: A\nLibrary: 30 CHANNEL: 00 TARGET: 08 LUN: 00\n")); len(c.Problems) != 0 {
		t.Errorf("adjacent libraries flagged: %v", c.Problems)
	}
	// Adjacent Drives do not: vtllibrary moves to the new drive only at its
	// Library ID line, so a serial before it lands on the previous drive.
	c := ParseDeviceConf([]byte(drv + " Library ID: 10 Slot: 01\nDrive: 12 CHANNEL: 00 TARGET: 02 LUN: 00\n Unit serial number: B\n Library ID: 10 Slot: 02\n"))
	if len(c.Problems) != 1 || c.Problems[0].Line != 3 || !strings.Contains(c.Problems[0].Msg, "after Drive 11") {
		t.Errorf("adjacent drives: %v", c.Problems)
	}
	// A 600-byte column-1 comment is still split by readline.
	if c := ParseDeviceConf([]byte(strings.Repeat("#", 600) + "\n")); len(c.Problems) != 1 {
		t.Errorf("long comment: %v", c.Problems)
	}
}

// TestConfValue checks attribute keys and values are read as conf_value
// reads them, and that SetAttr writes only what it reads back.
func TestConfValue(t *testing.T) {
	c := ParseDeviceConf([]byte("Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n" +
		" Vendor identification: IBM # vendor\n" +
		" Product  identification:\tULT3580-TD8\n" +
		" Unit serial number: X#1\n" +
		" NAA: 10:22:33:44:ab:00:01:00\n NAA:\n"))
	st := mustStanza(t, c, "Drive", 11)
	wantAttr(t, st, "Vendor identification", "IBM")
	wantAttr(t, st, "Product identification", "ULT3580-TD8")
	wantAttr(t, st, "Unit serial number", "X#1")
	// An empty NAA is ignored, not a reset: sscanf returns EOF, and mhVTL
	// acts only on a positive count (vtltape.c:2217-2236).
	wantAttr(t, st, "NAA", "10:22:33:44:ab:00:01:00")
	for _, v := range []string{"10:22", "zz"} {
		if err := c.SetAttr("Drive", 11, "NAA", v); err == nil {
			t.Errorf("SetAttr(NAA, %q) accepted", v)
		}
	}
	if err := c.SetAttr("Drive", 11, "Compression", ""); err == nil {
		t.Error("SetAttr(Compression, \"\") accepted")
	}

	for _, v := range []string{"HP #x", "HP\t#x", " HP", "HP ", "A\x00B"} {
		if err := c.SetAttr("Drive", 11, "Vendor identification", v); err == nil {
			t.Errorf("SetAttr(%q) accepted", v)
		}
	}
	if err := c.SetAttr("Drive", 11, "Product  identification", "x"); err == nil {
		t.Error("non-canonical key accepted")
	}
	// The trailing comment survives an edit of the value.
	if err := c.SetAttr("Drive", 11, "Vendor identification", "HP"); err != nil {
		t.Fatal(err)
	}
	if got := c.Lines()[1].Text; got != " Vendor identification: HP # vendor" {
		t.Errorf("line = %q", got)
	}
	// The doubled-space key is found under its canonical name.
	if err := c.SetAttr("Drive", 11, "Product identification", "ULT3580-TD9"); err != nil {
		t.Fatal(err)
	}
	if got := c.Lines()[2].Text; got != " Product  identification:\tULT3580-TD9" {
		t.Errorf("line = %q", got)
	}
	// Clearing a commented value drops the comment, which conf_value would
	// otherwise read as the value.
	if err := c.SetAttr("Drive", 11, "Vendor identification", ""); err != nil {
		t.Fatal(err)
	}
	if got := c.Lines()[1].Text; got != " Vendor identification: " {
		t.Errorf("line = %q", got)
	}
}

// TestRemoveStanzaBoundaries pins which comments go with a removed stanza
// and that the records around it never end up adjacent.
func TestRemoveStanzaBoundaries(t *testing.T) {
	in := "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n Vendor identification: STK\n\n" +
		"Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n# fifo: /run/mhvtl/fifo\n" +
		"# about drive 12\nDrive: 12 CHANNEL: 00 TARGET: 02 LUN: 00\n Library ID: 10 Slot: 02\n"
	c := ParseDeviceConf([]byte(in))
	if err := c.RemoveStanza("Drive", 11); err != nil {
		t.Fatal(err)
	}
	// Drive 11 abuts drive 12, so the blank before it stays (removing it
	// would join the Library to drive 12), and "# fifo:" is in drive 12's
	// leading block, so it stays too.
	want := "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n Vendor identification: STK\n\n" +
		"# fifo: /run/mhvtl/fifo\n# about drive 12\nDrive: 12 CHANNEL: 00 TARGET: 02 LUN: 00\n Library ID: 10 Slot: 02\n"
	if got := string(c.Render()); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if len(c.Problems) != 0 {
		t.Errorf("problems after removal: %v", c.Problems)
	}
}

// TestCScanf pins cScanf to what glibc's sscanf returns for the same input
// and format.
func TestCScanf(t *testing.T) {
	const (
		c1  = " Compression: factor %d enabled %d"
		c2  = " Compression: %d"
		lid = " Library ID: %ld Slot: %d"
		hdr = "Drive: %d CHANNEL: %d TARGET: %d LUN: %d"
	)
	for _, tc := range []struct {
		s, format string
		want      int
	}{
		{" Compression:", c1, -1}, {" Compression:", c2, -1},
		{"  ", c1, -1}, {" \t ", c1, -1}, {" Compression:\r", c1, -1},
		{" Compression: f", c1, -1}, {" Compression: f", c2, 0},
		{" Compression: factor", c1, -1},
		{" Compression: factor 1", c1, 1}, {" Compression: factor 1 enabled", c1, 1},
		{" Compression: factor 1 enabled 0", c1, 2},
		{" Compression: 5", c1, 0}, {" Compression: 5", c2, 1},
		{" Compression: bogus", c1, 0}, {" Compression: bogus", c2, 0},
		{" Compression: factor x enabled 1", c1, 0},
		{" Vendor identification: IBM", c1, 0}, {" Compression type: lzo", c1, 0},
		{" NAA:", naaFormat, -1}, {" NAA: 10:22", naaFormat, 2},
		{" NAA: 10:22:33:44:ab:00:00:00", naaFormat, 8}, {" NAA: zz", naaFormat, 0},
		{" NAA: 1:2:3:4:5:6:7:8", naaFormat, 8}, {" NAA: 100:22:33:44:55:66:77:88", naaFormat, 1},
		{" NAA: 10:22:33:44:AB:00:00:0g", naaFormat, 8}, {" NAA: -1:2", naaFormat, 2},
		{" Library ID: 10", lid, 1}, {" Library ID: 10 Slot: 01", lid, 2},
		{" Library ID: 10 Slot:", lid, 1}, {" Library ID:", lid, -1},
		{" Library ID: x", lid, 0}, {" Library  ID: 10 Slot: 2", lid, 2},
		{"Drive: 11", hdr, 1}, {"Drive: 11 CHANNEL: 00", hdr, 2},
		{"Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00", hdr, 4}, {"Dr", hdr, -1},
	} {
		if got := cScanf(tc.s, tc.format); got != tc.want {
			t.Errorf("sscanf(%q, %q) = %d, want %d", tc.s, tc.format, got, tc.want)
		}
	}
}
