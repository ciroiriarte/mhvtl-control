package chleak

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/personality"
)

func TestFixed(t *testing.T) {
	for rel, want := range map[string]bool{
		"6.12.111+deb13-amd64":         false,
		"5.14.0-687.58.1.el9_8.x86_64": false,
		"7.2.11-1-default":             false,
		"7.3.0-rc6":                    false,
		"7.3.0-rc7":                    true,
		"7.3.0-0.rc6.20261001.fc44":    false,
		"7.3.0-0.rc7.20261008.fc44":    true,
		"7.3.0-070300rc6-generic":      false,
		"7.3.0-200.fc44.x86_64":        true,
		"7.3-rc8":                      true,
		"7.3.0-1-default":              true,
		"7.10.2":                       true,
		"8.0.0":                        true,
		"":                             false,
		"garbage":                      false,
	} {
		if got := Fixed(rel); got != want {
			t.Errorf("Fixed(%q) = %v, want %v", rel, got, want)
		}
	}
}

func writeRefcnt(t *testing.T, root string, n string) {
	t.Helper()
	dir := filepath.Join(root, "module", "mhvtl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refcnt"), []byte(n+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	libs := []config.Library{{ID: 10, Serial: "XYZZY_A"}, {ID: 30, Serial: "XYZZY_B"}}
	host := []hostids.Device{
		// Library 10's changer, ch bound.
		{Host: 7, Driver: "mhvtl", Serial: "XYZZY_A", Bound: "ch"},
		// The same serial on TCMU's loopback HBA: mhVTL never removes
		// its LUs, so it does not count.
		{Host: 8, Driver: "tcm_loopback", Serial: "XYZZY_A", Bound: "ch"},
		// Library 30's changer without ch, and a drive.
		{Host: 3, Driver: "mhvtl", Serial: "XYZZY_B"},
		{Host: 3, Target: 1, Driver: "mhvtl", Serial: "XYZZY_B1", Bound: "st"},
		// Some other changer with ch.
		{Host: 0, Driver: "ahci", Serial: "REAL1", Bound: "ch"},
	}
	root := t.TempDir()
	writeRefcnt(t, root, "12")
	st, err := Check(root, "6.12.111+deb13-amd64", libs, host)
	if err != nil {
		t.Fatal(err)
	}
	n := 12
	want := Status{Changers: []Changer{{Library: 10, HCTL: "7:0:0:0"}}, ModuleRefcount: &n, MhvtlDevices: 3}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("Check = %+v, want %+v", st, want)
	}
	if !st.Exposed(10) || st.Exposed(30) {
		t.Errorf("Exposed(10) = %v, Exposed(30) = %v", st.Exposed(10), st.Exposed(30))
	}

	// On a kernel with the fix nothing is exposed, ch or not.
	st, _ = Check(root, "7.3.0", libs, host)
	if !st.KernelFixed || st.Exposed(10) {
		t.Errorf("fixed kernel: %+v", st)
	}

	// References with no mhVTL device left are the leak itself.
	st, _ = Check(root, "6.12.111+deb13-amd64", libs, nil)
	if !st.Leaked || *st.ModuleRefcount != 12 {
		t.Errorf("no devices: %+v", st)
	}

	// mhvtl.ko not loaded (TCMU backend): no refcount, nothing leaked or
	// exposed.
	st, err = Check(t.TempDir(), "6.12.111+deb13-amd64", libs, host)
	if err != nil || st.ModuleRefcount != nil || st.Leaked || st.Exposed(10) {
		t.Errorf("no module: %+v, %v", st, err)
	}
}

// A personality may rewrite the unit serial its changer reports; the match
// goes by what the changer reports, as the changer locator's does.
func TestCheckPersonalitySerial(t *testing.T) {
	libs := []config.Library{
		{ID: 10, Vendor: "IBM", Product: "03584L32", Serial: "XYZZY_A"},
		{ID: 20, Vendor: "ADIC", Product: "Scalar i2000", Serial: "XYZZY_B"},
	}
	var host []hostids.Device
	for i, lib := range libs {
		reported := personality.ChangerUnitSerial(lib.Vendor, lib.Product, lib.Serial)
		if reported == lib.Serial {
			t.Fatalf("%s %s reports its serial unchanged; the test needs one that rewrites it", lib.Vendor, lib.Product)
		}
		host = append(host, hostids.Device{Host: 3, Target: i * 8, Driver: "mhvtl", Serial: reported, Bound: "ch"})
	}
	// An SPC changer has no vpd_pg80 in sysfs: listed by address alone.
	host = append(host, hostids.Device{Host: 3, Target: 16, Driver: "mhvtl", Bound: "ch"})
	st, err := Check(t.TempDir(), "6.12.111+deb13-amd64", libs, host)
	if err != nil {
		t.Fatal(err)
	}
	want := []Changer{{Library: 10, HCTL: "3:0:0:0"}, {Library: 20, HCTL: "3:0:8:0"}, {HCTL: "3:0:16:0"}}
	if !reflect.DeepEqual(st.Changers, want) {
		t.Errorf("changers = %+v, want %+v", st.Changers, want)
	}
}

// With ch loaded, a library ch is not bound to now is exposed too: ch binds
// to its changer when the library starts.
func TestExposedChLoaded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bus", "scsi", "drivers", "ch"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRefcnt(t, root, "0")
	st, err := Check(root, "6.12.111+deb13-amd64", []config.Library{{ID: 10, Serial: "XYZZY_A"}}, nil)
	if err != nil || !st.ChLoaded || !st.Exposed(10) || len(st.Changers) != 0 {
		t.Errorf("ch loaded, not bound: %+v, %v", st, err)
	}
	if st, _ = Check(root, "7.3.0", nil, nil); st.Exposed(10) {
		t.Errorf("fixed kernel: %+v", st)
	}
	if st, _ = Check(t.TempDir(), "6.12.111+deb13-amd64", nil, nil); st.ChLoaded || st.Exposed(10) {
		t.Errorf("ch not loaded: %+v", st)
	}
}
