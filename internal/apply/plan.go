package apply

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
)

// UnitPlan is the set of mhVTL instances an apply stops and starts, derived
// by the helper from the current and the new files (plan 3.3, plan.go).
// Library and drive IDs index it; unit names are built from them only by
// libraryUnit and driveUnit.
type UnitPlan struct {
	// Restart are the libraries in both configurations that go down and
	// come back; Start the libraries added, Stop those deleted.
	Restart, Start, Stop []int
	// StopDrives and StartDrives are the drives stopped and started, by
	// the library they belong to in the current and in the new
	// configuration. A drive moved between libraries is stopped under its
	// old library and started under its new one.
	StopDrives, StartDrives map[int][]int
	// Standalone drives (a Library ID naming no library) have no library to
	// restart with them.
	StopStandalone, StartStandalone []int
	// NewBarcodes counts the pre-filled cartridges make_vtl_media is to
	// create: Slot barcodes of the new configuration that the current one
	// does not list anywhere.
	NewBarcodes int
	// Notes say what applies without a restart.
	Notes []string
}

func libraryUnit(id int) string { return fmt.Sprintf("vtllibrary@%d.service", id) }
func driveUnit(id int) string   { return fmt.Sprintf("vtltape@%d.service", id) }

// derivePlan derives the affected units (plan 3.3 table, with Phase 0
// amendment 1):
//
//   - a Library stanza or library_contents.N changed: vtllibrary@N;
//   - a Drive stanza changed: vtltape@D and its library, in the current and
//     the new configuration (vtllibrary reads its drives at start,
//     update_drive_details, mhvtl usr/cmd/vtllibrary.c:644-720 at
//     d65d25f,
//     https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/cmd/vtllibrary.c#L644-L720);
//   - a drive added or removed: start or stop vtltape@D, and restart its
//     library;
//   - a library added or deleted: start or stop all of its units;
//   - VERBOSE changed: every instance;
//   - VTL_DEBUG and CAPACITY: no restart.
//
// Any affected drive puts its library in the set (amendment 1): a drive
// restarted alone stays out of the library until the library's start-time
// registration.
//
// Stanzas compare by header address and attributes, so a comment edit
// restarts nothing. contents lists the libraries whose library_contents
// the request writes with other bytes, or deletes.
func derivePlan(curDC, nextDC *conffile.DeviceConf, cur, next config.Inventory, contents map[int]bool, curConf, nextConf *conffile.MhvtlConf) UnitPlan {
	curLibs, nextLibs := libraryIDs(cur), libraryIDs(next)
	restart := map[int]bool{}
	for id := range curLibs {
		if nextLibs[id] && (contents[id] || stanzaChanged(curDC, nextDC, "Library", id)) {
			restart[id] = true
		}
	}
	curDrives, nextDrives := drivesOf(cur), drivesOf(next)
	stopDrive, startDrive := map[int]bool{}, map[int]bool{}
	for id := range curDrives {
		if _, ok := nextDrives[id]; !ok || stanzaChanged(curDC, nextDC, "Drive", id) {
			stopDrive[id] = true
		}
	}
	for id := range nextDrives {
		if _, ok := curDrives[id]; !ok || stanzaChanged(curDC, nextDC, "Drive", id) {
			startDrive[id] = true
		}
	}
	verbose := confChanged(curConf, nextConf, "VERBOSE")
	if verbose {
		for id := range curLibs {
			if nextLibs[id] {
				restart[id] = true
			}
		}
		for id := range curDrives {
			if _, ok := nextDrives[id]; ok {
				stopDrive[id], startDrive[id] = true, true
			}
		}
	}
	// A library that stays and restarts takes none of its drives along
	// unless they are affected; an affected drive takes its library.
	for id := range stopDrive {
		if lib := curDrives[id]; curLibs[lib] && nextLibs[lib] {
			restart[lib] = true
		}
	}
	for id := range startDrive {
		if lib := nextDrives[id]; curLibs[lib] && nextLibs[lib] {
			restart[lib] = true
		}
	}

	p := UnitPlan{StopDrives: map[int][]int{}, StartDrives: map[int][]int{}}
	for id := range restart {
		p.Restart = append(p.Restart, id)
	}
	for id := range nextLibs {
		if !curLibs[id] {
			p.Start = append(p.Start, id)
		}
	}
	for id := range curLibs {
		if !nextLibs[id] {
			p.Stop = append(p.Stop, id)
		}
	}
	// A deleted library stops all its drives; an added one starts all.
	for id, lib := range curDrives {
		if curLibs[lib] && !nextLibs[lib] {
			stopDrive[id] = true
		}
	}
	for id, lib := range nextDrives {
		if nextLibs[lib] && !curLibs[lib] {
			startDrive[id] = true
		}
	}
	for id := range stopDrive {
		if lib := curDrives[id]; curLibs[lib] {
			p.StopDrives[lib] = append(p.StopDrives[lib], id)
		} else {
			p.StopStandalone = append(p.StopStandalone, id)
		}
	}
	for id := range startDrive {
		if lib := nextDrives[id]; nextLibs[lib] {
			p.StartDrives[lib] = append(p.StartDrives[lib], id)
		} else {
			p.StartStandalone = append(p.StartStandalone, id)
		}
	}
	for _, s := range [][]int{p.Restart, p.Start, p.Stop, p.StopStandalone, p.StartStandalone} {
		sort.Ints(s)
	}
	for _, m := range []map[int][]int{p.StopDrives, p.StartDrives} {
		for _, s := range m {
			sort.Ints(s)
		}
	}
	p.NewBarcodes = newBarcodes(cur, next)
	if confChanged(curConf, nextConf, "VTL_DEBUG") {
		p.Notes = append(p.Notes, "VTL_DEBUG applies on the next load of the mhvtl kernel module")
	}
	if confChanged(curConf, nextConf, "CAPACITY") {
		p.Notes = append(p.Notes, "CAPACITY applies to cartridges make_vtl_media creates from now on")
	}
	return p
}

// StopOrder is the order units go down: per library, vtllibrary@N first,
// then its drives (amendment 1: the library must not be left running
// against a drive daemon that is gone, mhvtl#272); standalone drives last.
func (p UnitPlan) StopOrder() []string {
	var out []string
	for _, lib := range append(append([]int{}, p.Restart...), p.Stop...) {
		out = append(out, libraryUnit(lib))
		for _, d := range p.StopDrives[lib] {
			out = append(out, driveUnit(d))
		}
	}
	for _, d := range p.StopStandalone {
		out = append(out, driveUnit(d))
	}
	return out
}

// StartOrder is the order units come up: per library its drives first,
// then vtllibrary@N, so the library registers drives that are running.
func (p UnitPlan) StartOrder() []string {
	var out []string
	libs := append(append([]int{}, p.Restart...), p.Start...)
	sort.Ints(libs)
	for _, lib := range libs {
		for _, d := range p.StartDrives[lib] {
			out = append(out, driveUnit(d))
		}
		out = append(out, libraryUnit(lib))
	}
	for _, d := range p.StartStandalone {
		out = append(out, driveUnit(d))
	}
	return out
}

// Units is the set of units the apply touches, sorted: what expect_units
// must equal.
func (p UnitPlan) Units() []string {
	set := map[string]bool{}
	for _, u := range append(p.StopOrder(), p.StartOrder()...) {
		set[u] = true
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// checkExpectUnits refuses an expect_units whose set differs from the
// helper's derivation (plan 3.2 item 12; AC 26).
func checkExpectUnits(expect []string, p UnitPlan) error {
	want := map[string]bool{}
	for _, u := range p.Units() {
		want[u] = true
	}
	got := map[string]bool{}
	for _, u := range expect {
		got[u] = true
	}
	var missing, extra []string
	for u := range want {
		if !got[u] {
			missing = append(missing, u)
		}
	}
	for u := range got {
		if !want[u] {
			extra = append(extra, u)
		}
	}
	if len(missing)+len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return refuse(ErrUnit, "expect_units differs from the helper's derivation: missing [%s], not affected [%s]",
		strings.Join(missing, " "), strings.Join(extra, " "))
}

// Budget terms (plan 3.1, RuntimeMaxSec=1800, and Phase 0 amendment 3).
const (
	// unitStop is TimeoutStopSec of vtllibrary@ and vtltape@
	// (etc/vtllibrary@.service.in:20, etc/vtltape@.service.in:17 at
	// d65d25f). vtllibrary exits only when idle, so a stop may run to the
	// SIGKILL; the spike measured up to 9.8 s for a busy library.
	unitStop = 10 * time.Second
	// unitStart is how long the helper waits for one unit to be active.
	unitStart = 30 * time.Second
	// readback is R2 for one library: READ ELEMENT STATUS, retried once on
	// the unit attention of amendment 4.
	readback = 2 * changerTimeout
	// daemonReload bounds systemctl daemon-reload.
	daemonReload = 10 * time.Second
	// mediaBase and mediaPerCartridge bound the mhvtl-media.service
	// restart: the spike measured about 12 ms per cartridge (S5); this
	// allows four times that.
	mediaBase         = 10 * time.Second
	mediaPerCartridge = 50 * time.Millisecond
	// stopSlack is the helper's own TimeoutStopSec, the time a SIGTERM
	// leaves to re-online and roll back.
	stopSlack = 60 * time.Second
	// maxBudget is RuntimeMaxSec less a margin (plan 3.1).
	maxBudget = 1700 * time.Second
)

// Budget is the worst-case time of the apply: forward, then a full
// rollback of everything it started, then the stop slack (plan 3.1, item
// 12).
func (p UnitPlan) Budget() time.Duration {
	stops := time.Duration(len(p.StopOrder())) * unitStop
	starts := time.Duration(len(p.StartOrder())) * unitStart
	rollbackStops := time.Duration(len(p.StartOrder())) * unitStop
	rollbackStarts := time.Duration(len(p.StopOrder())) * unitStart
	libsUp := time.Duration(len(p.Restart)+len(p.Start)) * readback
	libsBack := time.Duration(len(p.Restart)+len(p.Stop)) * readback
	var media time.Duration
	if p.NewBarcodes > 0 {
		media = mediaBase + time.Duration(p.NewBarcodes)*mediaPerCartridge
	}
	forward := stops + media + daemonReload + starts + libsUp
	rollback := rollbackStops + daemonReload + rollbackStarts + libsBack
	return forward + rollback + stopSlack
}

// checkBudget refuses a plan whose budget exceeds maxBudget.
func checkBudget(p UnitPlan) error {
	if b := p.Budget(); b > maxBudget {
		return refuse(ErrBudget, "the apply could take %v with its rollback, over the %v the helper's RuntimeMaxSec= leaves; split the change", b, maxBudget)
	}
	return nil
}

// stanzaChanged reports whether the kind/id stanza differs between a and b
// in a way mhVTL reads: present in one only, another address, or other
// attributes.
func stanzaChanged(a, b *conffile.DeviceConf, kind string, id int) bool {
	sa, oka := a.Stanza(kind, id)
	sb, okb := b.Stanza(kind, id)
	if oka != okb {
		return true
	}
	if !oka {
		return false
	}
	if sa.HasAddress != sb.HasAddress || sa.Channel != sb.Channel || sa.Target != sb.Target || sa.LUN != sb.LUN || len(sa.Attrs) != len(sb.Attrs) {
		return true
	}
	for i := range sa.Attrs {
		if sa.Attrs[i].Key != sb.Attrs[i].Key || sa.Attrs[i].Value != sb.Attrs[i].Value {
			return true
		}
	}
	return false
}

// drivesOf maps each drive ID to its Library ID, standalone drives
// included.
func drivesOf(inv config.Inventory) map[int]int {
	m := map[int]int{}
	for _, l := range inv.Libraries {
		for _, d := range l.Drives {
			m[d.ID] = l.ID
		}
	}
	for _, d := range inv.Standalone {
		m[d.ID] = d.LibraryID
	}
	return m
}

func confChanged(a, b *conffile.MhvtlConf, key string) bool {
	av, aok := a.Get(key)
	bv, bok := b.Get(key)
	return av != bv || aok != bok
}

func newBarcodes(cur, next config.Inventory) int {
	have := map[string]bool{}
	for _, l := range cur.Libraries {
		for _, s := range l.Slots {
			have[s.Barcode] = true
		}
		for _, s := range l.MAPs {
			have[s.Barcode] = true
		}
		for _, s := range l.PickerMedia {
			have[s.Barcode] = true
		}
	}
	n := 0
	for _, l := range next.Libraries {
		for _, s := range l.Slots {
			if s.Barcode != "" && !have[s.Barcode] {
				have[s.Barcode] = true
				n++
			}
		}
	}
	return n
}
