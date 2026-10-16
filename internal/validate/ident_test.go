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
		{"ID 0", func(i *config.Inventory) { i.Libraries[1].Drives[0].ID = 0 },
			[]string{"drive 0: ID 0 is out of range; mhVTL needs 1..1023"}, nil},
		{"ID at the limit", func(i *config.Inventory) { i.Libraries[1].Drives[0].ID = 1023 }, nil, nil},
		{"ID past the limit", func(i *config.Inventory) { i.Libraries[1].ID = 1024 },
			[]string{"library 1024: ID 1024 is out of range; mhVTL needs 1..1023"}, nil},
		{"negative channel", func(i *config.Inventory) { i.Libraries[1].Drives[0].Channel = -1 },
			[]string{"drive 31: CHANNEL -1 TARGET 9 LUN 0: none can be negative"}, nil},
		{"LUN at the limit", func(i *config.Inventory) { i.Libraries[1].Drives[0].LUN = 31 }, nil, nil},
		{"LUN past the limit", func(i *config.Inventory) { i.Libraries[1].Drives[0].LUN = 32 },
			[]string{"drive 31: LUN 32 is out of range; the mhvtl module answers LUNs 0..31 only"}, nil},
		{"duplicate channel/target/LUN", func(i *config.Inventory) { i.Libraries[1].Drives[0].Target = 1 },
			[]string{"drive 11 and drive 31 share CHANNEL 0 TARGET 1 LUN 0"}, nil},
		{"duplicate serial", func(i *config.Inventory) { i.Libraries[1].Drives[0].Serial = "XYZZY_A1" },
			[]string{`drive 11 and drive 31 share serial "XYZZY_A1"`}, nil},
		{"serial at the limit", func(i *config.Inventory) { i.Libraries[0].Drives[0].Serial = "ABCDEFGHIJ" }, nil, nil},
		{"serial past the limit", func(i *config.Inventory) { i.Libraries[0].Drives[0].Serial = "ABCDEFGHIJK" },
			[]string{`drive 11: serial "ABCDEFGHIJK" is 11 characters; mhvtl-control limits serials to 10 characters`}, nil},
		{"vendor and product at the limit", func(i *config.Inventory) {
			i.Libraries[0].Drives[0].Vendor, i.Libraries[0].Drives[0].Product = "ABCDEFGH", "ABCDEFGHIJKLMNOP"
		}, nil, nil},
		{"vendor past the limit", func(i *config.Inventory) { i.Libraries[0].Drives[0].Vendor = "ABCDEFGHI" },
			[]string{`drive 11: vendor "ABCDEFGHI" is 9 characters; INQUIRY has room for 8 and mhVTL cuts the rest`}, nil},
		{"product past the limit", func(i *config.Inventory) { i.Libraries[0].Product = "ABCDEFGHIJKLMNOPQ" },
			[]string{`library 10: product "ABCDEFGHIJKLMNOPQ" is 17 characters; INQUIRY has room for 16 and mhVTL cuts the rest`}, nil},
		{"no serial", func(i *config.Inventory) { i.Libraries[0].Serial = "" },
			[]string{"library 10 has no Unit serial number"}, nil},
		{"duplicate NAA", func(i *config.Inventory) { i.Libraries[1].Drives[0].NAA = "10:22:33:44:ab:00:01:00" },
			[]string{"drive 11 and drive 31 share NAA"}, nil},
		{"NAAs differing only in the NAA field", func(i *config.Inventory) { i.Libraries[1].Drives[0].NAA = "50:22:33:44:ab:00:01:00" },
			[]string{"drive 11 and drive 31 share NAA 50:22:33:44:ab:00:01:00"}, nil},
		{"malformed NAA", func(i *config.Inventory) { i.Libraries[0].Drives[0].NAA = "10:22:33" },
			[]string{"drive 11: NAA \"10:22:33\": want 8 hex bytes"}, nil},
		// The STK L-series has no VPD page 83h: its NAA is never sent.
		{"STK library NAA is not checked", func(i *config.Inventory) {
			i.Libraries[0].NAA = "10:22:33"
			i.Libraries[1].NAA = i.Libraries[1].Drives[0].NAA
		}, nil, nil},
		{"HP EML library NAA is checked", func(i *config.Inventory) {
			i.Libraries[1].Vendor, i.Libraries[1].Product = "HP", "EML E-Series"
			i.Libraries[1].NAA = i.Libraries[1].Drives[0].NAA
		}, []string{"library 30 and drive 31 share NAA"}, nil},
		{"missing NAA", func(i *config.Inventory) { i.Libraries[0].Drives[1].NAA = "" },
			nil, []string{"drive 12 has no NAA; mhVTL then derives 58:59:5a:5a:59:5f:41:32 from the end of the serial"}},
		{"derived NAAs that collide", func(i *config.Inventory) {
			i.Libraries[1].Drives[0].NAA, i.Libraries[1].Drives[0].Serial = "", "AAZZ_SAME1"
			i.Libraries[1].Drives[1].NAA, i.Libraries[1].Drives[1].Serial = "", "BBZZ_SAME1"
		}, []string{"drive 31 and drive 32 share NAA 5a:5a:5f:53:41:4d:45:31 (derived from its serial)"},
			[]string{"drive 31 has no NAA", "drive 32 has no NAA"}},
		{"a derived NAA equal to a configured one", func(i *config.Inventory) {
			i.Libraries[1].Drives[0].NAA = "3a:5a:5f:53:41:4d:45:31"
			i.Libraries[1].Drives[1].NAA, i.Libraries[1].Drives[1].Serial = "", "BBZZ_SAME1"
		}, []string{"drive 31 and drive 32 share NAA 5a:5a:5f:53:41:4d:45:31 (derived from its serial)"}, []string{"drive 32 has no NAA"}},
		{"standalone drive counts", func(i *config.Inventory) {
			i.Standalone = []config.Drive{{ID: 90, Target: 90, Serial: "XYZZY_B1", NAA: "40:00:00:00:00:00:00:90"}}
		}, []string{`drive 31 and drive 90 share serial "XYZZY_B1"`}, nil},
		{"Spectra Gator cuts an 11-character serial short", func(i *config.Inventory) {
			i.Libraries[1].Vendor, i.Libraries[1].Product = "SPECTRA", "GATOR"
			i.Libraries[1].Drives[0].Serial = "ABCDEFGHIJK"
		}, []string{`drive 31: serial "ABCDEFGHIJK" is 11 characters`},
			[]string{`library 30 (Spectra Gator) shows only 10 characters of a drive serial; drive 31's "ABCDEFGHIJK" is cut short`}},
		{"Spectra 215 reports no drive serial, so cuts none short", func(i *config.Inventory) {
			i.Libraries[1].Vendor, i.Libraries[1].Product = "SPECTRA", "215"
		}, nil, nil},
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
		// Library 10 is an STK L700, which sends no NAA.
		{"a real device with the STK library's device.conf NAA", cur, []hostids.Device{{Host: 5, Target: 3, Driver: "qla2xxx",
			NAAs: []string{"50:22:33:44:ab:00:00:00"}}}, nil},
		// Drive 11 exported again through tcm_loop: both its serial and its
		// NAA, so it is drive 11 itself.
		{"drive 11 re-exported through tcm_loopback", cur, []hostids.Device{{Host: 7, Target: 11, Driver: "tcm_loopback", Serial: "XYZZY_A1",
			NAAs: []string{"50:22:33:44:ab:00:01:00"}}}, nil},
		// "tcm_loop" is the fabric's bus name; no SCSI host has it as its
		// proc_name, so it gets no exemption.
		{"drive 11 on a host named after the tcm_loop bus", cur, []hostids.Device{{Host: 7, Target: 11, Driver: "tcm_loop", Serial: "XYZZY_A1",
			NAAs: []string{"50:22:33:44:ab:00:01:00"}}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 7:0:11:0 (tcm_loop)`,
				"drive 11: NAA 10:22:33:44:ab:00:01:00 is already used by host device 7:0:11:0 (tcm_loop)"}},
		{"the serial of drive 11 with another NAA", cur, []hostids.Device{{Host: 7, Target: 11, Driver: "tcm_loopback", Serial: "XYZZY_A1",
			NAAs: []string{"50:99:88:77:66:55:44:33"}}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 7:0:11:0 (tcm_loopback)`}},
		{"drive 11's serial and drive 12's NAA", cur, []hostids.Device{{Host: 7, Target: 11, Driver: "tcm_loopback", Serial: "XYZZY_A1",
			NAAs: []string{"50:22:33:44:ab:00:02:00"}}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 7:0:11:0 (tcm_loopback)`,
				"drive 12: NAA 10:22:33:44:ab:00:02:00 is already used by host device 7:0:11:0 (tcm_loopback)"}},
		// The same identity on another HBA (e.g. a target re-exporting it)
		// is a collision.
		// A second, default-configured mhVTL node's drive seen over iSCSI
		// (probe from the PR 2a re-review): the same serial and NAA, but not
		// a local re-export.
		{"another node's drive with the same identity over iSCSI", cur, []hostids.Device{{Host: 9, Target: 1, Driver: "iscsi_tcp", Serial: "XYZZY_A1",
			NAAs: []string{"50:22:33:44:ab:00:01:00"}}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 9:0:1:0 (iscsi_tcp)`,
				"drive 11: NAA 10:22:33:44:ab:00:01:00 is already used by host device 9:0:1:0 (iscsi_tcp)"}},
		{"same serial on another HBA", cur, []hostids.Device{{Host: 7, Target: 1, Driver: "iscsi_tcp", Serial: "XYZZY_A1"}},
			[]string{`drive 11: serial "XYZZY_A1" is already used by host device 7:0:1:0 (iscsi_tcp)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(tc.cur, clone(cur), nil, tc.host, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
		})
	}
}

// A re-export of a device of cur is that device, not a collision for it; it
// still collides with any other device of next that takes its identity.
func TestHostReexport(t *testing.T) {
	cur := shipped(t)
	host := []hostids.Device{{Host: 7, Target: 11, Driver: "tcm_loopback", Serial: "XYZZY_A1",
		NAAs: []string{"50:22:33:44:ab:00:01:00"}}}
	// Drive 11 is deleted and drive 31 takes its serial and NAA.
	next := clone(cur)
	d11 := next.Libraries[0].Drives[0]
	next.Libraries[0].Drives = next.Libraries[0].Drives[1:]
	next.Libraries[0].DriveLines = next.Libraries[0].DriveLines[1:]
	next.Libraries[1].Drives[0].Serial, next.Libraries[1].Drives[0].NAA = d11.Serial, d11.NAA
	r := Validate(cur, next, nil, host, Options{})
	expect(t, "errors", family(r.Errors, FamilyIdentity), []string{
		`drive 31: serial "XYZZY_A1" is already used by host device 7:0:11:0 (tcm_loopback)`,
		"drive 31: NAA 10:22:33:44:ab:00:01:00 is already used by host device 7:0:11:0 (tcm_loopback)",
	})
}

// A device with no NAA in device.conf reports one derived from its serial;
// the host check uses that.
func TestHostCollisionsDerivedNAA(t *testing.T) {
	cur := shipped(t)
	// An IBM LTO serial shorter than 10 is zero-filled on the left before
	// the NAA is taken from its last 8 bytes: "0000001234".
	cur.Libraries[0].Drives[0].Serial, cur.Libraries[0].Drives[0].NAA = "1234", ""
	// An HP EML library holds its serial padded to 14: "XYZZY_B       ".
	cur.Libraries[1].Vendor, cur.Libraries[1].Product, cur.Libraries[1].NAA = "HP", "EML E-Series", ""
	host := []hostids.Device{
		{Host: 5, Target: 3, Driver: "qla2xxx", NAAs: []string{"60:30:30:31:32:33:34:00"}},
		{Host: 5, Target: 4, Driver: "qla2xxx", NAAs: []string{"60:30:30:30:31:32:33:34"}},
		{Host: 5, Target: 5, Driver: "qla2xxx", NAAs: []string{"52:20:20:20:20:20:20:20"}},
	}
	r := Validate(cur, clone(cur), nil, host, Options{})
	expect(t, "errors", family(r.Errors, FamilyIdentity), []string{
		"drive 11: NAA 50:30:30:30:31:32:33:34 (derived from its serial) is already used by host device 5:0:4:0 (qla2xxx), which reports 60:30:30:30:31:32:33:34",
		"library 30: NAA 52:20:20:20:20:20:20:20 (derived from its serial) is already used by host device 5:0:5:0 (qla2xxx)",
	})
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

// An IBM 03584 changer sends no NAA, so its tcm_loop re-export is matched on
// serial, vendor and product (probe from the PR 2a re-review).
func TestChangerReexport(t *testing.T) {
	cur := gen(t)
	cur.Libraries[0].Vendor, cur.Libraries[0].Product = "IBM", "03584L32"
	for _, tc := range []struct {
		name   string
		h      hostids.Device
		errors []string
	}{
		{"the changer itself", hostids.Device{Host: 7, Target: 0, Driver: "tcm_loopback", Serial: "XYZZY_A", Vendor: "IBM", Model: "03584L32"}, nil},
		{"another product with its serial", hostids.Device{Host: 7, Target: 0, Driver: "tcm_loopback", Serial: "XYZZY_A", Vendor: "IBM", Model: "3573-TL"},
			[]string{`library 10: serial "XYZZY_A" is already used by host device 7:0:0:0 (tcm_loopback)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(cur, clone(cur), nil, []hostids.Device{tc.h}, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
		})
	}

	// The host reports the vendor and product mhVTL cut to their INQUIRY
	// widths; the over-long values themselves are a pre-existing error.
	long := clone(cur)
	long.Libraries[0].Vendor, long.Libraries[0].Product = "IBMXXXXXXX", "03584L32XXXXXXXXXX"
	h := hostids.Device{Host: 7, Target: 0, Driver: "tcm_loopback", Serial: "XYZZY_A", Vendor: "IBMXXXXX", Model: "03584L32XXXXXXXX"}
	r := Validate(long, clone(long), nil, []hostids.Device{h}, Options{})
	expect(t, "errors with an over-long vendor and product", family(r.Errors, FamilyIdentity), nil)
}
