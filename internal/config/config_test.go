package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func open(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseDeviceConf(t *testing.T) {
	libs, drives, problems, err := ParseDeviceConf(open(t, "testdata/device.conf"))
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
	if len(libs) != 2 || len(drives) != 8 {
		t.Fatalf("got %d libs, %d drives; want 2, 8", len(libs), len(drives))
	}
	l := libs[0]
	want := Library{ID: 10, Vendor: "STK", Product: "L700", Serial: "XYZZY_A",
		NAA: "10:22:33:44:ab:00:00:00", HomeDir: "/opt/mhvtl"}
	if l.ID != want.ID || l.Vendor != want.Vendor || l.Product != want.Product ||
		l.Serial != want.Serial || l.NAA != want.NAA || l.HomeDir != want.HomeDir {
		t.Errorf("library 10 = %+v", l)
	}
	if libs[1].ID != 30 || libs[1].Product != "L80" || libs[1].Target != 8 {
		t.Errorf("library 30 = %+v", libs[1])
	}
	tests := []struct {
		idx  int
		want Drive
	}{
		{0, Drive{ID: 11, Target: 1, LibraryID: 10, Slot: 1, Vendor: "IBM", Product: "ULT3580-TD8",
			Revision: "HB81", Serial: "XYZZY_A1", NAA: "10:22:33:44:ab:00:01:00",
			CompressionFactor: 1, CompressionEnabled: true, CompressionType: "lzo"}},
		{2, Drive{ID: 13, Target: 3, LibraryID: 10, Slot: 3, Vendor: "IBM", Product: "ULT3580-TD6",
			Revision: "2160", Serial: "XYZZY_A3", NAA: "10:22:33:44:ab:00:03:00",
			CompressionFactor: 1, CompressionEnabled: true, CompressionType: "lzo"}},
		{7, Drive{ID: 34, Target: 12, LibraryID: 30, Slot: 4, Vendor: "STK", Product: "T10000B",
			Revision: "550V", Serial: "XYZZY_B4", NAA: "30:22:33:44:ab:00:12:00",
			CompressionFactor: 1, CompressionEnabled: true, CompressionType: "lzo"}},
	}
	for _, tc := range tests {
		if got := drives[tc.idx]; got != tc.want {
			t.Errorf("drive[%d] = %+v, want %+v", tc.idx, got, tc.want)
		}
	}
}

func TestParseDeviceConfCompressionDisabled(t *testing.T) {
	in := "Drive: 5 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID:  10   Slot: 02\n Compression:  factor 9  enabled 0\n"
	_, d, _, err := ParseDeviceConf(strings.NewReader(in))
	if err != nil || len(d) != 1 {
		t.Fatalf("err=%v drives=%d", err, len(d))
	}
	if d[0].CompressionFactor != 9 || d[0].CompressionEnabled || d[0].Slot != 2 || d[0].LibraryID != 10 {
		t.Errorf("got %+v", d[0])
	}
}

// Lines mhVTL ignores or reads its own way are Problems, not read errors:
// the line is skipped and the rest of the file still projects.
func TestParseDeviceConfMalformed(t *testing.T) {
	const tail = "\nDrive: 2 CHANNEL: 0 TARGET: 2 LUN: 0\n Vendor identification: HP\n"
	cases := map[string]struct {
		in     string
		drives int
	}{
		"bad header":       {"Library: x CHANNEL: 0 TARGET: 0 LUN: 0\n", 1},
		"short header":     {"Drive: 1\n", 2},
		"orphan attribute": {" Vendor identification: IBM\n", 1},
		"bad compression":  {"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Compression: bogus\n", 2},
		"bad slot":         {"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Library ID: 10 Slot: zz\n", 2},
		"no colon":         {"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n garbage\n", 2},
		"text after LUN":   {"Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0 # main\n", 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, d, problems, err := ParseDeviceConf(strings.NewReader(tc.in + tail))
			if err != nil {
				t.Fatal(err)
			}
			if (len(problems) == 0) != (name == "text after LUN") {
				t.Errorf("problems = %v", problems)
			}
			if len(d) != tc.drives || d[len(d)-1].Vendor != "HP" {
				t.Errorf("drives = %+v, want %d", d, tc.drives)
			}
		})
	}
	// An attribute the projection cannot read, that conffile does not
	// flag, is reported by the projection.
	_, _, problems, _ := ParseDeviceConf(strings.NewReader("Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Slot: x\n"))
	if len(problems) != 1 || problems[0].Line != 2 || !strings.Contains(problems[0].Msg, "bad Slot") {
		t.Errorf("problems = %v", problems)
	}
}

func TestParseLibraryContents(t *testing.T) {
	drives, pickers, maps, slots, problems, err := ParseLibraryContents(open(t, "testdata/library_contents.sample"))
	if err != nil || len(problems) != 0 {
		t.Fatal(err, problems)
	}
	if len(drives) != 8 || len(pickers) != 1 || len(maps) != 4 || len(slots) != 32 {
		t.Fatalf("counts drives=%d pickers=%d maps=%d slots=%d", len(drives), len(pickers), len(maps), len(slots))
	}
	if drives[0] != (DriveLine{Number: 1}) || drives[7].Number != 8 {
		t.Errorf("drives = %+v", drives)
	}
	for _, tc := range []struct {
		n    int
		code string
	}{{1, "ULT001L1"}, {11, "SDLT01L1"}, {21, ""}, {31, "CLN001L1"}, {32, "CLN002L1"}} {
		if s := slots[tc.n-1]; s.Number != tc.n || s.Barcode != tc.code {
			t.Errorf("slot %d = %+v, want %q", tc.n, s, tc.code)
		}
	}
}

func TestParseLibraryContentsDriveSerial(t *testing.T) {
	drives, _, _, _, _, err := ParseLibraryContents(strings.NewReader("Drive 1: SN0001 extra\nDrive 2:\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []DriveLine{{1, "SN0001"}, {2, ""}}
	if len(drives) != 2 || drives[0] != want[0] || drives[1] != want[1] {
		t.Errorf("drives = %+v; want %+v", drives, want)
	}
}

func TestParseLibraryContentsMalformed(t *testing.T) {
	for _, in := range []string{"Slot x: ABC\n", "Slot 1 ABC\n", "Bogus 1:\n", "Slot:\n", "IEPort 1: B\n"} {
		_, _, _, slots, problems, err := ParseLibraryContents(strings.NewReader(in + "Slot 2: A\n"))
		if err != nil || len(problems) != 1 || len(slots) != 1 {
			t.Errorf("%q: slots %+v, problems %v, err %v", in, slots, problems, err)
		}
	}
}

func TestLoadInventory(t *testing.T) {
	inv, err := LoadInventory("testdata")
	if err != nil || len(inv.Problems) != 0 {
		t.Fatal(err, inv.Problems)
	}
	if len(inv.Libraries) != 2 {
		t.Fatalf("libraries = %d", len(inv.Libraries))
	}
	l10, l30 := inv.Libraries[0], inv.Libraries[1]
	if len(l10.Drives) != 4 || l10.Drives[0].Serial != "XYZZY_A1" {
		t.Errorf("l10 drives = %+v", l10.Drives)
	}
	if len(l10.Slots) != 39 || l10.Slots[0].Barcode != "E01001L8" || l10.Slots[20].Barcode != "" {
		t.Errorf("l10 slots = %d first=%+v slot21=%+v", len(l10.Slots), l10.Slots[0], l10.Slots[20])
	}
	if len(l10.MAPs) != 4 || len(l10.Pickers) != 1 {
		t.Errorf("l10 maps=%d pickers=%d", len(l10.MAPs), len(l10.Pickers))
	}
	if len(l30.Drives) != 4 || len(l30.Slots) != 0 || len(l30.MAPs) != 0 {
		t.Errorf("l30 should have drives but no contents: %+v", l30)
	}
	if len(inv.Standalone) != 0 {
		t.Errorf("standalone drives = %+v; every drive has a library", inv.Standalone)
	}
}

func TestLoadInventoryStandaloneDrive(t *testing.T) {
	d := t.TempDir()
	conf := "Library: 1 CHANNEL: 0 TARGET: 0 LUN: 0\n\n" +
		"Drive: 2 CHANNEL: 0 TARGET: 1 LUN: 0\n Library ID: 1 Slot: 1\n\n" +
		"Drive: 3 CHANNEL: 0 TARGET: 2 LUN: 0\n Library ID: 9 Slot: 1\n"
	if err := os.WriteFile(d+"/device.conf", []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, err := LoadInventory(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Libraries) != 1 || len(inv.Libraries[0].Drives) != 1 || inv.Libraries[0].Drives[0].ID != 2 {
		t.Errorf("libraries = %+v", inv.Libraries)
	}
	if len(inv.Standalone) != 1 || inv.Standalone[0].ID != 3 {
		t.Errorf("standalone = %+v; want drive 3", inv.Standalone)
	}
}

func TestLoadInventoryMissing(t *testing.T) {
	_, err := LoadInventory(t.TempDir())
	var de *DeviceConfError
	if !errors.As(err, &de) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want *DeviceConfError wrapping ErrNotExist", err)
	}
}

func TestLoadInventoryMalformed(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(d+"/device.conf", []byte("Library: 1 CHANNEL: 0 TARGET: 0 LUN: 0\n"), 0o644)
	os.WriteFile(d+"/library_contents.1", []byte("Slot x: A\n"), 0o644)
	inv, err := LoadInventory(d)
	if err != nil || len(inv.Libraries) != 1 || len(inv.Problems) != 1 ||
		inv.Problems[0].File != d+"/library_contents.1" || inv.Problems[0].Line != 1 {
		t.Errorf("inventory %+v, err %v; want library_contents.1 line 1 as a Problem", inv, err)
	}
}
