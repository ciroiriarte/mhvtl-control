package conffile

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the edit goldens' .want files")

// editOp is one operation of a .op.json file.
type editOp struct {
	Op                   string `json:"op"`
	Kind                 string `json:"kind"`
	ID                   int    `json:"id"`
	Channel, Target, LUN int
	Key, Value           string
	Barcode              string
	Attrs                []struct{ Key, Value string }
}

type editFile struct {
	File string   `json:"file"` // device.conf, library_contents or mhvtl.conf
	Ops  []editOp `json:"ops"`
}

func applyDevConf(c *DeviceConf, op editOp) error {
	switch op.Op {
	case "set_attr":
		return c.SetAttr(op.Kind, op.ID, op.Key, op.Value)
	case "delete_attr":
		return c.DeleteAttr(op.Kind, op.ID, op.Key)
	case "add_stanza":
		var attrs []Attr
		for _, a := range op.Attrs {
			attrs = append(attrs, Attr{Key: a.Key, Value: a.Value})
		}
		return c.AddStanza(Header{Kind: op.Kind, ID: op.ID, Channel: op.Channel, Target: op.Target, LUN: op.LUN}, attrs)
	case "remove_stanza":
		return c.RemoveStanza(op.Kind, op.ID)
	}
	return fmt.Errorf("unknown device.conf op %q", op.Op)
}

func applyLibContents(c *LibContents, op editOp) error {
	switch op.Op {
	case "set_barcode":
		return c.SetBarcode(op.Kind, op.ID, op.Barcode)
	case "add_entry":
		return c.AddEntry(op.Kind, op.ID, op.Barcode)
	case "remove_entry":
		return c.RemoveEntry(op.Kind, op.ID)
	}
	return fmt.Errorf("unknown library_contents op %q", op.Op)
}

// TestEditGolden runs each testdata/edit/<name>.op.json against <name>.in
// and compares with <name>.want. For device.conf it also checks that every
// stanza no op names, and the preamble, is byte-identical to the input.
func TestEditGolden(t *testing.T) {
	ops, err := filepath.Glob("testdata/edit/*.op.json")
	if err != nil || len(ops) == 0 {
		t.Fatalf("no edit goldens: %v", err)
	}
	for _, opPath := range ops {
		name := strings.TrimSuffix(opPath, ".op.json")
		t.Run(filepath.Base(name), func(t *testing.T) {
			raw, err := os.ReadFile(opPath)
			if err != nil {
				t.Fatal(err)
			}
			var ef editFile
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ef); err != nil {
				t.Fatalf("%s: %v", opPath, err)
			}
			in, err := os.ReadFile(name + ".in")
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			switch ef.File {
			case "device.conf":
				c := ParseDeviceConf(in)
				for _, op := range ef.Ops {
					if err := applyDevConf(c, op); err != nil {
						t.Fatalf("%+v: %v", op, err)
					}
				}
				got = c.Render()
				checkUntouched(t, ParseDeviceConf(in), ParseDeviceConf(got), ef.Ops)
			case "library_contents":
				c := ParseLibContents(in)
				for _, op := range ef.Ops {
					if err := applyLibContents(c, op); err != nil {
						t.Fatalf("%+v: %v", op, err)
					}
				}
				got = c.Render()
			case "mhvtl.conf":
				c := ParseMhvtlConf(in)
				for _, op := range ef.Ops {
					if op.Op != "set" {
						t.Fatalf("unknown mhvtl.conf op %q", op.Op)
					}
					if err := c.Set(op.Key, op.Value); err != nil {
						t.Fatalf("%+v: %v", op, err)
					}
				}
				got = c.Render()
			default:
				t.Fatalf("unknown file %q", ef.File)
			}
			if *update {
				if err := os.WriteFile(name+".want", got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(name + ".want")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func checkUntouched(t *testing.T, in, out *DeviceConf, ops []editOp) {
	t.Helper()
	touched := map[stanzaKey]bool{}
	for _, op := range ops {
		touched[stanzaKey{op.Kind, op.ID}] = true
	}
	before, after := stanzaTexts(in), stanzaTexts(out)
	for k, v := range before {
		if touched[k] {
			continue
		}
		// The one change allowed is the line ending of a file's last line:
		// a missing final newline moves to the new last line.
		if a := after[k]; a != v && strings.TrimRight(a, "\r\n") != strings.TrimRight(v, "\r\n") {
			t.Errorf("untouched %v changed:\n%q\nto\n%q", k, v, after[k])
		}
	}
	preamble := func(c *DeviceConf) string {
		end := len(c.Lines())
		if sts := c.Stanzas(); len(sts) > 0 {
			end = sts[0].Start
		}
		return string(renderLines(c.Lines()[:end]))
	}
	// A Library added in front of the first stanza moves the boundary.
	if p, q := preamble(in), preamble(out); !strings.HasPrefix(q, p) && !strings.HasPrefix(p, q) {
		t.Errorf("preamble changed:\n%q\nto\n%q", p, q)
	}
}

func TestEditRefusals(t *testing.T) {
	lc := ParseLibContents([]byte("Drive 1:\nPicker 1:\nMAP 1:\nSlot 1:\n"))
	if err := lc.SetBarcode("Drive", 1, "E01001L8"); !errors.Is(err, ErrDriveBarcode) {
		t.Errorf("Drive barcode: err = %v, want ErrDriveBarcode", err)
	}
	if err := lc.AddEntry("Drive", 2, "E01001L8"); !errors.Is(err, ErrDriveBarcode) {
		t.Errorf("new Drive with barcode: err = %v, want ErrDriveBarcode", err)
	}
	for _, bad := range []struct {
		kind, code string
	}{{"Picker", "X"}, {"Slot", "A B"}, {"MAP", "A\tB"}, {"Bogus", ""}} {
		if err := lc.SetBarcode(bad.kind, 1, bad.code); err == nil {
			t.Errorf("SetBarcode(%q, %q) accepted", bad.kind, bad.code)
		}
	}
	if err := lc.SetBarcode("Slot", 9, "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing slot: err = %v", err)
	}
	if err := lc.AddEntry("Slot", 1, ""); err == nil {
		t.Error("duplicate slot accepted")
	}

	dc := ParseDeviceConf([]byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n Vendor identification: STK\n"))
	if err := dc.SetAttr("Drive", 11, "NAA", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing stanza: err = %v", err)
	}
	for _, kv := range [][2]string{{"", "x"}, {"A:B", "x"}, {" A", "x"}, {"A", "x\ny"}, {"A", "x\r"}} {
		if err := dc.SetAttr("Library", 10, kv[0], kv[1]); err == nil {
			t.Errorf("SetAttr(%q, %q) accepted", kv[0], kv[1])
		}
	}
	if err := dc.AddStanza(Header{Kind: "Library", ID: 10}, nil); err == nil {
		t.Error("duplicate stanza accepted")
	}
	if err := dc.AddStanza(Header{Kind: "Robot", ID: 1}, nil); err == nil {
		t.Error("unknown stanza kind accepted")
	}

	mc := ParseMhvtlConf([]byte("CAPACITY=500\n"))
	if err := mc.Set("DAEMON_DEBUG", "-d"); err == nil {
		t.Error("unmodelled key accepted")
	}
	for _, v := range []string{"a b", `"x"`, "$(id)", "a;b", "x\n"} {
		if err := mc.Set("CAPACITY", v); err == nil {
			t.Errorf("value %q accepted", v)
		}
	}
	if got := string(mc.Render()); got != "CAPACITY=500\n" {
		t.Errorf("refused edits changed the file: %q", got)
	}
}
