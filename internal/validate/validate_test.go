package validate

import (
	"strconv"
	"strings"
	"testing"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
)

// shipped loads mhVTL's shipped device.conf and library_contents.10 (the
// config package's test copy): library 10 is an STK L700 with two LTO-8 and
// two LTO-6 drives, library 30 an STK L80 with four T10000B. The copy has no
// library_contents.30; the Drive lines mhVTL's generate_library_contents
// writes for it (one per drive) are added here.
func shipped(t *testing.T) config.Inventory {
	t.Helper()
	inv, err := config.LoadInventory("../config/testdata")
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range inv.Libraries {
		if len(l.DriveLines) == 0 {
			inv.Libraries[i].DriveLines = driveLines(len(l.Drives))
		}
	}
	return inv
}

// gen loads testdata/gen: the device.conf and library_contents.10 and .30
// that upstream c65bff0's generate_device_conf and generate_library_contents
// write (home directory made relative).
func gen(t *testing.T) config.Inventory {
	t.Helper()
	inv, err := config.LoadInventory("testdata/gen")
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// The generated configuration validates as it is, under every changer
// personality, with and without NAAs.
func TestGeneratedIsValid(t *testing.T) {
	cur := gen(t)
	if r := Validate(cur, clone(cur), nil, nil, Options{}); !r.OK() {
		t.Errorf("errors: %v", r.Errors)
	}
	for _, vp := range [][2]string{{"IBM", "03584L32"}, {"IBM", "3573-TL"}, {"HP", "MSL G3 Series"}, {"HP", "EML E-Series"},
		{"SPECTRA", "PYTHON"}, {"SPECTRA", "215"}, {"QUANTUM", "Scalar i6000"}, {"OVERLAND", "OVERLAND NEO"}, {"MHVTL", "VTL"}} {
		for _, dropNAA := range []bool{false, true} {
			c := gen(t)
			for i := range c.Libraries {
				c.Libraries[i].Vendor, c.Libraries[i].Product = vp[0], vp[1]
				if dropNAA {
					c.Libraries[i].NAA = ""
					for j := range c.Libraries[i].Drives {
						c.Libraries[i].Drives[j].NAA = ""
					}
				}
			}
			if r := Validate(c, clone(c), nil, nil, Options{}); !r.OK() {
				t.Errorf("%v without NAAs %v: %v", vp, dropNAA, r.Errors)
			}
		}
	}
}

// fromLive returns inv with the Slot and MAP lines of each library whose live
// placement is known showing that placement, as an editor built from the
// live view sends them: Merge takes next as an edit of Baseline.
func fromLive(inv config.Inventory, live *Live) config.Inventory {
	out := clone(inv)
	for i, l := range out.Libraries {
		p, ok := live.library(l.ID)
		if !ok {
			continue
		}
		b := Baseline(l, &p, nil)
		for j, s := range l.Slots {
			out.Libraries[i].Slots[j].Barcode = b.Slots[s.Number]
		}
		for j, m := range l.MAPs {
			out.Libraries[i].MAPs[j].Barcode = b.MAPs[m.Number]
		}
	}
	return out
}

// driveLines returns the lines "Drive 1:" to "Drive n:".
func driveLines(n int) []config.DriveLine {
	var out []config.DriveLine
	for i := 1; i <= n; i++ {
		out = append(out, config.DriveLine{Number: i})
	}
	return out
}

// clone deep-copies an inventory so a test case can change it.
func clone(inv config.Inventory) config.Inventory {
	out := config.Inventory{Standalone: append([]config.Drive(nil), inv.Standalone...)}
	for _, l := range inv.Libraries {
		l.Drives = append([]config.Drive(nil), l.Drives...)
		l.Slots = append([]config.Slot(nil), l.Slots...)
		l.MAPs = append([]config.MAPSlot(nil), l.MAPs...)
		l.Pickers = append([]int(nil), l.Pickers...)
		l.PickerMedia = append([]config.Slot(nil), l.PickerMedia...)
		l.DriveLines = append([]config.DriveLine(nil), l.DriveLines...)
		out.Libraries = append(out.Libraries, l)
	}
	return out
}

// lib builds a library with id, the given personality and LTO-8 drives
// numbered id+1..., and storage slots holding barcodes ("" for empty).
func lib(id int, vendor, product string, drives int, barcodes ...string) config.Library {
	l := config.Library{ID: id, Target: id, Vendor: vendor, Product: product,
		Serial: "LIB" + strconv.Itoa(id), NAA: naa(id), Pickers: []int{1}, DriveLines: driveLines(drives)}
	for i := 1; i <= drives; i++ {
		l.Drives = append(l.Drives, config.Drive{ID: id + i, Target: id + i, LibraryID: id, Slot: i,
			Vendor: "IBM", Product: "ULT3580-TD8", Serial: "DRV" + strconv.Itoa(id+i), NAA: naa(id + i)})
	}
	for i, b := range barcodes {
		l.Slots = append(l.Slots, config.Slot{Number: i + 1, Barcode: b})
	}
	return l
}

// naa returns a distinct device.conf NAA for n < 65536.
func naa(n int) string {
	b := []byte{0x30, 0xaa, 0xbb, 0xcc, 0, 0, byte(n >> 8), byte(n)}
	return hostids.FormatNAA(b)
}

func inv(libs ...config.Library) config.Inventory { return config.Inventory{Libraries: libs} }

// family returns the messages of one family among findings.
func family(fs []Finding, fam string) []string {
	var out []string
	for _, f := range fs {
		if f.Family == fam {
			out = append(out, f.Message)
		}
	}
	return out
}

// expect checks that the findings of fam are exactly one per want entry,
// each containing it.
func expect(t *testing.T, what string, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d findings, want %d:\n  %s", what, len(got), len(want), strings.Join(got, "\n  "))
		return
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("%s: finding %q does not contain %q", what, got[i], want[i])
		}
	}
}

func TestShippedConfigIsValid(t *testing.T) {
	cur := shipped(t)
	r := Validate(cur, clone(cur), nil, nil, Options{})
	if !r.OK() {
		t.Errorf("errors on the shipped configuration: %+v", r.Errors)
	}
	// Both STK libraries lack VPD 83h: one PBS warning each, nothing else.
	expect(t, "warnings", family(r.Warnings, FamilyPreset), []string{"library 10 would not work with Proxmox Backup Server", "library 30 would not work with Proxmox Backup Server"})
	if len(r.Warnings) != 2 {
		t.Errorf("warnings = %+v", r.Warnings)
	}
}

// The plan's acceptance check: the shipped L700 with the PBS preset gives
// exactly one blocking finding.
func TestShippedL700WithPBS(t *testing.T) {
	cur := shipped(t)
	r := Validate(cur, clone(cur), nil, nil, Options{TargetApps: map[int]string{10: PresetPBS}})
	if len(r.Errors) != 1 || r.Errors[0].Family != FamilyPreset || r.Errors[0].Library != 10 {
		t.Errorf("errors = %+v; want one preset error for library 10", r.Errors)
	}
}
