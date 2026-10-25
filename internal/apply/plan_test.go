package apply

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// planOf derives the plan from current device.conf cur and planMhvtlConf
// to device.conf dc and mhvtl.conf mc.
func planOf(t *testing.T, cur, dc, mc string, contents map[int]bool) UnitPlan {
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
	return derivePlan(conffile.ParseDeviceConf([]byte(cur)), conffile.ParseDeviceConf([]byte(dc)),
		inv(cur), inv(dc), contents,
		conffile.ParseMhvtlConf([]byte(planMhvtlConf)), conffile.ParseMhvtlConf([]byte(mc)), "")
}

func TestDerivePlan(t *testing.T) {
	newDrive := "Drive: 13 CHANNEL: 00 TARGET: 03 LUN: 00\n Library ID: 10 Slot: 03\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n Unit serial number: DRV13\n\n"
	newLibrary := "Library: 40 CHANNEL: 00 TARGET: 12 LUN: 00\n Vendor identification: STK\n Product identification: L700\n Unit serial number: LIB40\n\n" +
		"Drive: 41 CHANNEL: 00 TARGET: 13 LUN: 00\n Library ID: 40 Slot: 01\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n Unit serial number: DRV41\n"
	home30 := strings.Replace(planConf, " Unit serial number: LIB30\n", " Unit serial number: LIB30\n Home directory: /opt/b\n", 1)
	otherHome := strings.Replace(planMhvtlConf, "/opt/mhvtl", "/opt/other", 1)
	for _, tc := range []struct {
		name       string
		cur        string // the current device.conf
		dc, mc     string
		contents   map[int]bool
		stop, strt string // StopOrder and StartOrder, unit numbers only
		notes      int
	}{
		{"nothing", planConf, planConf, planMhvtlConf, nil, "", "", 0},
		{"a comment", planConf, strings.Replace(planConf, "VERSION: 5\n", "VERSION: 5\n# edited\n", 1), planMhvtlConf, nil, "", "", 0},
		{"a library stanza", planConf, strings.Replace(planConf, " Backoff: 400", " Backoff: 500", 1), planMhvtlConf, nil, "L10", "L10", 0},
		{"library_contents", planConf, planConf, planMhvtlConf, map[int]bool{30: true}, "L30", "L30", 0},
		{"a drive stanza takes its library", planConf, strings.Replace(planConf, "DRV12", "DRV12X", 1), planMhvtlConf, nil, "L10 T12", "T12 L10", 0},
		{"a drive moved between libraries", planConf, strings.Replace(planConf, " Library ID: 10 Slot: 02", " Library ID: 30 Slot: 02", 1), planMhvtlConf, nil, "L10 T12 L30", "L10 T12 L30", 0},
		{"a drive added", planConf, planConf + "\n" + newDrive, planMhvtlConf, nil, "L10", "T13 L10", 0},
		{"a drive removed", planConf, strings.Replace(planConf, stanza("Drive: 12 "), "", 1), planMhvtlConf, nil, "L10 T12", "L10", 0},
		{"a library added", planConf, planConf + "\n" + newLibrary, planMhvtlConf, nil, "", "T41 L40", 0},
		{"a library deleted", planConf, strings.Replace(strings.Replace(planConf, stanza("Library: 30 "), "", 1), stanza("Drive: 31 "), "", 1), planMhvtlConf, nil, "L30 T31", "", 0},
		{"a standalone drive", planConf, strings.Replace(planConf, "DRV50", "DRV50X", 1), planMhvtlConf, nil, "T50", "T50", 0},
		{"VERBOSE restarts every instance", planConf, planConf, strings.Replace(planMhvtlConf, "VERBOSE=1", "VERBOSE=2", 1), nil, "L10 T11 T12 L30 T31 T50", "T11 T12 L10 T31 L30 T50", 0},
		{"MHVTL_BACKEND restarts every instance", planConf, planConf, planMhvtlConf + "MHVTL_BACKEND=tcmu\n", nil, "L10 T11 T12 L30 T31 T50", "T11 T12 L10 T31 L30 T50", 0},
		{"a library's home directory takes its drives", planConf, strings.Replace(planConf, " Unit serial number: LIB10\n", " Unit serial number: LIB10\n Home directory: /opt/a\n", 1), planMhvtlConf, nil, "L10 T11 T12", "T11 T12 L10", 0},
		{"MHVTL_HOME_PATH restarts libraries without a home, and standalone drives", planConf, planConf, strings.Replace(planMhvtlConf, "/opt/mhvtl", "/opt/other", 1), nil, "L10 T11 T12 L30 T31 T50", "T11 T12 L10 T31 L30 T50", 0},
		{"MHVTL_HOME_PATH and a library given its own home", planConf, home30, otherHome, nil, "L10 T11 T12 L30 T31 T50", "T11 T12 L10 T31 L30 T50", 0},
		{"MHVTL_HOME_PATH leaves a library with its own home", home30, home30, otherHome, nil, "L10 T11 T12 T50", "T11 T12 L10 T50", 0},
		{"a library deleted, its drive's stanza kept", planConf, strings.Replace(planConf, stanza("Library: 30 "), "", 1), planMhvtlConf, nil, "L30 T31", "T31", 0},
		{"a library adopts a standalone drive", planConf, planConf + "\nLibrary: 99 CHANNEL: 00 TARGET: 14 LUN: 00\n Vendor identification: STK\n Product identification: L700\n Unit serial number: LIB99\n", planMhvtlConf, nil, "T50", "T50 L99", 0},
		{"VTL_DEBUG and CAPACITY restart nothing", planConf, planConf, strings.Replace(strings.Replace(planMhvtlConf, "VTL_DEBUG=0", "VTL_DEBUG=1", 1), "CAPACITY=500", "CAPACITY=900", 1), nil, "", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planOf(t, tc.cur, tc.dc, tc.mc, tc.contents)
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
			// A drive in both configurations stops exactly when it starts.
			stops, starts := map[string]bool{}, map[string]bool{}
			for _, u := range p.StopOrder() {
				stops[u] = true
			}
			for _, u := range p.StartOrder() {
				starts[u] = true
			}
			curD, nextD := map[string]bool{}, map[string]bool{}
			for _, d := range []string{"11", "12", "31", "50"} {
				curD["vtltape@"+d+".service"] = true
			}
			for u := range curD {
				if strings.Contains(tc.dc, "Drive: "+strings.TrimSuffix(strings.TrimPrefix(u, "vtltape@"), ".service")+" ") {
					nextD[u] = true
				}
			}
			for u := range curD {
				if nextD[u] && stops[u] != starts[u] {
					t.Errorf("%s: stopped %v, started %v", u, stops[u], starts[u])
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
	// VERBOSE on planConf: libraries 10 and 30 with drives 11, 12 and 31,
	// and standalone drive 50.
	p := planOf(t, planConf, planConf, strings.Replace(planMhvtlConf, "VERBOSE=1", "VERBOSE=2", 1), nil)
	units := 2*libraryStop + 4*driveStop + 2*libraryStart + 4*driveStart
	want := bayMonitorTime + 2*guardLibrary + 3*guardDrive + daemonReload + units + startWait + 2*readback + // forward
		2*guardLibrary + 3*guardDrive + daemonReload + units + 2*readback + // rollback
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

	libraries := func(n int, verbose bool) UnitPlan {
		p := UnitPlan{StopDrives: map[int][]int{}, StartDrives: map[int][]int{}, Verbose: verbose}
		for lib := 100; lib < 100+n; lib++ {
			p.Restart = append(p.Restart, lib)
			p.StopDrives[lib] = []int{lib*10 + 1, lib*10 + 2}
			p.StartDrives[lib] = []int{lib*10 + 1, lib*10 + 2}
			p.GuardDrives += 2
			p.RollbackGuardDrives += 2
		}
		return p
	}
	// VERBOSE on 4 libraries of 2 drives each fits.
	if err := checkBudget(libraries(4, true)); err != nil {
		t.Errorf("4 libraries: %v", err)
	}
	// 20 do not; for VERBOSE the refusal says to set it by hand.
	big := libraries(20, false)
	if big.Budget() <= maxBudget {
		t.Fatalf("budget %v fits", big.Budget())
	}
	if err := checkBudget(big); err == nil || err.(*refusal).code != ErrBudget || !strings.Contains(err.Error(), "split the change") {
		t.Errorf("checkBudget: %v", err)
	}
	if err := checkBudget(libraries(20, true)); err == nil || !strings.Contains(err.Error(), "VERBOSE restarts every instance") {
		t.Errorf("checkBudget VERBOSE: %v", err)
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
	if n := newBarcodes(cur, next, ""); n != 1 {
		t.Errorf("new barcodes %d, want 1", n)
	}
	// B00001L8 a bay cartridge, a symlink in the home: not new.
	home := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(home, "B00001L8")); err != nil {
		t.Fatal(err)
	}
	if n := newBarcodes(cur, next, home); n != 0 {
		t.Errorf("new barcodes with a bay cartridge %d, want 0", n)
	}
}
