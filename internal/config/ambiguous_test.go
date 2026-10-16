package config

import (
	"os"
	"strings"
	"testing"
)

// A white-space line inside a record still loads in mhVTL, so the read path
// accepts it (conffile flags it for the editor) and keeps the attribute that
// follows it in the record.
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

// A field with no value is ignored, as conf_value ignores it: it neither
// clears an earlier value nor fails to parse.
func TestParseDeviceConfEmptyValue(t *testing.T) {
	in := "Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n" +
		" Vendor identification: IBM\n Vendor identification:\n Compression:\n"
	_, drives, err := ParseDeviceConf(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(drives) != 1 || drives[0].Vendor != "IBM" {
		t.Errorf("drives = %+v", drives)
	}
}
