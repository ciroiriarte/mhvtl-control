package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// that upstream generate_device_conf and generate_library_contents write
// (home directory made relative). Master 993b193 writes them byte for byte as
// c65bff0, where they were made, did.
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

// A finding carries a code and the elements it concerns, and serialises them
// with lowercase field names.
func TestFindingCodeAndElements(t *testing.T) {
	cur := inv(lib(10, "IBM", "03584L32", 1, "E01001L8", ""))
	next := clone(cur)
	next.Libraries[0].Slots[1].Barcode = "E01001L8"
	r := Validate(cur, next, nil, nil, Options{})
	if len(r.Errors) != 1 {
		t.Fatalf("errors = %+v; want one", r.Errors)
	}
	f := r.Errors[0]
	want := []ElementRef{{ElementSlot, 10, 1}, {ElementSlot, 10, 2}}
	if f.Code != CodeBarcodeDup || f.Family != FamilyBarcode || len(f.Elements) != 2 || f.Elements[0] != want[0] || f.Elements[1] != want[1] {
		t.Errorf("finding = %+v; want code %s with elements %+v", f, CodeBarcodeDup, want)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"code":"barcode.duplicate"`) || !strings.Contains(string(b), `"elements":[{"kind":"slot","library":10,"number":1}`) {
		t.Errorf("json = %s", b)
	}
}

// A conffile Problem in the proposed files blocks, since no writer writes
// one; one only the configuration in force has is a pre-existing warning.
func TestFileProblems(t *testing.T) {
	d := t.TempDir()
	conf := "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n Vendor identification: IBM\n Product identification: 03584L32\n Unit serial number: LIB10\n NAA: 30:aa:bb:cc:00:00:00:0a\n\n" +
		"Drive: 011 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n Unit serial number: DRV11\n NAA: 30:aa:bb:cc:00:00:00:0b\n"
	if err := os.WriteFile(filepath.Join(d, "device.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "library_contents.10"), []byte("Drive 1:\nPicker 1:\nSlot 1:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := config.LoadInventory(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad.Problems) != 1 {
		t.Fatalf("problems = %+v; want the leading 0", bad.Problems)
	}
	good := clone(bad) // the same configuration, without the Problems

	r := Validate(good, bad, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyFile), []string{"Drive id with a leading 0"})
	if len(r.Errors) != 1 || r.Errors[0].Code != CodeFileProblem {
		t.Errorf("errors = %+v", r.Errors)
	}

	r = Validate(bad, good, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyFile), nil)
	expect(t, "warnings", family(r.Warnings, FamilyFile), []string{"Drive id with a leading 0"})
	if len(r.Warnings) != 1 || r.Warnings[0].Tag != TagPreExisting {
		t.Errorf("warnings = %+v", r.Warnings)
	}
}

// A Home directory with a blank in it blocks when the change sets it, and is
// a pre-existing warning when the configuration in force has it already.
func TestHomeWithBlank(t *testing.T) {
	cur := inv(lib(10, "IBM", "03584L32", 1))
	next := clone(cur)
	next.Libraries[0].HomeDir = "/srv/my tapes"
	r := Validate(cur, next, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyMedia), []string{`library 10: Home directory "/srv/my tapes" has a blank in it`})

	next2 := clone(next)
	next2.Libraries[0].Serial = "LIB10B"
	r = Validate(next, next2, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyMedia), nil)
	expect(t, "warnings", family(r.Warnings, FamilyMedia), []string{`library 10: Home directory "/srv/my tapes" has a blank in it`})
	if len(r.Warnings) != 1 || r.Warnings[0].Tag != TagPreExisting {
		t.Errorf("warnings = %+v", r.Warnings)
	}
}

// TestHomeProblem: a media home is an absolute, clean path of plain
// characters, outside system trees and mhvtl-control's own state.
func TestHomeProblem(t *testing.T) {
	for path, ok := range map[string]bool{
		"/opt/mhvtl":                     true,
		"/srv/tapes":                     true,
		"/var/lib/mhvtl":                 true,
		"/var/lib/mhvtl-controlled":      true,
		"tapes":                          false,
		"./tapes":                        false,
		"/proc/sys/kernel":               false,
		"/var/lib/mhvtl-control-apply":   false,
		"/var/lib/mhvtl-control-apply/x": false,
		"/var/lib/mhvtl-control":         false,
		"/etc/mhvtl":                     false,
		"/x/../etc/cron.d":               false,
		"/srv/tapes/":                    false,
		"/srv//tapes":                    false,
		"/":                              false,
		"/usr/local/tapes":               false,
		"/srv/$(id)":                     false,
		"/srv/a b":                       false,
		"/run/mhvtl":                     false,
		"/dev/shm/tapes":                 false,
		"/lib/systemd/system":            false,
		"/lib64":                         false,
		"/sbin/x":                        false,
		"/home/tapes":                    false,
		"/root/tapes":                    false,
		"/var/log/mhvtl":                 false,
		"/var/spool/cron":                false,
		"/var/lib/systemd/x":             false,
		"/library":                       true,
		"/homes/tapes":                   true,
	} {
		if got := HomeProblem(path) == ""; got != ok {
			t.Errorf("HomeProblem(%q) = %q, want ok %v", path, HomeProblem(path), ok)
		}
	}
	cur := shipped(t)
	next := clone(cur)
	next.Libraries[0].HomeDir = "/etc/mhvtl"
	r := Validate(cur, next, nil, nil, Options{})
	expect(t, "errors", codes(r.Errors, CodeMediaHomeForbidden), []string{`library 10: Home directory "/etc/mhvtl" cannot be a media home: under /etc`})
	// One the configuration in force already has is a pre-existing warning.
	r = Validate(next, clone(next), nil, nil, Options{})
	expect(t, "warnings", codes(r.Warnings, CodeMediaHomeForbidden), []string{"/etc/mhvtl"})
}

func codes(fs []Finding, code string) []string {
	var out []string
	for _, f := range fs {
		if f.Code == code {
			out = append(out, f.Message)
		}
	}
	return out
}
