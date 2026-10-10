package rawdisk

import (
	"strings"
	"testing"
)

// TestFormatRejectsArgvInjection verifies the argv-injection guards in
// ToolFormatter.Format: a non-absolute or flag-like device and an out-of-charset
// barcode are rejected before any subprocess runs.
func TestFormatRejectsArgvInjection(t *testing.T) {
	tf := ToolFormatter{ToolPath: "/bin/true"}
	cases := []struct {
		name, device, barcode, want string
	}{
		{"relative device", "sdb", "VOL001L5", "absolute path"},
		{"flag device", "-rf", "VOL001L5", "absolute path"},
		{"flag-smuggle device", "--barcode", "VOL001L5", "absolute path"},
		{"flag barcode", "/dev/sdb", "--help", "invalid barcode"},
		{"space barcode", "/dev/sdb", "VOL 1", "invalid barcode"},
		{"semicolon barcode", "/dev/sdb", "a;b", "invalid barcode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tf.Format(FormatRequest{Device: c.device, Barcode: c.barcode, Force: true})
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), c.want)
			}
		})
	}
}

// TestFormatArgsTerminatesOptions pins the argument ordering: flags first, then
// "--", then the positional device.
func TestFormatArgsTerminatesOptions(t *testing.T) {
	got := formatArgs("VOL001L5", "/dev/sdb")
	want := []string{"--barcode", "VOL001L5", "--", "/dev/sdb"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}
