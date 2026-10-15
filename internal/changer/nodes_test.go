package changer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mhvtld/internal/config"
)

// fakeNodes lays out what bench-debian13 shows for an STK L700 changer
// (sg10/sch0) and an IBM ULT3580 drive (sg8/st2): the sysfs class children
// of each sg device and the udev links under /dev/tape.
func fakeNodes(t *testing.T) (sys, dev string) {
	t.Helper()
	sys, dev = t.TempDir(), t.TempDir()
	fakeSysfs(t, sys, "sg10", "8", "XYZZY_A")
	fakeSysfs(t, sys, "sg8", "1", "00XYZZY_A1")
	fakeSysfs(t, sys, "sg0", "0", "") // a disk: no tape or changer children
	mkdirs := func(dir string, names ...string) {
		for _, n := range names {
			if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkdirs(filepath.Join(sys, "class/scsi_generic/sg10/device/scsi_changer"), "sch0")
	mkdirs(filepath.Join(sys, "class/scsi_generic/sg8/device/scsi_tape"),
		"nst2", "nst2a", "nst2l", "nst2m", "st2", "st2a", "st2l", "st2m")
	links := map[string]string{
		"by-id/scsi-00XYZZY_A1":                 "../../st2",
		"by-id/scsi-00XYZZY_A1-nst":             "../../nst2",
		"by-id/scsi-00XYZZY_A1-sg":              "../../sg8",
		"by-id/scsi-350223344ab000100-nst":      "../../nst2",
		"by-id/scsi-00XYZZY_A2-nst":             "../../nst0", // another drive
		"by-id/scsi-SSTK_L700_XYZZY_A":          "../../sg10",
		"by-id/scsi-SSTK_L700_XYZZY_A-changer":  "../../sg10",
		"by-path/pci-0000:00:1f.0-sas-0x1-nst":  "../../nst2",
		"by-path/pci-0000:00:1f.0-sas-0x1-st2a": "../../st20", // not a prefix match
	}
	for name, dst := range links {
		p := filepath.Join(dev, "tape", name)
		mkdirs(filepath.Dir(p), "")
		if err := os.Symlink(dst, p); err != nil {
			t.Fatal(err)
		}
	}
	return sys, dev
}

func TestNodes(t *testing.T) {
	sys, dev := fakeNodes(t)
	loc := &Locator{SysRoot: sys, DevRoot: dev}
	id := func(n string) string { return filepath.Join(dev, "tape/by-id", n) }

	got, err := loc.Nodes(filepath.Join(dev, "sg8"))
	if err != nil {
		t.Fatal(err)
	}
	want := Nodes{
		SG:  filepath.Join(dev, "sg8"),
		ST:  filepath.Join(dev, "st2"),
		NST: filepath.Join(dev, "nst2"),
		ByID: []string{
			id("scsi-00XYZZY_A1"), id("scsi-00XYZZY_A1-nst"), id("scsi-00XYZZY_A1-sg"),
			id("scsi-350223344ab000100-nst"),
		},
		ByPath: []string{filepath.Join(dev, "tape/by-path/pci-0000:00:1f.0-sas-0x1-nst")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drive nodes = %+v\nwant %+v", got, want)
	}

	got, err = loc.Nodes("/dev/sg10")
	if err != nil {
		t.Fatal(err)
	}
	want = Nodes{
		SG:   filepath.Join(dev, "sg10"),
		SCH:  filepath.Join(dev, "sch0"),
		ByID: []string{id("scsi-SSTK_L700_XYZZY_A"), id("scsi-SSTK_L700_XYZZY_A-changer")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changer nodes = %+v\nwant %+v", got, want)
	}

	if got, err := loc.Nodes("/dev/sg0"); err != nil || !reflect.DeepEqual(got, Nodes{SG: filepath.Join(dev, "sg0")}) {
		t.Errorf("disk nodes = %+v, %v; want sg only", got, err)
	}
}

// An override may name a by-id link rather than /dev/sgN, and a link named
// like an sg node is followed to the node it points at.
func TestNodesFollowsOverrideLink(t *testing.T) {
	sys, dev := fakeNodes(t)
	loc := &Locator{SysRoot: sys, DevRoot: dev}
	if err := os.WriteFile(filepath.Join(dev, "sg8"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loc.Nodes(filepath.Join(dev, "tape/by-id/scsi-00XYZZY_A1-sg"))
	if err != nil || got.ST != filepath.Join(dev, "st2") {
		t.Errorf("nodes via link = %+v, %v; want st2", got, err)
	}
	alias := filepath.Join(t.TempDir(), "sg10")
	if err := os.Symlink(filepath.Join(dev, "sg8"), alias); err != nil {
		t.Fatal(err)
	}
	if got, err := loc.Nodes(alias); err != nil || got.SG != filepath.Join(dev, "sg8") {
		t.Errorf("nodes via sg-named link = %+v, %v; want sg8", got, err)
	}
}

func TestNodesUnavailable(t *testing.T) {
	sys, dev := fakeNodes(t)
	loc := &Locator{SysRoot: sys, DevRoot: dev}
	if _, err := loc.Nodes("/dev/sg99"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Nodes(/dev/sg99): err = %v, want ErrUnavailable", err)
	}
	// A path that isn't an sg node is a configuration mistake, not absence.
	for _, p := range []string{filepath.Join(dev, "missing"), filepath.Join(dev, "tape/by-id/scsi-00XYZZY_A1-nst")} {
		if _, err := loc.Nodes(p); err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("Nodes(%s): err = %v, want a non-ErrUnavailable error", p, err)
		}
	}
}

func TestNodeResolver(t *testing.T) {
	sys, dev := fakeNodes(t)
	r := NodeResolver{
		Locator: &Locator{SysRoot: sys, DevRoot: dev},
		Drive: func(d config.Drive) (string, error) {
			if d.ID == 11 {
				return filepath.Join(dev, "sg8"), nil
			}
			return "", ErrUnavailable
		},
	}
	if n, err := r.LibraryNodes(config.Library{ID: 10, Serial: "XYZZY_A"}); err != nil || n.SCH != filepath.Join(dev, "sch0") {
		t.Errorf("library nodes = %+v, %v", n, err)
	}
	if n, err := r.DriveNodes(config.Drive{ID: 11}); err != nil || n.NST != filepath.Join(dev, "nst2") {
		t.Errorf("drive 11 nodes = %+v, %v", n, err)
	}
	if _, err := r.DriveNodes(config.Drive{ID: 12}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("drive 12: err = %v, want ErrUnavailable", err)
	}
	if _, err := (NodeResolver{Locator: r.Locator}).DriveNodes(config.Drive{ID: 11}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no drive locator: err = %v, want ErrUnavailable", err)
	}
}
