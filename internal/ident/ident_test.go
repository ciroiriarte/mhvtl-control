package ident

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"mhvtl-control/internal/hostids"
)

// machineID is a made-up /etc/machine-id. Its app-specific ID was checked
// against systemd-id128 machine-id --app-specific on a real machine ID with
// the same code.
const machineID = "0123456789abcdef0123456789abcdef"

func writeMachineID(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(p, []byte(s), 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplianceID(t *testing.T) {
	id, err := ApplianceID(writeMachineID(t, machineID+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(id[:]); got != "f0e80ac11e5f417a81397f9204f51923" {
		t.Errorf("ApplianceID = %s", got)
	}
	for _, bad := range []string{"", "0123", machineID + "00", "0123456789abcdef0123456789abcdeg", "00000000000000000000000000000000"} {
		if _, err := ApplianceID(writeMachineID(t, bad)); err == nil {
			t.Errorf("ApplianceID(%q) accepted", bad)
		}
	}
	if _, err := ApplianceID(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
}

func generator(t *testing.T) Generator {
	t.Helper()
	id, err := ApplianceID(writeMachineID(t, machineID))
	if err != nil {
		t.Fatal(err)
	}
	return Generator{Appliance: id}
}

func TestSerial(t *testing.T) {
	g := generator(t)
	none := func(string) bool { return false }
	s1, err := g.Serial(none)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^MV[A-Z2-7]{8}$`).MatchString(s1) {
		t.Errorf("serial %q is not MV + 8 base32 characters", s1)
	}
	// Stable: the same appliance proposes the same serial.
	if again, _ := g.Serial(none); again != s1 {
		t.Errorf("second call = %s; want %s", again, s1)
	}
	// A taken serial moves to the next in the sequence.
	s2, err := g.Serial(func(s string) bool { return s == s1 })
	if err != nil || s2 == s1 || len(s2) != 10 {
		t.Errorf("after a collision = %q, %v", s2, err)
	}
	// Another appliance proposes another serial.
	other := Generator{Appliance: AppSpecific([16]byte{1}, AppID)}
	if o, _ := other.Serial(none); o == s1 {
		t.Errorf("two appliances both propose %s", o)
	}
	if _, err := g.Serial(func(string) bool { return true }); !errors.Is(err, ErrExhausted) {
		t.Errorf("all taken: err = %v", err)
	}
}

func TestNAA(t *testing.T) {
	g := generator(t)
	n1, err := g.NAA(func([]byte) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	b, err := hostids.ParseNAA(n1)
	if err != nil {
		t.Fatalf("NAA %q does not parse: %v", n1, err)
	}
	// NAA 5h with mhVTL's placeholder OUI 0x123456, as update_vpd_83 sends.
	if b[0] != 0x51 || b[1] != 0x23 || b[2] != 0x45 || b[3]>>4 != 6 {
		t.Errorf("NAA %s: want 51:23:45:6x:...", n1)
	}
	if again, _ := g.NAA(func([]byte) bool { return false }); again != n1 {
		t.Errorf("second call = %s; want %s", again, n1)
	}
	other := Generator{Appliance: AppSpecific([16]byte{1}, AppID)}
	if o, _ := other.NAA(func([]byte) bool { return false }); o == n1 {
		t.Errorf("two appliances both propose %s", o)
	}
	// A collision on the wire (same low 60 bits) moves on.
	n2, err := g.NAA(func(c []byte) bool { return hostids.SameNAA(c, b) })
	if err != nil || n2 == n1 {
		t.Errorf("after a collision = %q, %v", n2, err)
	}
	if _, err := g.NAA(func([]byte) bool { return true }); !errors.Is(err, ErrExhausted) {
		t.Errorf("all taken: err = %v", err)
	}
}
