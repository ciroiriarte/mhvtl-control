package validate

import (
	"fmt"
	"strconv"
	"strings"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/personality"
)

// MaxSerialLen is the longest unit serial number mhvtl-control allows. mhVTL
// itself takes up to 40 characters (include/vtllib.h MAX_DRIVE_SN_LEN :88,
// upstream master c65bff0), but some of what it reports has a fixed 10-byte
// field: the HP EML/MSL and Overland changers copy exactly 10 bytes of the
// serial into VPD page 83h (usr/pm/hp_smc_pm.c:79, usr/pm/overland_pm.c:63),
// so a longer one is cut short there. etc/generate_device_conf.in:52 states
// the same limit.
const MaxSerialLen = 10

// MaxID is the highest library or drive ID. vtltape and vtllibrary exit on
// a -q queue ID outside 0..MAXPRIOR-1 (mhvtl include/utils/q.h:51, MAXPRIOR
// 1024; usr/cmd/vtltape.c:2451, usr/cmd/vtllibrary.c:1672, upstream master
// 993b193), and the kernel module refuses a minor of DEF_MAX_MINOR_NO (1024)
// or more (kernel/mhvtl.c:186, :1246). ID 0 is out too: the ID is the
// System V message type the daemon receives on, and msgsnd(2) fails with
// EINVAL on a type below 1; the daemons' own message says [1..1023].
const MaxID = 1023

// MaxLUNs is the number of LUNs the mhvtl module answers: a command to LUN
// DEF_MAX_LUNS (32) or above gets DID_NO_CONNECT (kernel/mhvtl.c:126, :195,
// :602 in 993b193). It is not a module parameter.
const MaxLUNs = 32

// MaxVendorLen and MaxProductLen are the INQUIRY vendor and product
// identification field widths. mhVTL cuts a longer device.conf value to
// them, logging only (conf_clamp_string, usr/vtllib.c:1150; called at
// usr/cmd/vtllibrary.c:1305 and :1316, usr/cmd/vtltape.c:2161 and :2167 in
// upstream c65bff0).
const (
	MaxVendorLen  = 8
	MaxProductLen = 16
)

// cut returns s cut to n bytes, as conf_clamp_string does.
func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// device is a library or drive of device.conf, for the identity checks.
type device struct {
	kind                 string // "library" or "drive"
	id                   int
	lib                  int // the library a drive belongs to, else its own ID
	channel, target, lun int
	vendor, product      string
	serial, naa          string
	// sendsNAA is whether the device reports an NAA in VPD page 83h. Every
	// drive does (vtltape calls update_vpd_83); a library only if its
	// personality does (personality.Model.SendsNAA).
	sendsNAA bool
}

func (d device) String() string { return fmt.Sprintf("%s %d", d.kind, d.id) }

// key is the element reference of the device.
func (d device) key() ElementRef {
	if d.kind == "library" {
		return libKey(d.id)
	}
	return driveKey(d.id)
}

// derivedNAA is the NAA mhVTL reports for a device whose device.conf entry
// has none: the last 8 bytes of the serial as the daemon holds it, with the
// NAA field rewritten to 5h (usr/vtllib.c update_vpd_83 :2471-2480; the
// changers' update_eml_vpd_83 do the same). Line numbers are upstream master
// c65bff0. vtltape holds a drive serial padded with %-10s
// (usr/cmd/vtltape.c:2157), with an IBM LTO serial shorter than 10 then
// zero-filled on the left to 10 (usr/vtllib.c ibm_lto_serial_pad :2371-2392,
// called at usr/cmd/vtltape.c:2256); vtllibrary holds its own serial padded
// with %-14s, cut to 15 (usr/cmd/vtllibrary.c:1301).
func derivedNAA(d device) []byte {
	sn := fmt.Sprintf("%-10s", d.serial)
	if d.kind == "library" {
		sn = fmt.Sprintf("%-14s", d.serial)
		if len(sn) > 15 {
			sn = sn[:15]
		}
	} else if strings.TrimRight(d.vendor, " ") == "IBM" &&
		(strings.HasPrefix(d.product, "ULT3580-") || strings.HasPrefix(d.product, "ULTRIUM-")) {
		if t := strings.TrimRight(sn, " "); len(t) > 0 && len(t) < 10 {
			sn = strings.Repeat("0", 10-len(t)) + t
		}
	}
	b := []byte(sn[len(sn)-8:])
	b[0] = b[0]&0x0f | 0x50
	return b
}

// naaText names the NAA a device reports, for messages.
func naaText(d device, b []byte) string {
	if d.naa != "" {
		return d.naa
	}
	return hostids.FormatNAA(b) + " (derived from its serial)"
}

func devices(inv config.Inventory) []device {
	var devs []device
	drive := func(d config.Drive, lib int) device {
		return device{"drive", d.ID, lib, d.Channel, d.Target, d.LUN, d.Vendor, d.Product, d.Serial, d.NAA, true}
	}
	for _, l := range inv.Libraries {
		sends := personality.Lookup(l.Vendor, l.Product).SendsNAA
		devs = append(devs, device{"library", l.ID, l.ID, l.Channel, l.Target, l.LUN, l.Vendor, l.Product, l.Serial, l.NAA, sends})
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
// counts, except a local re-export of a device of cur (see reexport), which
// does not collide with that device in next.
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
	wire := make([][]byte, len(devs)) // the NAA each device reports, if known
	for i, d := range devs {
		if d.id < 1 || d.id > MaxID {
			r.errorf(CodeIdentIDRange, d.lib, []ElementRef{d.key()}, "%s: ID %d is out of range; mhVTL needs 1..%d (the -q queue ID, the char device minor, and the message type, which msgsnd refuses at 0)", d, d.id, MaxID)
		}
		if d.channel < 0 || d.target < 0 || d.lun < 0 {
			r.errorf(CodeIdentAddressRange, d.lib, []ElementRef{d.key()}, "%s: CHANNEL %d TARGET %d LUN %d: none can be negative", d, d.channel, d.target, d.lun)
		}
		if d.lun >= MaxLUNs {
			r.errorf(CodeIdentAddressRange, d.lib, []ElementRef{d.key()}, "%s: LUN %d is out of range; the mhvtl module answers LUNs 0..%d only", d, d.lun, MaxLUNs-1)
		}
		if o, dup := ids[d.id]; dup {
			r.errorf(CodeIdentIDDup, d.lib, []ElementRef{o.key(), d.key()}, "%s and %s share ID %d; mhVTL addresses its daemons by ID", o, d, d.id)
		} else {
			ids[d.id] = d
		}
		k := [3]int{d.channel, d.target, d.lun}
		if o, dup := ctl[k]; dup {
			r.errorf(CodeIdentAddressDup, d.lib, []ElementRef{o.key(), d.key()}, "%s and %s share CHANNEL %d TARGET %d LUN %d", o, d, d.channel, d.target, d.lun)
		} else {
			ctl[k] = d
		}

		switch {
		case d.serial == "":
			r.errorf(CodeIdentSerialMissing, d.lib, []ElementRef{d.key()}, "%s has no Unit serial number", d)
		case len(d.serial) > MaxSerialLen:
			r.errorf(CodeIdentSerialLength, d.lib, []ElementRef{d.key()}, "%s: serial %q is %d characters; mhvtl-control limits serials to %d characters, the fixed field some changers report them in (e.g. VPD page 83h, usr/pm/hp_smc_pm.c:79)", d, d.serial, len(d.serial), MaxSerialLen)
		}
		if len(d.vendor) > MaxVendorLen {
			r.errorf(CodeIdentVendorLength, d.lib, []ElementRef{d.key()}, "%s: vendor %q is %d characters; INQUIRY has room for %d and mhVTL cuts the rest", d, d.vendor, len(d.vendor), MaxVendorLen)
		}
		if len(d.product) > MaxProductLen {
			r.errorf(CodeIdentProductLength, d.lib, []ElementRef{d.key()}, "%s: product %q is %d characters; INQUIRY has room for %d and mhVTL cuts the rest", d, d.product, len(d.product), MaxProductLen)
		}
		if d.serial != "" {
			if o, dup := serials[d.serial]; dup {
				r.errorf(CodeIdentSerialDup, d.lib, []ElementRef{o.key(), d.key()}, "%s and %s share serial %q", o, d, d.serial)
			} else {
				serials[d.serial] = d
			}
		}

		if !d.sendsNAA {
			// Its device.conf NAA never reaches the host.
			continue
		}
		var b []byte
		switch {
		case d.naa != "":
			var err error
			if b, err = hostids.ParseNAA(d.naa); err != nil {
				r.errorf(CodeIdentNAAInvalid, d.lib, []ElementRef{d.key()}, "%s: %v", d, err)
				continue
			}
		case d.serial == "":
			continue // already refused above
		default:
			b = derivedNAA(d)
			r.warnf(CodeIdentNAADerived, d.lib, []ElementRef{d.key()}, "%s has no NAA; mhVTL then derives %s from the end of the serial (usr/vtllib.c update_vpd_83), which other devices can repeat",
				d, hostids.FormatNAA(b))
		}
		wire[i] = b
		for _, o := range naas {
			if hostids.SameNAA(o.b, b) {
				r.errorf(CodeIdentNAADup, d.lib, []ElementRef{o.d.key(), d.key()}, "%s and %s share NAA %s (mhVTL rewrites the NAA field to 5h, so only the last 60 bits count)", o.d, d, naaText(d, b))
				break
			}
		}
		naas = append(naas, naaOwner{b, d})
	}

	checkDriveLines(r, next, serials)

	// The drive identifier the library reports in READ ELEMENT STATUS can
	// cut the drive serial short. A personality with no identifier area
	// reports no serial at all, so nothing is cut (usr/cmd/vtllibrary.c
	// set_drive_serial :631-632, upstream master c65bff0).
	for _, l := range next.Libraries {
		p := personality.Lookup(l.Vendor, l.Product)
		for _, d := range l.Drives {
			if area := p.SerialArea(); area > 0 && len(d.Serial) > area {
				r.warnf(CodeIdentSerialCut, l.ID, []ElementRef{libKey(l.ID), driveKey(d.ID)}, "library %d (%s) shows only %d characters of a drive serial; drive %d's %q is cut short in READ ELEMENT STATUS",
					l.ID, p.Name, area, d.ID, d.Serial)
			}
		}
	}

	ours := map[[3]int]bool{}
	curDevs := devices(cur)
	for _, d := range curDevs {
		ours[[3]int{d.channel, d.target, d.lun}] = true
	}
	for _, h := range host {
		if h.Driver == hostids.MhvtlDriver && ours[[3]int{h.Channel, h.Target, h.LUN}] {
			continue
		}
		self, isSelf := device{}, false
		if h.Driver == TCMLoopDriver {
			self, isSelf = reexport(h, curDevs)
		}
		for i, d := range devs {
			if isSelf && d.kind == self.kind && d.id == self.id {
				continue
			}
			if d.serial != "" && d.serial == h.Serial {
				r.stickyErrorf(CodeIdentHostSerial, d.lib, []ElementRef{d.key()}, "%s: serial %q is already used by host device %s (%s)", d, d.serial, h.HCTL(), driverName(h))
			}
			b := wire[i]
			if b == nil {
				continue
			}
			for _, hn := range h.NAAs {
				if hb, err := hostids.ParseNAA(hn); err == nil && hostids.SameNAA(b, hb) {
					r.stickyErrorf(CodeIdentHostNAA, d.lib, []ElementRef{d.key()}, "%s: NAA %s is already used by host device %s (%s), which reports %s", d, naaText(d, b), h.HCTL(), driverName(h), hn)
				}
			}
		}
	}
}

// TCMLoopDriver is the proc_name of the LIO loopback fabric's SCSI hosts
// (Linux drivers/target/loopback/tcm_loop.c:296, tcm_loop_driver_template,
// https://github.com/torvalds/linux/blob/v6.12/drivers/target/loopback/tcm_loop.c#L296).
// "tcm_loop" is the fabric's bus and driver name (:87, :93), not the host's.
const TCMLoopDriver = "tcm_loopback"

// reexport returns the device of cur that h, a tcm_loop device, is an export
// of: one whose serial and reported NAA h both reports. A changer that sends
// no NAA is matched on its serial and INQUIRY vendor and product instead,
// cut to the INQUIRY field widths as mhVTL reports them.
//
// Only tcm_loop counts: the loopback fabric presents LIO backstores of this
// host to this host, so a match there is mhVTL's device exported again
// (through a pscsi or TCMU backstore). A device on any other HBA, such as an
// iSCSI session, may be another mhVTL node whose default device.conf carries
// the same serials and NAAs, and multipath would merge the two; that stays a
// collision. Which backstore a tcm_loop device sits on is not checked (that
// would mean mapping the SCSI host to its loopback target port and LUN under
// /sys/kernel/config/target/loopback and following the LUN to
// core/{pscsi,user}_*), so the match rests on the identity alone.
func reexport(h hostids.Device, cur []device) (device, bool) {
	for _, c := range cur {
		if c.serial == "" || c.serial != h.Serial {
			continue
		}
		if !c.sendsNAA {
			if strings.TrimSpace(cut(c.vendor, MaxVendorLen)) == h.Vendor && strings.TrimSpace(cut(c.product, MaxProductLen)) == h.Model {
				return c, true
			}
			continue
		}
		b := derivedNAA(c)
		if c.naa != "" {
			var err error
			if b, err = hostids.ParseNAA(c.naa); err != nil {
				continue
			}
		}
		for _, hn := range h.NAAs {
			if hb, err := hostids.ParseNAA(hn); err == nil && hostids.SameNAA(b, hb) {
				return c, true
			}
		}
	}
	return device{}, false
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
			els := []ElementRef{lineKey(l.ID, dl.Number)}
			d, ok := bySlot[dl.Number]
			switch {
			case ok && d.Serial == "":
				r.errorf(CodeIdentDriveLineSerial, l.ID, append(els, driveKey(d.ID)),
					"%s line \"Drive %d: %s\" makes the changer report serial %q for drive %d, which has no Unit serial number in device.conf; the changer and the drive would report different serials",
					file, dl.Number, dl.Serial, dl.Serial, d.ID)
			case ok && d.Serial != dl.Serial:
				r.warnf(CodeIdentDriveLineIgnored, l.ID, append(els, driveKey(d.ID)), "%s line \"Drive %d: %s\" is ignored: device.conf gives drive %d serial %q, which vtllibrary applies afterwards",
					file, dl.Number, dl.Serial, d.ID, d.Serial)
			case !ok:
				r.warnf(CodeIdentDriveLineNoDrive, l.ID, els, "%s line \"Drive %d: %s\": no drive of device.conf sits in drive slot %d, so the changer reports a drive that does not exist",
					file, dl.Number, dl.Serial, dl.Number)
				if o, dup := serials[dl.Serial]; dup {
					r.errorf(CodeIdentDriveLineSerialDup, l.ID, append(els, o.key()), "%s line \"Drive %d: %s\" gives the changer a drive with serial %q, which %s already uses",
						file, dl.Number, dl.Serial, dl.Serial, o)
				} else if n, dup := phantom[dl.Serial]; dup {
					r.errorf(CodeIdentDriveLineSerialDup, l.ID, append(els, lineKey(l.ID, n)), "%s lines \"Drive %d\" and \"Drive %d\" both give the changer serial %q",
						file, n, dl.Number, dl.Serial)
				} else {
					phantom[dl.Serial] = dl.Number
				}
			}
		}
	}
}
