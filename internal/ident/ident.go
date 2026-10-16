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
// device.conf allows (mhvtl etc/device.conf: "Serial num max len is 10").
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
// rejects, in device.conf notation. It is a locally assigned NAA: the NAA
// field (the top four bits) is 3h and the other 60 bits are the locally
// administered value. SPC-4 r37 §7.8.6.6 (NAA designator format), NAA Locally
// Assigned designator format; https://www.t10.org/. The table and page
// numbers are not confirmed against a copy of the standard.
//
// mhVTL puts 5h in the NAA field on the wire for drives and several changers
// (mhvtl usr/vtllib.c update_vpd_83), so taken should compare the low 60 bits
// (hostids.SameNAA).
func (g Generator) NAA(taken func(naa []byte) bool) (string, error) {
	for n := uint32(0); n < maxTries; n++ {
		b := g.candidate("naa", n)[:8]
		b[0] = 0x30 | b[0]&0x0f
		if !taken(b) {
			return hostids.FormatNAA(b), nil
		}
	}
	return "", ErrExhausted
}
