package changer

import (
	"errors"
	"strings"
	"testing"

	"mhvtl-control/internal/scsi"
)

func TestLocatorFindBySerial(t *testing.T) {
	root := t.TempDir()
	fakeSysfs(t, root, "sg0", "8", "XYZZY_A")   // changer sharing a drive's serial
	fakeSysfs(t, root, "sg1", "1", "XYZZY_A1")  // tape drive, serial in sysfs
	fakeSysfs(t, root, "sg2", "", "XYZZY_A2")   // no type file: INQUIRY says tape
	fakeSysfs(t, root, "sg3", "1", "")          // tape drive, INQUIRY VPD 80h fallback
	fakeSysfs(t, root, "sg4", "8", "XYZZY_A1 ") // changer with the drive's serial

	open := func(path string) (Device, error) {
		return &scsi.FakeDevice{Respond: func(cdb, buf []byte) (int, error) {
			switch {
			case path == "/dev/sg2" && cdb[1]&1 == 0:
				inq := make([]byte, 36)
				inq[0] = scsi.PeripheralSequential
				return copy(buf, inq), nil
			case path == "/dev/sg3" && cdb[1]&1 == 1 && cdb[2] == 0x80:
				return copy(buf, vpd80("XYZZY_A3")), nil
			}
			return 0, illegal(0x24, 0x00)
		}}, nil
	}
	loc := &Locator{SysRoot: root, DevRoot: "/dev", Open: open}

	for serial, want := range map[string]string{
		"XYZZY_A1": "/dev/sg1",
		"XYZZY_A2": "/dev/sg2",
		"XYZZY_A3": "/dev/sg3",
	} {
		if p, err := loc.FindBySerial(serial, scsi.PeripheralSequential); err != nil || p != want {
			t.Errorf("tape %s = %q, %v; want %s", serial, p, err, want)
		}
	}
	if p, err := loc.FindBySerial("XYZZY_A", scsi.PeripheralMediumChanger); err != nil || p != "/dev/sg0" {
		t.Errorf("changer = %q, %v; want /dev/sg0", p, err)
	}

	_, err := loc.FindBySerial("XYZZY_A", scsi.PeripheralSequential)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no tape drive with serial") {
		t.Errorf("changer serial as tape: err = %v", err)
	}
	_, err = loc.FindBySerial("NOPE", scsi.PeripheralMediumChanger)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no medium changer with serial") {
		t.Errorf("unknown changer: err = %v", err)
	}
	if _, err := loc.FindBySerial("  ", scsi.PeripheralSequential); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty serial: err = %v, want ErrUnavailable", err)
	}
}
