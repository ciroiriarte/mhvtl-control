package validate

import (
	"fmt"

	"mhvtl-control/internal/config"
)

// displaced is a cartridge of cur that next drops from the library.
type displaced struct {
	barcode, where string
}

// checkRemovals refuses to let a cartridge leave a library without a
// Disposition saying where it goes. A cartridge leaves when cur has it, in
// library_contents (slot, MAP slot or picker) or live, and next has it
// nowhere in the same library: in no slot, MAP slot or picker of the file,
// and in no drive live. Whether its slot was deleted, cleared or became a
// gap makes no difference; otherwise it would silently leave the library at
// the next restart. A cartridge that moves within the library needs none.
// Deleting a whole library is not covered here: its cartridges stay in its
// home (AC 12).
func checkRemovals(r *Result, cur, next config.Inventory, live *Live, opts Options) {
	byLib := map[int][]Disposition{}
	for _, d := range opts.Dispositions {
		byLib[d.Library] = append(byLib[d.Library], d)
	}
	for _, id := range sortedKeys(byLib) {
		_, inCur := findLibrary(cur, id)
		_, inNext := findLibrary(next, id)
		if !inCur || !inNext {
			r.errorf(FamilyRemoval, id, []string{libKey(id)}, "dispositions name library %d, which is not in both the current and the new configuration", id)
		}
	}

	for _, lib := range next.Libraries {
		old, ok := findLibrary(cur, lib.ID)
		if !ok {
			continue
		}
		lp, haveLive := live.library(lib.ID)
		keptSlots, keptMAPs := map[int]string{}, map[int]string{}
		highest := 0
		for _, s := range lib.Slots {
			keptSlots[s.Number] = s.Barcode
			highest = max(highest, s.Number)
		}
		// vtllibrary makes an empty slot for each number the Slot lines skip
		// (usr/cmd/vtllibrary.c:1054-1064, upstream master c65bff0).
		for n := 1; n < highest; n++ {
			if _, ok := keptSlots[n]; !ok {
				keptSlots[n] = ""
			}
		}
		for _, m := range lib.MAPs {
			keptMAPs[m.Number] = m.Barcode
		}
		present := barcodes(listed(lib))
		for _, b := range lp.Drives {
			if b != "" {
				present[b] = true
			}
		}

		var gone []displaced
		seen := map[string]bool{}
		add := func(b, where string) {
			if b != "" && !seen[b] && !present[b] {
				seen[b] = true
				gone = append(gone, displaced{b, where})
			}
		}
		for _, s := range old.Slots {
			add(s.Barcode, fmt.Sprintf("slot %d", s.Number))
		}
		for _, n := range sortedKeys(lp.Slots) {
			add(lp.Slots[n], fmt.Sprintf("slot %d", n))
		}
		for _, m := range old.MAPs {
			add(m.Barcode, fmt.Sprintf("MAP slot %d", m.Number))
		}
		for _, n := range sortedKeys(lp.MAPs) {
			add(lp.MAPs[n], fmt.Sprintf("MAP slot %d", n))
		}
		for _, p := range old.PickerMedia {
			add(p.Barcode, fmt.Sprintf("picker %d", p.Number))
		}

		disp := map[string]Disposition{}
		for _, d := range byLib[lib.ID] {
			if _, dup := disp[d.Barcode]; dup {
				r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: two dispositions for %s", lib.ID, d.Barcode)
				continue
			}
			disp[d.Barcode] = d
			if !seen[d.Barcode] {
				r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: a disposition names %s, but it does not leave the library", lib.ID, d.Barcode)
			}
		}

		freeMAPs := 0
		for n, b := range keptMAPs {
			if b == "" && (!haveLive || lp.MAPs[n] == "") {
				freeMAPs++
			}
		}
		exports := 0
		targets := map[int]string{}
		for _, g := range gone {
			d, ok := disp[g.barcode]
			if !ok {
				// The apply drops it, even when cur already lists it
				// nowhere (a cartridge only the live state has).
				r.stickyErrorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %s (%s) would leave the library; say where it goes: a storage slot, export, or shelf", lib.ID, g.barcode, g.where)
				continue
			}
			switch d.To {
			case ToShelf:
			case ToExport:
				exports++
			case ToSlot:
				b, kept := keptSlots[d.Slot]
				switch {
				case !kept:
					r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %s is to go to slot %d, which the new configuration does not have", lib.ID, g.barcode, d.Slot)
				case b != "" && b != g.barcode:
					r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %s is to go to slot %d, which lists %s", lib.ID, g.barcode, d.Slot, b)
				case haveLive && lp.Slots[d.Slot] != "" && lp.Slots[d.Slot] != g.barcode:
					r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %s is to go to slot %d, which holds %s now", lib.ID, g.barcode, d.Slot, lp.Slots[d.Slot])
				case targets[d.Slot] != "":
					r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %s and %s are both to go to slot %d", lib.ID, targets[d.Slot], g.barcode, d.Slot)
				default:
					targets[d.Slot] = g.barcode
				}
			default:
				r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: unknown destination %q for %s; want slot, export or shelf", lib.ID, d.To, g.barcode)
			}
		}
		if exports > freeMAPs {
			r.errorf(FamilyRemoval, lib.ID, []string{libKey(lib.ID)}, "library %d: %d cartridges are to be exported, but only %d MAP slots are free", lib.ID, exports, freeMAPs)
		}
	}
}
