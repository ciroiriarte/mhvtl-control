package conffile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// corpus returns every round-trip fixture, keyed by path.
func corpus(t testing.TB) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob("testdata/roundtrip/*/*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no round-trip corpus: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = b
	}
	return out
}

// render parses data with the parser its file name calls for and renders it
// back, returning any problems found.
func render(name string, data []byte) ([]byte, []Problem) {
	switch {
	case strings.Contains(name, "library_contents"):
		c := ParseLibContents(data)
		return c.Render(), c.Problems
	case strings.HasSuffix(name, "mhvtl.conf"):
		return ParseMhvtlConf(data).Render(), nil
	default:
		c := ParseDeviceConf(data)
		return c.Render(), c.Problems
	}
}

func TestRoundTripCorpus(t *testing.T) {
	for name, data := range corpus(t) {
		t.Run(name, func(t *testing.T) {
			got, problems := render(filepath.Base(name), data)
			if !bytes.Equal(got, data) {
				t.Errorf("render differs from input\ngot:  %q\nwant: %q", got, data)
			}
			// Files in ambiguous/ load in mhVTL but carry lines its
			// parsers read differently: those, and only those, are flagged.
			if strings.Contains(name, "/ambiguous/") {
				if len(problems) == 0 {
					t.Error("no problems reported")
				}
				for _, p := range problems {
					if !p.Ambiguous {
						t.Errorf("not an ambiguous-line problem: %v", p)
					}
				}
			} else if len(problems) > 0 {
				t.Errorf("problems in a valid file: %v", problems)
			}
		})
	}
}

func mustStanza(t *testing.T, c *DeviceConf, kind string, id int) Stanza {
	t.Helper()
	st, ok := c.Stanza(kind, id)
	if !ok {
		t.Fatalf("%s %d not found", kind, id)
	}
	return st
}

func wantAttr(t *testing.T, st Stanza, key, want string) {
	t.Helper()
	if got, ok := st.Get(key); !ok || got != want {
		t.Errorf("%s %d %q = %q, %v; want %q", st.Kind, st.ID, key, got, ok, want)
	}
}

func readConf(t *testing.T, name string) *DeviceConf {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/roundtrip", name))
	if err != nil {
		t.Fatal(err)
	}
	return ParseDeviceConf(b)
}

func TestShippedDeviceConf(t *testing.T) {
	c := readConf(t, "shipped/device.conf")
	var ids []int
	for _, st := range c.Stanzas() {
		if !st.HasAddress {
			t.Errorf("%s %d has no address", st.Kind, st.ID)
		}
		ids = append(ids, st.ID)
	}
	if want := []int{10, 11, 12, 13, 14, 30, 31, 32, 33, 34}; !reflect.DeepEqual(ids, want) {
		t.Errorf("stanza ids = %v, want %v", ids, want)
	}
	if v, ok := c.Version(); !ok || v != "5" {
		t.Errorf("VERSION = %q, %v", v, ok)
	}
	l10 := mustStanza(t, c, "Library", 10)
	wantAttr(t, l10, "PERSIST", "False")
	wantAttr(t, l10, "Backoff", "400")
	// The "# fifo:" comment after the attributes stays in the stanza.
	if line := c.Lines()[l10.End-1].Text; line != "# fifo: /run/mhvtl/fifo" {
		t.Errorf("library 10 ends with %q", line)
	}
	d13 := mustStanza(t, c, "Drive", 13)
	if d13.Target != 3 || d13.Channel != 0 || d13.LUN != 0 {
		t.Errorf("drive 13 address = %+v", d13)
	}
	wantAttr(t, d13, "Library ID", "10 Slot: 03")
}

func TestCraftedDeviceConf(t *testing.T) {
	t.Run("crlf", func(t *testing.T) {
		c := readConf(t, "crafted/crlf.device.conf")
		wantAttr(t, mustStanza(t, c, "Library", 10), "Vendor identification", "STK")
		wantAttr(t, mustStanza(t, c, "Drive", 11), "Product identification", "ULT3580-TD8")
	})
	t.Run("tab indent", func(t *testing.T) {
		c := readConf(t, "crafted/tab-indent.device.conf")
		l := mustStanza(t, c, "Library", 10)
		wantAttr(t, l, "Vendor identification", "STK")
		wantAttr(t, l, "Product identification", "L700")
		wantAttr(t, mustStanza(t, c, "Drive", 11), "Product identification", "ULT3580-TD8")
	})
	t.Run("no final newline", func(t *testing.T) {
		c := readConf(t, "crafted/no-final-newline.device.conf")
		d := mustStanza(t, c, "Drive", 11)
		wantAttr(t, d, "Product identification", "ULT3580-TD8")
		if d.End != len(c.Lines()) {
			t.Errorf("drive 11 ends at %d of %d lines", d.End, len(c.Lines()))
		}
	})
	t.Run("unknown keys", func(t *testing.T) {
		c := readConf(t, "crafted/unknown-keys.device.conf")
		l := mustStanza(t, c, "Library", 10)
		for k, v := range map[string]string{"PERSIST": "True", "Backoff": "400",
			"movecommand": "/usr/local/bin/robot", "commandtimeout": "20",
			"fifo": "/run/mhvtl/fifo", "Some future key": "with: colons"} {
			wantAttr(t, l, k, v)
		}
		wantAttr(t, mustStanza(t, c, "Drive", 11), "Backoff", "1000")
	})
	t.Run("indented hash", func(t *testing.T) {
		c := readConf(t, "crafted/indented-hash.device.conf")
		l := mustStanza(t, c, "Library", 10)
		var keys []string
		for _, a := range l.Attrs {
			keys = append(keys, a.Key)
		}
		if want := []string{"Vendor identification", "Product identification", "Unit serial number"}; !reflect.DeepEqual(keys, want) {
			t.Errorf("attrs = %v, want %v", keys, want)
		}
		wantAttr(t, l, "Product identification", "L700")
		if l.Start != l.HeaderLine-1 {
			t.Errorf("leading comment not in the stanza: start %d header %d", l.Start, l.HeaderLine)
		}
	})
	t.Run("VERSION 3", func(t *testing.T) {
		c := readConf(t, "crafted/version3.device.conf")
		if v, ok := c.Version(); !ok || v != "3" {
			t.Errorf("VERSION = %q, %v", v, ok)
		}
	})
	t.Run("headerless drive", func(t *testing.T) {
		c := readConf(t, "crafted/headerless-drive.device.conf")
		d11 := mustStanza(t, c, "Drive", 11)
		if d11.HasAddress {
			t.Errorf("drive 11 should have no address: %+v", d11)
		}
		wantAttr(t, d11, "Library ID", "10 Slot: 01")
		if d12 := mustStanza(t, c, "Drive", 12); !d12.HasAddress || d12.Target != 2 {
			t.Errorf("drive 12 = %+v", d12)
		}
	})
}

// TestAmbiguousBlank checks white-space lines inside a record: each is a
// Problem, and the record goes on past it as vtllibrary reads it, so the
// attribute after it is not lost or orphaned.
func TestAmbiguousBlank(t *testing.T) {
	c := readConf(t, "ambiguous/space-blank.device.conf")
	var lines []int
	for _, p := range c.Problems {
		lines = append(lines, p.Line)
	}
	if want := []int{5, 10}; !reflect.DeepEqual(lines, want) {
		t.Errorf("problem lines = %v, want %v", lines, want)
	}
	wantAttr(t, mustStanza(t, c, "Library", 10), "Product identification", "L700")
	if d11 := mustStanza(t, c, "Drive", 11); d11.End != d11.HeaderLine+3 {
		t.Errorf("drive 11 should run to the next header: %+v", d11)
	}

	c = readConf(t, "ambiguous/crlf-space-blank.device.conf")
	if len(c.Problems) != 1 || c.Problems[0].Line != 5 || !c.Problems[0].Ambiguous {
		t.Errorf("problems = %+v, want one ambiguous on line 5", c.Problems)
	}
	// An empty CRLF line is a separator to every parser: not flagged.
	if d11 := mustStanza(t, c, "Drive", 11); d11.End != d11.HeaderLine+2 {
		t.Errorf("drive 11 should end at the empty CRLF line: %+v", d11)
	}

	// A white-space line outside any record changes nothing in mhVTL.
	if c := ParseDeviceConf([]byte("VERSION: 5\n  \n\nLibrary: 1 CHANNEL: 0 TARGET: 0 LUN: 0\n")); len(c.Problems) != 0 {
		t.Errorf("white space outside a record flagged: %v", c.Problems)
	}
}

func ambiguousCount(c *DeviceConf) int {
	n := 0
	for _, p := range c.Problems {
		if p.Ambiguous {
			n++
		}
	}
	return n
}

func TestCraftedLibContents(t *testing.T) {
	b, err := os.ReadFile("testdata/roundtrip/crafted/no-final-newline.library_contents")
	if err != nil {
		t.Fatal(err)
	}
	c := ParseLibContents(b)
	want := []Entry{
		{Kind: "Drive", Number: 1, Line: 2},
		{Kind: "Picker", Number: 1, Line: 4},
		{Kind: "MAP", Number: 1, Barcode: "E01040L8", Line: 6},
		{Kind: "Slot", Number: 1, Barcode: "E01001L8", Line: 9},
		{Kind: "Slot", Number: 2, Line: 10},
		{Kind: "Slot", Number: 3, Barcode: "CLN001L1", Line: 11},
	}
	if got := c.Entries(); !reflect.DeepEqual(got, want) {
		t.Errorf("entries =\n%+v\nwant\n%+v", got, want)
	}
	if v, ok := c.Version(); !ok || v != "2" {
		t.Errorf("VERSION = %q, %v", v, ok)
	}
}

// TestPersistLibContents reads library_contents.10.persist as save_config
// writes it (mhvtl usr/cmd/vtllibrary.c:1177-1212): no VERSION line, a blank
// line before each element type, "Slot N: " with a trailing space when
// empty, and barcodes padded to 16 by add_barcode's "%-16.16s"
// (vtllibrary.c:345). Drive lines never carry a barcode: save_config prints
// "Drive %d:" whatever the drive holds. The fixture is library 10 after the
// phase 0 S3 moves; its 976 bytes match the file the VM wrote.
func TestPersistLibContents(t *testing.T) {
	b, err := os.ReadFile("testdata/roundtrip/persist/library_contents.10.persist")
	if err != nil {
		t.Fatal(err)
	}
	c := ParseLibContents(b)
	if len(c.Problems) != 0 {
		t.Fatalf("problems: %v", c.Problems)
	}
	if v, ok := c.Version(); ok {
		t.Errorf("VERSION = %q, want none", v)
	}
	got := map[string]string{}
	count := map[string]int{}
	for _, e := range c.Entries() {
		got[fmt.Sprintf("%s %d", e.Kind, e.Number)] = e.Barcode
		count[e.Kind]++
	}
	if count["Picker"] != 1 || count["MAP"] != 4 || count["Drive"] != 4 || count["Slot"] != 39 {
		t.Errorf("counts = %v", count)
	}
	for k, want := range map[string]string{"MAP 1": "E01002L8", "MAP 2": "", "Picker 1": "",
		"Drive 4": "", "Slot 1": "", "Slot 2": "", "Slot 21": "E01001L8", "Slot 30": "F01030L6"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	// Rewriting a padded line changes that line only.
	if err := c.SetBarcode("Slot", 21, "E01021L8"); err != nil {
		t.Fatal(err)
	}
	in, out := splitLines(b), c.Lines()
	if len(in) != len(out) {
		t.Fatalf("line count %d -> %d", len(in), len(out))
	}
	for i := range in {
		if in[i] != out[i] && in[i].Text != "Slot 21: E01001L8        " {
			t.Errorf("line %d changed: %q -> %q", i+1, in[i].Text, out[i].Text)
		}
	}
	if again := ParseLibContents(c.Render()); len(again.Problems) != 0 {
		t.Errorf("problems after edit: %v", again.Problems)
	}
}

func TestCraftedMhvtlConf(t *testing.T) {
	b, err := os.ReadFile("testdata/roundtrip/crafted/quoted.mhvtl.conf")
	if err != nil {
		t.Fatal(err)
	}
	c := ParseMhvtlConf(b)
	for k, v := range map[string]string{"MHVTL_HOME_PATH": "/var/lib/mhvtl", "CAPACITY": "500",
		"VERBOSE": "1", "VTL_DEBUG": "0", "UNMODELLED": "keep"} {
		if got, ok := c.Get(k); !ok || got != v {
			t.Errorf("%s = %q, %v; want %q", k, got, ok, v)
		}
	}
	if _, ok := c.Get("DAEMON_DEBUG"); ok {
		t.Error("a commented-out key was read")
	}
}

func TestProblems(t *testing.T) {
	cases := map[string]string{
		"Library: x CHANNEL: 0 TARGET: 0 LUN: 0\n":                         "malformed record header",
		" Vendor identification: IBM\n":                                    "attribute outside of a record",
		"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n garbage\n":                 "expected 'Key: value'",
		"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Slot: 1\n\n Vendor: IBM\n": "attribute outside of a record",
		"garbage\n": "malformed record header",
	}
	for in, want := range cases {
		c := ParseDeviceConf([]byte(in))
		if len(c.Problems) != 1 || !strings.Contains(c.Problems[0].Msg, want) {
			t.Errorf("%q: problems %v, want one %q", in, c.Problems, want)
		}
		if !bytes.Equal(c.Render(), []byte(in)) {
			t.Errorf("%q: not round-tripped", in)
		}
	}
	for in, want := range map[string]string{"Slot x: ABC\n": "bad number", "Slot 1 ABC\n": "missing ':'",
		"Bogus 1:\n": "unknown entry", "Slot:\n": "malformed"} {
		c := ParseLibContents([]byte(in))
		if len(c.Problems) != 1 || !strings.Contains(c.Problems[0].Msg, want) {
			t.Errorf("%q: problems %v, want one %q", in, c.Problems, want)
		}
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, data := range corpus(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dc := ParseDeviceConf(data)
		if got := dc.Render(); !bytes.Equal(got, data) {
			t.Fatalf("device.conf: render %q != input %q", got, data)
		}
		lc := ParseLibContents(data)
		if got := lc.Render(); !bytes.Equal(got, data) {
			t.Fatalf("library_contents: render %q != input %q", got, data)
		}
		mc := ParseMhvtlConf(data)
		if got := mc.Render(); !bytes.Equal(got, data) {
			t.Fatalf("mhvtl.conf: render %q != input %q", got, data)
		}

		// An edit must read back as written, leave every other stanza's
		// lines as they were, and never write a white-space line.
		ambiguous := ambiguousCount(dc)
		defer func() {
			if n := ambiguousCount(ParseDeviceConf(dc.Render())); n > ambiguous {
				t.Fatalf("edits added white-space lines: %d -> %d", ambiguous, n)
			}
		}()
		if sts := dc.Stanzas(); len(sts) > 0 {
			st := sts[0]
			before := stanzaTexts(dc)
			if err := dc.SetAttr(st.Kind, st.ID, "Fuzz key", "fuzz value"); err != nil {
				t.Fatal(err)
			}
			again := ParseDeviceConf(dc.Render())
			got, ok := again.Stanza(st.Kind, st.ID)
			if !ok {
				t.Fatalf("%s %d lost after SetAttr", st.Kind, st.ID)
			}
			if v, _ := got.Get("Fuzz key"); v != "fuzz value" {
				t.Fatalf("SetAttr read back %q", v)
			}
			after := stanzaTexts(again)
			for k, v := range before {
				if k != (stanzaKey{st.Kind, st.ID}) && after[k] != v {
					t.Fatalf("SetAttr on %s %d changed %v", st.Kind, st.ID, k)
				}
			}
		}
		if err := dc.AddStanza(Header{Kind: "Drive", ID: 987654, Target: 1},
			[]Attr{{Key: "Library ID", Value: "10 Slot: 01"}}); err == nil {
			again := ParseDeviceConf(dc.Render())
			got, ok := again.Stanza("Drive", 987654)
			if !ok || !got.HasAddress {
				t.Fatalf("added stanza not read back: %+v %v", got, ok)
			}
			if v, _ := got.Get("Library ID"); v != "10 Slot: 01" {
				t.Fatalf("added stanza attr = %q", v)
			}
			if err := again.RemoveStanza("Drive", 987654); err != nil {
				t.Fatal(err)
			}
			if _, ok := again.Stanza("Drive", 987654); ok {
				t.Fatal("stanza still there after RemoveStanza")
			}
		}
		if err := mc.Set("CAPACITY", "1234"); err != nil {
			t.Fatal(err)
		}
		if v, _ := ParseMhvtlConf(mc.Render()).Get("CAPACITY"); v != "1234" {
			t.Fatalf("CAPACITY read back %q", v)
		}
	})
}

type stanzaKey struct {
	kind string
	id   int
}

// stanzaTexts maps each stanza (first occurrence) to its lines' bytes.
func stanzaTexts(c *DeviceConf) map[stanzaKey]string {
	out := map[stanzaKey]string{}
	for _, st := range c.Stanzas() {
		k := stanzaKey{st.Kind, st.ID}
		if _, dup := out[k]; !dup {
			out[k] = string(renderLines(c.Lines()[st.Start:st.End]))
		}
	}
	return out
}
