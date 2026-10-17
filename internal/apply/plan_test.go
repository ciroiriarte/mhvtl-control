package apply

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
)

// planConf is a small device.conf: library 10 with drives 11 and 12,
// library 30 with drive 31, and drive 50, standalone (library 99 does not
// exist).
const planConf = `VERSION: 5

Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00
 Vendor identification: STK
 Product identification: L700
 Unit serial number: LIB10
 Backoff: 400

Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00
 Library ID: 10 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD8
 Unit serial number: DRV11

Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00
 Library ID: 10 Slot: 02
 Vendor identification: IBM
 Product identification: ULT3580-TD8
 Unit serial number: DRV12

Library: 30 CHANNEL: 00 TARGET: 08 LUN: 00
 Vendor identification: STK
 Product identification: L700
 Unit serial number: LIB30

Drive: 31 CHANNEL: 00 TARGET: 09 LUN: 00
 Library ID: 30 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD8
 Unit serial number: DRV31

Drive: 50 CHANNEL: 00 TARGET: 10 LUN: 00
 Library ID: 99 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD8
 Unit serial number: DRV50
`

const planMhvtlConf = "MHVTL_HOME_PATH=/opt/mhvtl\nCAPACITY=500\nVERBOSE=1\nVTL_DEBUG=0\n"

// stanza returns planConf's stanza for header, up to the next blank line.
func stanza(header string) string {
	i := strings.Index(planConf, header)
	j := strings.Index(planConf[i:], "\n\n")
	if j < 0 {
		return planConf[i:]
	}
	return planConf[i : i+j+2]
}

func planOf(t *testing.T, dc, mc string, contents map[int]bool) UnitPlan {
	t.Helper()
	inv := func(dc string) config.Inventory {
		inv, err := config.ReadInventory("/etc/mhvtl", func(name string) ([]byte, error) {
			if name == deviceConf {
				return []byte(dc), nil
			}
			return nil, fs.ErrNotExist
		})
		if err != nil {
			t.Fatal(err)
		}
		return inv
	}
	return derivePlan(conffile.ParseDeviceConf([]byte(planConf)), conffile.ParseDeviceConf([]byte(dc)),
		inv(planConf), inv(dc), contents,
		conffile.ParseMhvtlConf([]byte(planMhvtlConf)), conffile.ParseMhvtlConf([]byte(mc)))
}

func TestDerivePlan(t *testing.T) {
	newDrive := "Drive: 13 CHANNEL: 00 TARGET: 03 LUN: 00\n Library ID: 10 Slot: 03\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n Unit serial number: DRV13\n\n"
	newLibrary := "Library: 40 CHANNEL: 00 TARGET: 12 LUN: 00\n Vendor identification: STK\n Product identification: L700\n Unit serial number: LIB40\n\n" +
		"Drive: 41 CHANNEL: 00 TARGET: 13 LUN: 00\n Library ID: 40 Slot: 01\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n Unit serial number: DRV41\n"
	for _, tc := range []struct {
		name       string
		dc, mc     string
		contents   map[int]bool
		stop, strt string // StopOrder and StartOrder, unit numbers only
		notes      int
	}{
		{"nothing", planConf, planMhvtlConf, nil, "", "", 0},
		{"a comment", strings.Replace(planConf, "VERSION: 5\n", "VERSION: 5\n# edited\n", 1), planMhvtlConf, nil, "", "", 0},
		{"a library stanza", strings.Replace(planConf, " Backoff: 400", " Backoff: 500", 1), planMhvtlConf, nil, "L10", "L10", 0},
		{"library_contents", planConf, planMhvtlConf, map[int]bool{30: true}, "L30", "L30", 0},
		{"a drive stanza takes its library", strings.Replace(planConf, "DRV12", "DRV12X", 1), planMhvtlConf, nil, "L10 T12", "T12 L10", 0},
		{"a drive moved between libraries", strings.Replace(planConf, " Library ID: 10 Slot: 02", " Library ID: 30 Slot: 02", 1), planMhvtlConf, nil, "L10 T12 L30", "L10 T12 L30", 0},
		{"a drive added", planConf + "\n" + newDrive, planMhvtlConf, nil, "L10", "T13 L10", 0},
		{"a drive removed", strings.Replace(planConf, stanza("Drive: 12 "), "", 1), planMhvtlConf, nil, "L10 T12", "L10", 0},
		{"a library added", planConf + "\n" + newLibrary, planMhvtlConf, nil, "", "T41 L40", 0},
		{"a library deleted", strings.Replace(strings.Replace(planConf, stanza("Library: 30 "), "", 1), stanza("Drive: 31 "), "", 1), planMhvtlConf, nil, "L30 T31", "", 0},
		{"a standalone drive", strings.Replace(planConf, "DRV50", "DRV50X", 1), planMhvtlConf, nil, "T50", "T50", 0},
		{"VERBOSE restarts every instance", planConf, strings.Replace(planMhvtlConf, "VERBOSE=1", "VERBOSE=2", 1), nil, "L10 T11 T12 L30 T31 T50", "T11 T12 L10 T31 L30 T50", 0},
		{"VTL_DEBUG and CAPACITY restart nothing", planConf, strings.Replace(strings.Replace(planMhvtlConf, "VTL_DEBUG=0", "VTL_DEBUG=1", 1), "CAPACITY=500", "CAPACITY=900", 1), nil, "", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planOf(t, tc.dc, tc.mc, tc.contents)
			if got := short(p.StopOrder()); got != tc.stop {
				t.Errorf("stop order %q, want %q", got, tc.stop)
			}
			if got := short(p.StartOrder()); got != tc.strt {
				t.Errorf("start order %q, want %q", got, tc.strt)
			}
			if len(p.Notes) != tc.notes {
				t.Errorf("notes %q", p.Notes)
			}
			for _, u := range p.Units() {
				if !unitRE.MatchString(u) {
					t.Errorf("unit %q", u)
				}
			}
		})
	}
}

// short writes units as L<n> and T<n>.
func short(units []string) string {
	var out []string
	for _, u := range units {
		var n int
		if _, err := fmt.Sscanf(u, "vtllibrary@%d.service", &n); err == nil {
			out = append(out, fmt.Sprint("L", n))
		} else if _, err := fmt.Sscanf(u, "vtltape@%d.service", &n); err == nil {
			out = append(out, fmt.Sprint("T", n))
		} else {
			out = append(out, "?"+u)
		}
	}
	return strings.Join(out, " ")
}

func TestPlanBudget(t *testing.T) {
	// VERBOSE on planConf: 6 units down and up, 2 libraries read back.
	p := planOf(t, planConf, strings.Replace(planMhvtlConf, "VERBOSE=1", "VERBOSE=2", 1), nil)
	want := 6*unitStop + daemonReload + 6*unitStart + 2*readback + // forward
		6*unitStop + daemonReload + 6*unitStart + 2*readback + // rollback
		stopSlack
	if got := p.Budget(); got != want {
		t.Errorf("budget %v, want %v", got, want)
	}
	if err := checkBudget(p); err != nil {
		t.Error(err)
	}

	// New pre-filled cartridges add the media restart.
	p.NewBarcodes = 200
	if got := p.Budget(); got != want+mediaBase+200*mediaPerCartridge {
		t.Errorf("budget with media %v", got)
	}

	// 20 libraries of 2 drives each restarting does not fit.
	big := UnitPlan{StopDrives: map[int][]int{}, StartDrives: map[int][]int{}}
	for lib := 100; lib < 120; lib++ {
		big.Restart = append(big.Restart, lib)
		big.StopDrives[lib] = []int{lib*10 + 1, lib*10 + 2}
		big.StartDrives[lib] = []int{lib*10 + 1, lib*10 + 2}
	}
	if big.Budget() <= maxBudget {
		t.Fatalf("budget %v fits", big.Budget())
	}
	if err := checkBudget(big); err == nil || err.(*refusal).code != ErrBudget {
		t.Errorf("checkBudget: %v", err)
	}
	if maxBudget+stopSlack > 1800*time.Second {
		t.Errorf("maxBudget %v leaves no room under RuntimeMaxSec=1800", maxBudget)
	}
}

func TestNewBarcodes(t *testing.T) {
	cur := config.Inventory{Libraries: []config.Library{{ID: 10, Slots: []config.Slot{{Number: 1, Barcode: "A00001L8"}}, MAPs: []config.MAPSlot{{Number: 1, Barcode: "M00001L8"}}}}}
	next := config.Inventory{Libraries: []config.Library{{ID: 10, Slots: []config.Slot{
		{Number: 1, Barcode: "A00001L8"}, {Number: 2, Barcode: "M00001L8"}, {Number: 3, Barcode: "B00001L8"}, {Number: 4},
	}}}}
	if n := newBarcodes(cur, next); n != 1 {
		t.Errorf("new barcodes %d, want 1", n)
	}
}
