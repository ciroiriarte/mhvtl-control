package validate

import (
	"fmt"
	"strconv"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/personality"
)

// MaxSerialLen is the longest unit serial number device.conf allows (mhvtl
// etc/device.conf: "Serial num max len is 10").
const MaxSerialLen = 10

// device is a library or drive of device.conf, for the identity checks.
type device struct {
	kind                 string // "library" or "drive"
	id                   int
	lib                  int // the library a drive belongs to, else its own ID
	channel, target, lun int
	serial, naa          string
}

func (d device) String() string { return fmt.Sprintf("%s %d", d.kind, d.id) }

// key is the element key of the device.
func (d device) key() string {
	if d.kind == "library" {
		return libKey(d.id)
	}
	return driveKey(d.id)
}

func devices(inv config.Inventory) []device {
	var devs []device
	drive := func(d config.Drive, lib int) device {
		return device{"drive", d.ID, lib, d.Channel, d.Target, d.LUN, d.Serial, d.NAA}
	}
	for _, l := range inv.Libraries {
		devs = append(devs, device{"library", l.ID, l.ID, l.Channel, l.Target, l.LUN, l.Serial, l.NAA})
		for _, d := range l.Drives {
			devs = append(devs, drive(d, l.ID))
		}
	}
	for _, d := range inv.Standalone {
		devs = append(devs, drive(d, 0))
	}
	return devs
}

// checkIdentities refuses duplicate IDs, channel/target/LUN addresses, serial
// numbers and NAAs in next, and serials or NAAs that another SCSI device of
// the host already reports. mhVTL's own devices on the host are those of cur,
// recognised by their mhvtl HBA address; any other device, real or virtual,
// counts.
func checkIdentities(r *Result, cur, next config.Inventory, host []hostids.Device) {
	devs := devices(next)

	ids := map[int]device{}
	ctl := map[[3]int]device{}
	serials := map[string]device{}
	type naaOwner struct {
		b []byte
		d device
	}
	var naas []naaOwner
	for _, d := range devs {
		if o, dup := ids[d.id]; dup {
			r.errorf(FamilyIdentity, d.lib, []string{o.key(), d.key()}, "%s and %s share ID %d; mhVTL addresses its daemons by ID", o, d, d.id)
		} else {
			ids[d.id] = d
		}
		k := [3]int{d.channel, d.target, d.lun}
		if o, dup := ctl[k]; dup {
			r.errorf(FamilyIdentity, d.lib, []string{o.key(), d.key()}, "%s and %s share CHANNEL %d TARGET %d LUN %d", o, d, d.channel, d.target, d.lun)
		} else {
			ctl[k] = d
		}

		switch {
		case d.serial == "":
			r.errorf(FamilyIdentity, d.lib, []string{d.key()}, "%s has no Unit serial number", d)
		case len(d.serial) > MaxSerialLen:
			r.errorf(FamilyIdentity, d.lib, []string{d.key()}, "%s: serial %q is %d characters; device.conf allows %d", d, d.serial, len(d.serial), MaxSerialLen)
		}
		if d.serial != "" {
			if o, dup := serials[d.serial]; dup {
				r.errorf(FamilyIdentity, d.lib, []string{o.key(), d.key()}, "%s and %s share serial %q", o, d, d.serial)
			} else {
				serials[d.serial] = d
			}
		}

		if d.naa == "" {
			r.warnf(FamilyIdentity, d.lib, "%s has no NAA; mhVTL then derives one from the end of the serial (usr/vtllib.c update_vpd_83), which other devices can repeat", d)
			continue
		}
		b, err := hostids.ParseNAA(d.naa)
		if err != nil {
			r.errorf(FamilyIdentity, d.lib, []string{d.key()}, "%s: %v", d, err)
			continue
		}
		for _, o := range naas {
			if hostids.SameNAA(o.b, b) {
				r.errorf(FamilyIdentity, d.lib, []string{o.d.key(), d.key()}, "%s and %s share NAA %s (mhVTL rewrites the NAA field of drives to 5h, so only the last 60 bits count)", o.d, d, d.naa)
				break
			}
		}
		naas = append(naas, naaOwner{b, d})
	}

	checkDriveLines(r, next, serials)

	// The drive identifier the library reports in READ ELEMENT STATUS can
	// cut the drive serial short.
	for _, l := range next.Libraries {
		p := personality.Lookup(l.Vendor, l.Product)
		for _, d := range l.Drives {
			if area := p.SerialArea(); len(d.Serial) > area {
				r.warnf(FamilyIdentity, l.ID, "library %d (%s) shows only %d characters of a drive serial; drive %d's %q is cut short in READ ELEMENT STATUS",
					l.ID, p.Name, area, d.ID, d.Serial)
			}
		}
	}

	ours := map[[3]int]bool{}
	for _, d := range devices(cur) {
		ours[[3]int{d.channel, d.target, d.lun}] = true
	}
	for _, h := range host {
		if h.Driver == hostids.MhvtlDriver && ours[[3]int{h.Channel, h.Target, h.LUN}] {
			continue
		}
		for _, d := range devs {
			if d.serial != "" && d.serial == h.Serial {
				r.stickyErrorf(FamilyIdentity, d.lib, []string{d.key()}, "%s: serial %q is already used by host device %s (%s)", d, d.serial, h.HCTL(), driverName(h))
			}
			b, err := hostids.ParseNAA(d.naa)
			if err != nil {
				continue
			}
			for _, hn := range h.NAAs {
				if hb, err := hostids.ParseNAA(hn); err == nil && hostids.SameNAA(b, hb) {
					r.stickyErrorf(FamilyIdentity, d.lib, []string{d.key()}, "%s: NAA %s is already used by host device %s (%s), which reports %s", d, d.naa, h.HCTL(), driverName(h), hn)
				}
			}
		}
	}
}

func driverName(h hostids.Device) string {
	if h.Driver == "" {
		return "unknown driver"
	}
	return h.Driver
}

// checkDriveLines checks the serials "Drive N: X" lines of library_contents
// give drive slots. Line numbers are mhvtl upstream master 6458223,
// https://github.com/markh794/mhvtl/blob/6458223/usr/cmd/vtllibrary.c:
// vtllibrary's main() calls customise_lu (:1711), whose personality init
// calls init_slot_info (e.g. usr/pm/default_smc_pm.c:61); __init_slot_info
// reads each Drive line (:1031-1039) and init_drive_slot (:851-878) passes a
// non-empty value to set_drive_serial (:621, called at :876), which sets the
// serial the changer reports in READ ELEMENT STATUS. Then main() calls
// update_drive_details (:1713; do_rereadconfig does the same at :1581-1583),
// which reads device.conf and, for this library's drive in that slot, sets
// the serial again from its "Unit serial number" (:719-721). vtltape answers
// INQUIRY with that device.conf serial (usr/cmd/vtltape.c:2154-2157). So:
//   - with a device.conf serial, the Drive line value is overridden: changer
//     and drive agree, and the value is ignored (a warning);
//   - without one, the changer reports the Drive line value and the drive
//     reports none: they disagree (blocking);
//   - for a slot no device.conf drive occupies, the changer reports a drive
//     with that serial and nothing behind it; the serial must still not
//     repeat another device's (blocking), and the empty drive is a warning.
func checkDriveLines(r *Result, next config.Inventory, serials map[string]device) {
	for _, l := range next.Libraries {
		bySlot := map[int]config.Drive{}
		for _, d := range l.Drives {
			bySlot[d.Slot] = d
		}
		file := "library_contents." + strconv.Itoa(l.ID)
		phantom := map[string]int{}
		for _, dl := range l.DriveLines {
			if dl.Serial == "" {
				continue
			}
			els := []string{lineKey(l.ID, dl.Number)}
			d, ok := bySlot[dl.Number]
			switch {
			case ok && d.Serial == "":
				r.errorf(FamilyIdentity, l.ID, append(els, driveKey(d.ID)),
					"%s line \"Drive %d: %s\" makes the changer report serial %q for drive %d, which has no Unit serial number in device.conf; the changer and the drive would report different serials",
					file, dl.Number, dl.Serial, dl.Serial, d.ID)
			case ok && d.Serial != dl.Serial:
				r.warnf(FamilyIdentity, l.ID, "%s line \"Drive %d: %s\" is ignored: device.conf gives drive %d serial %q, which vtllibrary applies afterwards",
					file, dl.Number, dl.Serial, d.ID, d.Serial)
			case !ok:
				r.warnf(FamilyIdentity, l.ID, "%s line \"Drive %d: %s\": no drive of device.conf sits in drive slot %d, so the changer reports a drive that does not exist",
					file, dl.Number, dl.Serial, dl.Number)
				if o, dup := serials[dl.Serial]; dup {
					r.errorf(FamilyIdentity, l.ID, append(els, o.key()), "%s line \"Drive %d: %s\" gives the changer a drive with serial %q, which %s already uses",
						file, dl.Number, dl.Serial, dl.Serial, o)
				} else if n, dup := phantom[dl.Serial]; dup {
					r.errorf(FamilyIdentity, l.ID, append(els, lineKey(l.ID, n)), "%s lines \"Drive %d\" and \"Drive %d\" both give the changer serial %q",
						file, n, dl.Number, dl.Serial)
				} else {
					phantom[dl.Serial] = dl.Number
				}
			}
		}
	}
}
