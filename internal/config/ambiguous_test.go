package config

import (
	"os"
	"strings"
	"testing"

	"mhvtl-control/internal/conffile"
)

// mhVTL still starts with a white-space line inside a record, though not
// as written: in a Drive record vtltape loads uninitialised compression
// settings from it (vtltape.c:2204). The read path accepts it (conffile
// flags it for the editor) and keeps the attribute that follows it in the
// record.
func TestParseDeviceConfAmbiguousBlank(t *testing.T) {
	b, err := os.ReadFile("../conffile/testdata/roundtrip/ambiguous/space-blank.device.conf")
	if err != nil {
		t.Fatal(err)
	}
	libs, drives, err := ParseDeviceConf(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 || libs[0].Product != "L700" || len(drives) != 2 {
		t.Errorf("libs = %+v, drives = %+v", libs, drives)
	}
}

// A conf_value field with no value is ignored: it neither clears an earlier
// value nor fails to parse. An empty Compression is not ignored by vtltape
// (vtltape.c:2204): it is an ambiguous Problem, and nothing is projected.
func TestParseDeviceConfEmptyValue(t *testing.T) {
	in := "Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n" +
		" Vendor identification: IBM\n Vendor identification:\n Compression:\n"
	if p := conffile.ParseDeviceConf([]byte(in)).Problems; len(p) == 0 || !strings.Contains(p[len(p)-1].Msg, "empty Compression") {
		t.Errorf("empty Compression not flagged: %v", p)
	}
	_, drives, err := ParseDeviceConf(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 1 || drives[0].Vendor != "IBM" || drives[0].CompressionFactor != 0 {
		t.Errorf("drives = %+v", drives)
	}
}

// The forms of NAA, Library ID and Compression that mhVTL reads differently
// from conf_value are projected as mhVTL reads them.
func TestParseDeviceConfScanfFields(t *testing.T) {
	const drv = "Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n"
	for _, tc := range []struct {
		in   string
		want Drive
	}{
		// "Compression: N" sets the factor, 0 when out of range.
		{drv + " Compression: 5\n", Drive{CompressionFactor: 5}},
		{drv + " Compression: factor 3 enabled 1\n Compression: 12\n", Drive{CompressionEnabled: true}},
		// A short NAA is dropped for mhVTL's default; an unreadable one is
		// ignored.
		{drv + " NAA: 10:22:33:44:ab:00:01:00\n NAA: 10:22\n", Drive{}},
		{drv + " NAA: 10:22:33:44:ab:00:01:00\n NAA: zz\n", Drive{NAA: "10:22:33:44:ab:00:01:00"}},
		// vtllibrary puts the drive in no slot without "Slot: N".
		{drv + " Library ID: 10\n", Drive{}},
	} {
		_, d, err := ParseDeviceConf(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		tc.want.ID, tc.want.Target = 11, 1
		if len(d) != 1 || d[0] != tc.want {
			t.Errorf("%q: drives = %+v, want %+v", tc.in, d, tc.want)
		}
	}
}
