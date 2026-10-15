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
	libs, drives, err := ParseDeviceConf(open(t, "testdata/device.conf"))
	if err != nil {
		t.Fatal(err)
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
	_, d, err := ParseDeviceConf(strings.NewReader(in))
	if err != nil || len(d) != 1 {
		t.Fatalf("err=%v drives=%d", err, len(d))
	}
	if d[0].CompressionFactor != 9 || d[0].CompressionEnabled || d[0].Slot != 2 || d[0].LibraryID != 10 {
		t.Errorf("got %+v", d[0])
	}
}

func TestParseDeviceConfMalformed(t *testing.T) {
	cases := map[string]string{
		"bad header":       "Library: x CHANNEL: 0 TARGET: 0 LUN: 0\n",
		"short header":     "Drive: 1\n",
		"orphan attribute": " Vendor identification: IBM\n",
		"bad compression":  "Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Compression: bogus\n",
		"bad slot":         "Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n Library ID: 10 Slot: zz\n",
		"no colon":         "Drive: 1 CHANNEL: 0 TARGET: 1 LUN: 0\n garbage\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseDeviceConf(strings.NewReader(in)); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestParseLibraryContents(t *testing.T) {
	drives, pickers, maps, slots, err := ParseLibraryContents(open(t, "testdata/library_contents.sample"))
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 8 || len(pickers) != 1 || len(maps) != 4 || len(slots) != 32 {
		t.Fatalf("counts drives=%d pickers=%d maps=%d slots=%d", len(drives), len(pickers), len(maps), len(slots))
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

func TestParseLibraryContentsMalformed(t *testing.T) {
	for _, in := range []string{"Slot x: ABC\n", "Slot 1 ABC\n", "Bogus 1:\n", "Slot:\n"} {
		if _, _, _, _, err := ParseLibraryContents(strings.NewReader(in)); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestLoadInventory(t *testing.T) {
	inv, err := LoadInventory("testdata")
	if err != nil {
		t.Fatal(err)
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
	if _, err := LoadInventory(d); err == nil {
		t.Error("expected error for malformed library_contents")
	}
}
