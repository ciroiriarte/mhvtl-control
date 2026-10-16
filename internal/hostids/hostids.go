// Package hostids reads the identities the SCSI devices of this host report,
// real or virtual: the unit serial number (VPD page 80h) and the NAA
// designators (VPD page 83h), as the kernel caches them in sysfs. The
// configuration editor refuses a serial or NAA another device already uses.
package hostids

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mhvtl-control/internal/scsi"
)

// MhvtlDriver is the proc_name of mhVTL's pseudo HBA (mhvtl kernel/mhvtl.c
// mhvtl_driver_name).
const MhvtlDriver = "mhvtl"

// Device is one SCSI device of the host.
type Device struct {
	Host, Channel, Target, LUN int
	// Driver is the proc_name of the device's SCSI host, e.g. "mhvtl".
	Driver string
	// Serial is the VPD 80h unit serial number, trimmed; empty when the
	// device has none.
	Serial string
	// NAAs are the NAA designators of VPD 83h as lower-case hex bytes joined
	// by ':', the notation of device.conf.
	NAAs []string
}

// HCTL formats the device's address as H:C:T:L.
func (d Device) HCTL() string {
	return fmt.Sprintf("%d:%d:%d:%d", d.Host, d.Channel, d.Target, d.LUN)
}

// Read lists the devices under <sysRoot>/class/scsi_device. A host without
// SCSI devices yields none. A device whose VPD pages are absent or malformed
// is listed without those identities.
func Read(sysRoot string) ([]Device, error) {
	classDir := filepath.Join(sysRoot, "class", "scsi_device")
	ents, err := os.ReadDir(classDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, e := range ents {
		d, ok := parseHCTL(e.Name())
		if !ok {
			continue
		}
		devDir := filepath.Join(classDir, e.Name(), "device")
		if b, err := os.ReadFile(filepath.Join(devDir, "vpd_pg80")); err == nil {
			if s, err := scsi.ParseVPDSerial(b); err == nil {
				d.Serial = s
			}
		}
		if b, err := os.ReadFile(filepath.Join(devDir, "vpd_pg83")); err == nil {
			d.NAAs = parseNAAs(b)
		}
		if b, err := os.ReadFile(filepath.Join(sysRoot, "class", "scsi_host", "host"+strconv.Itoa(d.Host), "proc_name")); err == nil {
			d.Driver = strings.TrimSpace(string(b))
		}
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return less(devs[i], devs[j]) })
	return devs, nil
}

func less(a, b Device) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Channel != b.Channel {
		return a.Channel < b.Channel
	}
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	return a.LUN < b.LUN
}

// parseHCTL parses a scsi_device name, "H:C:T:L".
func parseHCTL(name string) (Device, bool) {
	f := strings.Split(name, ":")
	if len(f) != 4 {
		return Device{}, false
	}
	var n [4]int
	for i, s := range f {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return Device{}, false
		}
		n[i] = v
	}
	return Device{Host: n[0], Channel: n[1], Target: n[2], LUN: n[3]}, true
}

// designatorNAA is designator type 3h of a VPD 83h designation descriptor.
const designatorNAA = 0x3

// parseNAAs returns the NAA designators of a VPD page 83h response. Each
// designation descriptor is a 4-byte header (code set, designator type in the
// low nibble of byte 1, length in byte 3) and the designator; SPC-4 r37
// §7.8.6.1 (Device Identification VPD page), https://www.t10.org/ (table
// number not confirmed against a copy of the standard).
func parseNAAs(b []byte) []string {
	if len(b) < 4 || b[1] != 0x83 {
		return nil
	}
	end := min(4+int(binary.BigEndian.Uint16(b[2:4])), len(b))
	var naas []string
	for p := 4; p+4 <= end; {
		n := int(b[p+3])
		if p+4+n > end {
			break
		}
		if b[p+1]&0x0f == designatorNAA && n > 0 {
			naas = append(naas, FormatNAA(b[p+4:p+4+n]))
		}
		p += 4 + n
	}
	return naas
}

// FormatNAA writes an NAA designator the way device.conf does.
func FormatNAA(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = hex.EncodeToString([]byte{c})
	}
	return strings.Join(parts, ":")
}

// ParseNAA parses a device.conf NAA, eight hex bytes separated by ':' (mhvtl
// usr/cmd/vtllibrary.c and vtltape.c read it with "%02x:...:%02x").
func ParseNAA(s string) ([]byte, error) {
	f := strings.Split(strings.TrimSpace(s), ":")
	if len(f) != 8 {
		return nil, fmt.Errorf("NAA %q: want 8 hex bytes separated by ':'", s)
	}
	b := make([]byte, 8)
	for i, x := range f {
		v, err := strconv.ParseUint(x, 16, 8)
		if err != nil || len(x) == 0 || len(x) > 2 {
			return nil, fmt.Errorf("NAA %q: bad byte %q", s, x)
		}
		b[i] = byte(v)
	}
	return b, nil
}

// SameNAA reports whether two 8-byte NAA designators are the same identifier
// on the wire. The NAA field (the top nibble) is ignored: mhVTL rewrites it to
// 5h in the VPD 83h of every drive and of several changers (mhvtl
// usr/vtllib.c update_vpd_83, usr/pm/hp_smc_pm.c and overland_pm.c), so
// device.conf values that differ only there collide on the host. Designators
// that are not 8 bytes long never match.
func SameNAA(a, b []byte) bool {
	if len(a) != 8 || len(b) != 8 {
		return false
	}
	return binary.BigEndian.Uint64(a)&(1<<60-1) == binary.BigEndian.Uint64(b)&(1<<60-1)
}
