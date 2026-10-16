package validate

import (
	"testing"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
)

func TestIdentities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*config.Inventory)
		errors []string
		warns  []string
	}{
		{"shipped", func(*config.Inventory) {}, nil, nil},
		{"duplicate ID", func(i *config.Inventory) { i.Libraries[1].Drives[0].ID = 11 },
			[]string{"drive 11 and drive 11 share ID 11"}, nil},
		{"library and drive share an ID", func(i *config.Inventory) { i.Libraries[1].ID = 11 },
			[]string{"drive 11 and library 11 share ID 11"}, nil},
		{"duplicate channel/target/LUN", func(i *config.Inventory) { i.Libraries[1].Drives[0].Target = 1 },
			[]string{"drive 11 and drive 31 share CHANNEL 0 TARGET 1 LUN 0"}, nil},
		{"duplicate serial", func(i *config.Inventory) { i.Libraries[1].Drives[0].Serial = "XYZZY_A1" },
			[]string{`drive 11 and drive 31 share serial "XYZZY_A1"`}, nil},
		{"serial at the limit", func(i *config.Inventory) { i.Libraries[0].Drives[0].Serial = "ABCDEFGHIJ" }, nil, nil},
		{"serial past the limit", func(i *config.Inventory) { i.Libraries[0].Drives[0].Serial = "ABCDEFGHIJK" },
			[]string{`drive 11: serial "ABCDEFGHIJK" is 11 characters; device.conf allows 10`}, nil},
		{"no serial", func(i *config.Inventory) { i.Libraries[0].Serial = "" },
			[]string{"library 10 has no Unit serial number"}, nil},
		{"duplicate NAA", func(i *config.Inventory) { i.Libraries[1].Drives[0].NAA = "10:22:33:44:ab:00:01:00" },
			[]string{"drive 11 and drive 31 share NAA"}, nil},
		{"NAAs differing only in the NAA field", func(i *config.Inventory) { i.Libraries[1].Drives[0].NAA = "50:22:33:44:ab:00:01:00" },
			[]string{"drive 11 and drive 31 share NAA 50:22:33:44:ab:00:01:00"}, nil},
		{"malformed NAA", func(i *config.Inventory) { i.Libraries[0].NAA = "10:22:33" },
			[]string{"library 10: NAA \"10:22:33\": want 8 hex bytes"}, nil},
		{"missing NAA", func(i *config.Inventory) { i.Libraries[0].Drives[1].NAA = "" },
			nil, []string{"drive 12 has no NAA"}},
		{"standalone drive counts", func(i *config.Inventory) {
			i.Standalone = []config.Drive{{ID: 90, Target: 90, Serial: "XYZZY_B1", NAA: "40:00:00:00:00:00:00:90"}}
		}, []string{`drive 31 and drive 90 share serial "XYZZY_B1"`}, nil},
		{"Spectra 215 hides drive serials", func(i *config.Inventory) {
			i.Libraries[1].Vendor, i.Libraries[1].Product = "SPECTRA", "215"
		}, nil, []string{
			`library 30 (Spectra Treefrog (215)) shows only 0 characters of a drive serial; drive 31's "XYZZY_B1"`,
			"drive 32's", "drive 33's", "drive 34's",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := shipped(t)
			next := clone(cur)
			tc.edit(&next)
			r := Validate(cur, next, nil, nil, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
			expect(t, "warnings", family(r.Warnings, FamilyIdentity), tc.warns)
		})
	}
}

func TestHostCollisions(t *testing.T) {
	cur := shipped(t)
	// The NAA field is 5h on the wire for the drives mhVTL runs.
	mhvtlDrive11 := hostids.Device{Host: 3, Target: 1, Driver: "mhvtl", Serial: "XYZZY_A1", NAAs: []string{"50:22:33:44:ab:00:01:00"}}
	for _, tc := range []struct {
		name   string
		cur    config.Inventory
		host   []hostids.Device
		errors []string
	}{
		{"mhVTL's own devices are not collisions", cur, []hostids.Device{mhvtlDrive11}, nil},
		{"a real disk with another serial", cur, []hostids.Device{{Host: 0, Driver: "ahci", Serial: "WD-123",
			NAAs: []string{"50:01:4e:e2:00:00:00:01"}}}, nil},
		{"a real device with the same serial", cur, []hostids.Device{{Host: 0, Driver: "ahci", Serial: "XYZZY_A1"}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 0:0:0:0 (ahci)`}},
		{"a real device with the same NAA", cur, []hostids.Device{{Host: 5, Target: 2, Driver: "qla2xxx",
			NAAs: []string{"50:22:33:44:ab:00:02:00"}}},
			[]string{"drive 12: NAA 10:22:33:44:ab:00:02:00 is already used by host device 5:0:2:0 (qla2xxx), which reports 50:22:33:44:ab:00:02:00"}},
		// An mhVTL device the current device.conf no longer has (not yet
		// restarted) still holds its identity.
		{"a stale mhVTL device", config.Inventory{}, []hostids.Device{mhvtlDrive11},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 3:0:1:0 (mhvtl)`,
				"drive 11: NAA 10:22:33:44:ab:00:01:00 is already used by host device 3:0:1:0 (mhvtl)"}},
		// The same identity on another HBA (e.g. a target re-exporting it)
		// is a collision.
		{"same serial on another HBA", cur, []hostids.Device{{Host: 7, Target: 1, Driver: "iscsi_tcp", Serial: "XYZZY_A1"}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 7:0:1:0 (iscsi_tcp)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(tc.cur, clone(cur), nil, tc.host, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
		})
	}
}

func TestHostCollisionsFromSysfs(t *testing.T) {
	devs, err := hostids.Read("testdata/sys")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("testdata/sys: %d devices, want 2", len(devs))
	}
	cur := shipped(t)
	r := Validate(cur, clone(cur), nil, devs, Options{})
	// 3:0:1:0 is mhVTL's drive 11; 0:0:0:0 is a disk that reuses drive 12's
	// serial.
	expect(t, "errors", family(r.Errors, FamilyIdentity),
		[]string{`drive 12: serial "XYZZY_A2" is already used by host device 0:0:0:0 (ahci)`})
}

// A "Drive N: X" line of library_contents gives the changer serial X for drive
// slot N until device.conf's Unit serial number replaces it.
func TestDriveLineSerials(t *testing.T) {
	line := func(n int, serial string) func(*config.Inventory) {
		return func(i *config.Inventory) {
			i.Libraries[0].DriveLines[n-1].Serial = serial
		}
	}
	for _, tc := range []struct {
		name   string
		edit   func(*config.Inventory)
		errors []string
		warns  []string
	}{
		{"same as device.conf", line(1, "XYZZY_A1"), nil, nil},
		{"device.conf overrides it", line(1, "OTHER1"), nil,
			[]string{`library_contents.10 line "Drive 1: OTHER1" is ignored: device.conf gives drive 11 serial "XYZZY_A1"`}},
		{"no device.conf serial: changer and drive disagree", func(i *config.Inventory) {
			line(1, "OTHER1")(i)
			i.Libraries[0].Drives[0].Serial = ""
		}, []string{
			"drive 11 has no Unit serial number",
			`library_contents.10 line "Drive 1: OTHER1" makes the changer report serial "OTHER1" for drive 11, which has no Unit serial number in device.conf; the changer and the drive would report different serials`,
		}, nil},
		{"empty drive slot with a free serial", func(i *config.Inventory) {
			i.Libraries[0].DriveLines = append(i.Libraries[0].DriveLines, config.DriveLine{Number: 5, Serial: "GHOST5"})
		}, nil, []string{`library_contents.10 line "Drive 5: GHOST5": no drive of device.conf sits in drive slot 5`}},
		{"empty drive slot repeating a drive serial", func(i *config.Inventory) {
			i.Libraries[0].DriveLines = append(i.Libraries[0].DriveLines, config.DriveLine{Number: 5, Serial: "XYZZY_B1"})
		}, []string{`library_contents.10 line "Drive 5: XYZZY_B1" gives the changer a drive with serial "XYZZY_B1", which drive 31 already uses`},
			[]string{"no drive of device.conf sits in drive slot 5"}},
		{"two empty drive slots with one serial", func(i *config.Inventory) {
			i.Libraries[0].DriveLines = append(i.Libraries[0].DriveLines,
				config.DriveLine{Number: 5, Serial: "GHOST"}, config.DriveLine{Number: 6, Serial: "GHOST"})
		}, []string{`library_contents.10 lines "Drive 5" and "Drive 6" both give the changer serial "GHOST"`},
			[]string{"drive slot 5", "drive slot 6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := shipped(t)
			next := clone(cur)
			tc.edit(&next)
			r := Validate(cur, next, nil, nil, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
			expect(t, "warnings", family(r.Warnings, FamilyIdentity), tc.warns)
		})
	}
}
