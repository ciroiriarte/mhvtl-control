package personality

import (
	"fmt"
	"strings"
)

// Citations in this file are mhVTL upstream master 852b78a,
// https://github.com/markh794/mhvtl/tree/852b78a7ae3add53ffdc418add5f14999762d788.

// Library serial buffers of vtllibrary: init_lu clamps the device.conf
// "Unit serial number" and holds it as snprintf(lu_serial_no, SCSI_SN_LEN,
// "%-14s"), and the vendor as "%-8s" in VENDOR_ID_LEN + 1
// (usr/cmd/vtllibrary.c:1310-1313 and :1326-1329,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/vtllibrary.c#L1310-L1329;
// SCSI_SN_LEN 16, include/vtllib.h:82).
const (
	scsiSNLen     = 16
	vendorIDLen   = 8
	maxDriveSNLen = 40
)

// UnitSerialPage returns the data of the VPD page 80h (Unit Serial Number)
// a changer of this model reports, after its 4-byte header, for the
// device.conf vendor and serial. The page length is len of the result.
//
// The personalities differ (Model.UnitSerialCall names the function):
//   - none: vtllibrary's own page, the held serial
//     (usr/cmd/vtllibrary.c:1406-1408,
//     https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/vtllibrary.c#L1406-L1408);
//   - update_ibm_3584_vpd_80: 22 bytes, "%-10.10s", NUL, NUL, the first
//     storage element address as "%04x"
//     (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/ibm_smc_pm.c#L168-L192);
//   - update_ibm_3100_vpd_80: 16 bytes, "%-12.12s" then "_LL0"
//     (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/ibm_smc_pm.c#L260-L280);
//   - update_eml_vpd_80 (HP and Overland): 18 bytes, "%-10.10s" then NULs
//     (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/hp_smc_pm.c#L32-L49,
//     https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/overland_pm.c#L11-L29);
//   - update_scalar_vpd_80: 24 bytes, the vendor "%-8.8s" then the serial
//     "%-16.16s"
//     (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/scalar_pm.c#L48-L71);
//   - update_stk_l_vpd_80(lu, n): n bytes, blank-filled, the serial cut to n
//     (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/pm/stklxx_pm.c#L35-L53).
func (m Model) UnitSerialPage(vendor, serial string) []byte {
	sn := heldLibrarySerial(serial)
	switch m.UnitSerialCall {
	case "":
		return []byte(sn)
	case "update_ibm_3584_vpd_80(lu)":
		d := make([]byte, 0x16)
		copy(d, cfmt(sn, 10))
		copy(d[12:], fmt.Sprintf("%04x", m.StartStorage))
		return d
	case "update_ibm_3100_vpd_80(lu)":
		d := make([]byte, 0x10)
		copy(d, cfmt(sn, 12))
		copy(d[12:], "_LL0")
		return d
	case "update_eml_vpd_80(lu)":
		d := make([]byte, 0x12)
		copy(d, cfmt(sn, 10))
		return d
	case "update_scalar_vpd_80(lu)":
		v := cfmt(cut(vendor, vendorIDLen), 8)
		return []byte(v + cfmt(sn, 16))
	case "update_stk_l_vpd_80(lu, 11)":
		return []byte(cfmt(sn, 11))
	case "update_stk_l_vpd_80(lu, 12)":
		return []byte(cfmt(sn, 12))
	}
	panic("personality: no VPD 80h layout for " + m.UnitSerialCall)
}

// UnitSerial is UnitSerialPage trimmed of blanks and NULs at both ends, as
// scsi.ParseVPDSerial returns a page: the serial to compare with what the
// changer reports. It is matched whole, never as a substring, so XYZZY_4
// does not find XYZZY_40.
func (m Model) UnitSerial(vendor, serial string) string {
	return trimVPD(m.UnitSerialPage(vendor, serial))
}

// ChangerUnitSerial returns the unit serial number the changer of a library
// with this device.conf vendor, product and serial reports, as
// scsi.ParseVPDSerial returns it.
func ChangerUnitSerial(vendor, product, serial string) string {
	return Lookup(vendor, product).UnitSerial(vendor, serial)
}

// DriveUnitSerialPage returns the data of the VPD page 80h a drive with
// this device.conf vendor, product and serial reports, after its 4-byte
// header. vtltape builds it for every drive personality alike, none
// replaces it (usr/cmd/vtltape.c:2274-2278,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/vtltape.c#L2274-L2278):
// the serial as held, clamped to MAX_DRIVE_SN_LEN and "%-10s"
// (:2169-2173,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/vtltape.c#L2169-L2173;
// MAX_DRIVE_SN_LEN 40, include/vtllib.h:88), after ibm_lto_serial_pad
// has zero-filled it on the left to 10 characters when the vendor is IBM,
// the product ULT3580-* or ULTRIUM-*, and the serial shorter than 10, as
// the real drives report it (usr/vtllib.c:2374-2395,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/vtllib.c#L2374-L2395).
// It compares the vendor_id ("%-8s") without its trailing blanks and the
// product_id ("%-16s") case-sensitively (:2177, :2183).
func DriveUnitSerialPage(vendor, product, serial string) []byte {
	held := fmt.Sprintf("%-10s", cut(serial, maxDriveSNLen))
	sn := strings.TrimRight(held, " ")
	v := strings.TrimRight(cut(vendor, vendorIDLen), " ")
	if v != "IBM" || !(strings.HasPrefix(product, "ULT3580-") || strings.HasPrefix(product, "ULTRIUM-")) ||
		sn == "" || len(sn) >= 10 {
		return []byte(held)
	}
	return []byte(strings.Repeat("0", 10-len(sn)) + sn)
}

// DriveUnitSerial is DriveUnitSerialPage as scsi.ParseVPDSerial reads it:
// the serial to compare with what the drive reports, whole.
func DriveUnitSerial(vendor, product, serial string) string {
	return trimVPD(DriveUnitSerialPage(vendor, product, serial))
}

// heldLibrarySerial is the serial as vtllibrary holds it: clamped to
// SCSI_SN_LEN, then "%-14s" in SCSI_SN_LEN bytes.
func heldLibrarySerial(serial string) string {
	return cut(fmt.Sprintf("%-14s", cut(serial, scsiSNLen)), scsiSNLen-1)
}

// cfmt is C's "%-n.ns": s cut to n, blank-padded to n.
func cfmt(s string, n int) string { return fmt.Sprintf("%-*s", n, cut(s, n)) }

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func trimVPD(b []byte) string { return strings.Trim(string(b), " \x00") }
