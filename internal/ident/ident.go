// Package ident generates the unit serial numbers and NAA identifiers of new
// libraries and drives. They derive from an appliance ID, itself derived from
// /etc/machine-id, so the same appliance proposes the same identities and two
// appliances propose different ones.
package ident

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"mhvtl-control/internal/hostids"
)

// AppID is mhvtl-control's application ID for the machine-specific appliance
// ID (b349f03c-51c5-4cc2-bed3-a88406a1fe06). It must never change: every
// identity generated so far derives from it.
var AppID = [16]byte{0xb3, 0x49, 0xf0, 0x3c, 0x51, 0xc5, 0x4c, 0xc2, 0xbe, 0xd3, 0xa8, 0x84, 0x06, 0xa1, 0xfe, 0x06}

// maxTries bounds the search for an identity nobody uses.
const maxTries = 4096

// ApplianceID reads a machine ID file (/etc/machine-id: 32 hex digits) and
// returns the ID specific to mhvtl-control.
func ApplianceID(machineIDPath string) ([16]byte, error) {
	b, err := os.ReadFile(machineIDPath)
	if err != nil {
		return [16]byte{}, err
	}
	var m [16]byte
	s := bytes.TrimSpace(b)
	if len(s) != 32 {
		return [16]byte{}, fmt.Errorf("%s: want 32 hex digits", machineIDPath)
	}
	if _, err := hex.Decode(m[:], s); err != nil {
		return [16]byte{}, fmt.Errorf("%s: %w", machineIDPath, err)
	}
	if m == ([16]byte{}) {
		return [16]byte{}, fmt.Errorf("%s: machine ID is all zeros", machineIDPath)
	}
	return AppSpecific(m, AppID), nil
}

// AppSpecific derives an application-specific ID from a machine ID the way
// systemd's sd_id128_get_machine_app_specific() does: HMAC-SHA256 keyed with
// the machine ID over the application ID, truncated to 128 bits and marked as
// a version 4 UUID. The machine ID itself cannot be recovered from it.
func AppSpecific(machine, app [16]byte) [16]byte {
	mac := hmac.New(sha256.New, machine[:])
	mac.Write(app[:])
	var id [16]byte
	copy(id[:], mac.Sum(nil))
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// Generator proposes identities for one appliance.
//
// It walks a fixed sequence and returns the first candidate the caller does
// not report as taken, so after a device is deleted its serial and NAA come
// back for the next device created. A host that still remembers the old
// device by those identifiers (an initiator's persistent binding, a backup
// application's drive table) would take the new device for it. Keeping a
// persisted high-water mark of the sequence, so identifiers are never
// reused, belongs to Phase 4, which owns the state written to disk.
type Generator struct {
	Appliance [16]byte
}

// ErrExhausted means every candidate tried was taken.
var ErrExhausted = errors.New("ident: no free identity found")

// candidate returns the n-th pseudo-random block for purpose.
func (g Generator) candidate(purpose string, n uint32) []byte {
	mac := hmac.New(sha256.New, g.Appliance[:])
	mac.Write([]byte(purpose))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], n)
	mac.Write(c[:])
	return mac.Sum(nil)
}

// Serial returns the first serial number of this appliance's sequence that
// taken rejects: "MV" and 8 base32 characters, 10 in all, the most
// device.conf allows (mhvtl etc/generate_device_conf.in:52: "Serial num max
// len is 10").
func (g Generator) Serial(taken func(serial string) bool) (string, error) {
	for n := uint32(0); n < maxTries; n++ {
		s := "MV" + base32.StdEncoding.EncodeToString(g.candidate("serial", n)[:5])
		if !taken(s) {
			return s, nil
		}
	}
	return "", ErrExhausted
}

// NAA returns the first NAA identifier of this appliance's sequence that taken
// rejects, in device.conf notation.
//
// It is an IEEE Registered NAA (NAA field 5h: the NAA nibble, a 24-bit OUI and
// a 36-bit vendor specific identifier; SPC-4 r37 §7.8.6.6, NAA designator
// format, https://www.t10.org/, table and page numbers not confirmed against
// a copy of the standard). mhVTL forces the NAA field to 5h on the wire
// whatever device.conf says (mhvtl usr/vtllib.c update_vpd_83, lines
// 2423-2484 on upstream master 993b193: the placeholder 51:23:45:60:... at
// 2449-2461, the rewrite at 2482-2483), so a locally assigned NAA 3h could
// never reach the host. Generating 5h keeps the wire value identical to what
// device.conf holds.
//
// The identifier carries mhVTL's placeholder OUI, 0x123456, from the
// 51:23:45:60:... bytes update_vpd_83 starts from. That OUI is not
// registered with the IEEE.
//
// Uniqueness across appliances rests on the 36 vendor specific bits, taken
// from an HMAC of the appliance ID and the sequence number. Among N
// identifiers from different appliances the chance of any collision is about
// N^2/2^37: under 1e-5 for 1,000 devices, 0.07% for 10,000, and 50% only near
// 300,000. Collisions with devices this host can see are excluded by taken,
// which should ignore the NAA field (hostids.SameNAA), as host devices and
// older device.conf entries may carry another one.
//
// Not every changer personality sends the NAA. Paths below are mhvtl
// usr/pm/ on upstream master 993b193
// (https://github.com/markh794/mhvtl/tree/993b193/usr/pm):
//   - IBM 03584 and TS3100 build their own VPD page 83h holding only a T10
//     vendor ID designator (ibm_smc_pm.c update_ibm_3584_vpd_83 :189,
//     update_ibm_3100_vpd_83 :277);
//   - Quantum Scalar does the same (scalar_pm.c update_scalar_vpd_83 :73);
//   - STK L-series remove page 83h (stklxx_pm.c update_stk_l_vpd_83 :55), and
//     the Spectra personalities never build one (spectra_pm.c).
//
// For those the NAA in device.conf never reaches the host. HP EML/MSL and
// Overland send it with the same 5h rewrite as the drives (hp_smc_pm.c
// update_eml_vpd_83 :114-115, overland_pm.c update_eml_vpd_83 :98-99).
func (g Generator) NAA(taken func(naa []byte) bool) (string, error) {
	for n := uint32(0); n < maxTries; n++ {
		c := g.candidate("naa", n)
		b := []byte{0x51, 0x23, 0x45, 0x60 | c[0]&0x0f, c[1], c[2], c[3], c[4]}
		if !taken(b) {
			return hostids.FormatNAA(b), nil
		}
	}
	return "", ErrExhausted
}
