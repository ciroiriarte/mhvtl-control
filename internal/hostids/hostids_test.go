package hostids

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeSysfs builds <root>/class/scsi_device/<hctl>/device/{vpd_pg80,vpd_pg83}
// and <root>/class/scsi_host/host<H>/proc_name. An empty serial omits
// vpd_pg80; no naas omits vpd_pg83.
func fakeSysfs(t *testing.T, root, hctl, driver, serial string, naas ...[]byte) {
	t.Helper()
	dir := filepath.Join(root, "class", "scsi_device", hctl, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if serial != "" {
		pg := append([]byte{0x01, 0x80, 0, byte(len(serial) + 2)}, serial+"  "...)
		write(t, filepath.Join(dir, "vpd_pg80"), pg)
	}
	if len(naas) > 0 {
		// A T10 vendor ID designator first, as mhVTL and real drives send.
		body := append([]byte{0x02, 0x01, 0, 8}, "VENDOR  "...)
		for _, n := range naas {
			body = append(body, 0x01, 0x03, 0, byte(len(n)))
			body = append(body, n...)
		}
		write(t, filepath.Join(dir, "vpd_pg83"), append([]byte{0x01, 0x83, byte(len(body) >> 8), byte(len(body))}, body...))
	}
	host := "host" + strings.Split(hctl, ":")[0]
	hdir := filepath.Join(root, "class", "scsi_host", host)
	if err := os.MkdirAll(hdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(hdir, "proc_name"), []byte(driver+"\n"))
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	naaA := []byte{0x50, 0x22, 0x33, 0x44, 0xab, 0, 1, 0}
	naa6 := []byte{0x60, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	fakeSysfs(t, root, "3:0:1:0", "mhvtl", "XYZZY_A1", naaA)
	fakeSysfs(t, root, "0:0:0:0", "ahci", "WD-123", naa6)
	fakeSysfs(t, root, "1:0:0:0", "usb-storage", "") // no VPD at all
	dev := filepath.Join(root, "class", "scsi_device", "3:0:1:0", "device")
	write(t, filepath.Join(dev, "vendor"), []byte("IBM     \n"))
	write(t, filepath.Join(dev, "model"), []byte("ULT3580-TD8     \n"))
	if err := os.MkdirAll(filepath.Join(root, "class", "scsi_device", "not-an-hctl"), 0o755); err != nil {
		t.Fatal(err)
	}

	devs, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{Host: 0, Driver: "ahci", Serial: "WD-123", NAAs: []string{"60:01:02:03:04:05:06:07:08:09:0a:0b:0c:0d:0e:0f"}},
		{Host: 1, Driver: "usb-storage"},
		{Host: 3, Target: 1, Driver: "mhvtl", Serial: "XYZZY_A1", NAAs: []string{"50:22:33:44:ab:00:01:00"}, Vendor: "IBM", Model: "ULT3580-TD8"},
	}
	if !reflect.DeepEqual(devs, want) {
		t.Errorf("Read =\n%+v\nwant\n%+v", devs, want)
	}
	if got := devs[2].HCTL(); got != "3:0:1:0" {
		t.Errorf("HCTL = %s", got)
	}
}

func TestReadNoSCSI(t *testing.T) {
	devs, err := Read(t.TempDir())
	if err != nil || devs != nil {
		t.Errorf("Read on a host without scsi_device = %v, %v; want nil, nil", devs, err)
	}
}

func TestParseNAAsMalformed(t *testing.T) {
	for name, b := range map[string][]byte{
		"short":         {0x00, 0x83},
		"wrong page":    {0x00, 0x80, 0, 4, 1, 3, 0, 0},
		"overrun":       {0x00, 0x83, 0, 6, 1, 3, 0, 8, 0x50, 1},
		"zero length":   {0x00, 0x83, 0, 4, 1, 3, 0, 0},
		"length > page": {0x00, 0x83, 0, 40, 1, 3, 0, 2, 0x50, 1},
	} {
		got := parseNAAs(b)
		if name == "length > page" {
			// The page length is clamped to what was read.
			if !reflect.DeepEqual(got, []string{"50:01"}) {
				t.Errorf("%s: parseNAAs = %v", name, got)
			}
			continue
		}
		if got != nil {
			t.Errorf("%s: parseNAAs = %v; want none", name, got)
		}
	}
}

func TestParseNAA(t *testing.T) {
	b, err := ParseNAA("10:22:33:44:AB:00:00:00")
	if err != nil || FormatNAA(b) != "10:22:33:44:ab:00:00:00" {
		t.Errorf("ParseNAA = %v, %v", b, err)
	}
	for _, bad := range []string{"", "10:22:33", "10:22:33:44:ab:00:00:0g", "10:22:33:44:ab:00:00:100", "10:22:33:44:ab:00::00"} {
		if _, err := ParseNAA(bad); err == nil {
			t.Errorf("ParseNAA(%q) accepted", bad)
		}
	}
}

func TestSameNAA(t *testing.T) {
	a, _ := ParseNAA("10:22:33:44:ab:00:00:00")
	b, _ := ParseNAA("50:22:33:44:ab:00:00:00") // what mhVTL puts on the wire
	c, _ := ParseNAA("30:22:33:44:ab:00:01:00")
	if !SameNAA(a, b) {
		t.Error("NAAs differing only in the NAA field do not match")
	}
	if SameNAA(a, c) {
		t.Error("different NAAs match")
	}
	if SameNAA(a, append(a, 0, 0, 0, 0, 0, 0, 0, 0)) {
		t.Error("an 8-byte NAA matches a 16-byte one")
	}
}
