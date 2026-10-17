package validate

import "mhvtl-control/internal/config"

// displaced is a cartridge of cur that next drops from the library.
type displaced struct {
	barcode, where string
}

// checkRemovals refuses to let a cartridge leave a library without a
// Disposition saying where it goes. The apply writes next's library_contents
// merged with the live placement (Merge), so a cartridge leaves when it is in
// the placement in force, Baseline(cur), and nowhere in Merge(cur, next) or a
// live drive: its element was deleted, cleared or made a gap, or next put
// another barcode there. Otherwise it would silently leave the library at the
// next restart. With the live placement known, next is read as an edit of
// the live view, so a cartridge imported at run time stays unless next
// clears its element; a next built from cur's file instead of the live view
// clears it. A cartridge that moves within the library needs none. A
// cartridge in a live drive counts as present, even when next
// deletes that drive: validation does not refuse it, the apply does, since it
// refuses to touch a loaded drive (plan 3.3, exec step 2, drive_loaded) and,
// when forced, puts the cartridge back in a storage slot (step 5). Deleting a
// whole library is not covered here: its cartridges stay in its home (AC 12).
func checkRemovals(r *Result, cur, next config.Inventory, live *Live, opts Options) {
	byLib := map[int][]Disposition{}
	for _, d := range opts.Dispositions {
		byLib[d.Library] = append(byLib[d.Library], d)
	}
	for _, id := range sortedKeys(byLib) {
		_, inCur := findLibrary(cur, id)
		_, inNext := findLibrary(next, id)
		if !inCur || !inNext {
			r.errorf(CodeRemovalUnknownLibrary, id, []ElementRef{libKey(id)}, "dispositions name library %d, which is not in both the current and the new configuration", id)
		}
	}

	for _, lib := range next.Libraries {
		old, ok := findLibrary(cur, lib.ID)
		if !ok {
			continue
		}
		lp, haveLive := live.library(lib.ID)
		keptSlots, keptMAPs := fileSlots(lib), fileMAPs(lib)
		// Decide on the placement the apply writes: next's file merged with
		// the live placement (Merge). A cartridge in a live drive counts as
		// present even when next deletes the drive: the apply refuses a loaded
		// drive it affects (plan 3.3, drive_loaded) and places the cartridge
		// in a slot when forced.
		var livep *Placement
		if haveLive {
			livep = &lp
		}
		bay := bayCartridge(homeOf(lib, opts))
		inDrive := map[string]int{}
		for _, n := range sortedKeys(lp.Drives) {
			if b := lp.Drives[n]; b != "" {
				inDrive[b] = n
			}
		}
		merged := Merge(old, lib, livep, bay)
		present := map[string]bool{}
		for _, c := range merged.cartridges() {
			present[c.barcode] = true
			// The merge would list it in a second element.
			if n, loaded := inDrive[c.barcode]; loaded {
				r.stickyErrorf(CodeBarcodeLoaded, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %s (%s) is loaded in drive %d now", lib.ID, c.barcode, c.where, n)
			}
		}
		for b := range inDrive {
			present[b] = true
		}
		var gone []displaced
		seen := map[string]bool{}
		for _, c := range Baseline(old, livep, bay).cartridges() {
			if !present[c.barcode] {
				seen[c.barcode] = true
				gone = append(gone, c)
			}
		}

		if haveLive {
			warnMoveBack(r, lib.ID, old, lib, lp, merged)
		}

		disp := map[string]Disposition{}
		for _, d := range byLib[lib.ID] {
			if _, dup := disp[d.Barcode]; dup {
				r.errorf(CodeRemovalDispositionDup, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: two dispositions for %s", lib.ID, d.Barcode)
				continue
			}
			disp[d.Barcode] = d
			if !seen[d.Barcode] {
				r.errorf(CodeRemovalNotLeaving, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: a disposition names %s, but it does not leave the library", lib.ID, d.Barcode)
			}
		}

		// Free MAP slots and slot targets are judged on the merged
		// placement the apply writes. A MAP slot holding a bay cartridge
		// (live, or next's file when live is unknown) is not free: Merge
		// leaves it out, but the bay monitor's coldplug places it in the
		// MAP again, and an export there would make that fail
		// (usr/cmd/mhvtl-bay-monitor.c:116-131 in upstream 852b78a).
		freeMAPs := 0
		for n, b := range keptMAPs {
			if haveLive {
				b = lp.MAPs[n]
			}
			if merged.MAPs[n] == "" && (b == "" || !bay(b)) {
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
				r.stickyErrorf(CodeRemovalLeaves, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %s (%s) would leave the library; say where it goes: a storage slot, export, or shelf", lib.ID, g.barcode, g.where)
				continue
			}
			switch d.To {
			case ToShelf:
			case ToExport:
				exports++
			case ToSlot:
				_, kept := keptSlots[d.Slot]
				b := merged.Slots[d.Slot]
				switch {
				case !kept:
					r.errorf(CodeRemovalSlotMissing, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %s is to go to slot %d, which the new configuration does not have", lib.ID, g.barcode, d.Slot)
				case b != "" && b != g.barcode:
					r.errorf(CodeRemovalSlotTaken, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %s is to go to slot %d, which will hold %s", lib.ID, g.barcode, d.Slot, b)
				case targets[d.Slot] != "":
					r.errorf(CodeRemovalSlotShared, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %s and %s are both to go to slot %d", lib.ID, targets[d.Slot], g.barcode, d.Slot)
				default:
					targets[d.Slot] = g.barcode
				}
			default:
				r.errorf(CodeRemovalDestination, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: unknown destination %q for %s; want slot, export or shelf", lib.ID, d.To, g.barcode)
			}
		}
		if exports > freeMAPs {
			r.errorf(CodeRemovalMAPFull, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: %d cartridges are to be exported, but only %d MAP slots are free", lib.ID, exports, freeMAPs)
		}
	}
}

// warnMoveBack warns when next keeps an element as cur's library_contents
// lists it while the live view has that cartridge in another slot or MAP
// slot: the merge reads it as an edit and moves the cartridge back. That is
// what a next built from cur's file rather than the live view does to a
// run-time move, so it may not be meant.
func warnMoveBack(r *Result, id int, cur, next config.Library, live, merged Placement) {
	where := map[string]string{}
	for _, c := range (Placement{Slots: live.Slots, MAPs: live.MAPs}).cartridges() {
		where[c.barcode] = c.where
	}
	for _, e := range []struct {
		kind              string
		ref               func(lib, n int) ElementRef
		cur, next, l, mrg map[int]string
	}{
		{"slot", slotKey, fileSlots(cur), fileSlots(next), live.Slots, merged.Slots},
		{"MAP slot", mapKey, fileMAPs(cur), fileMAPs(next), live.MAPs, merged.MAPs},
	} {
		for _, k := range sortedKeys(e.next) {
			b := e.next[k]
			if b == "" || b != e.cur[k] || e.l[k] == b || e.mrg[k] != b || where[b] == "" {
				continue
			}
			r.warnf(CodeRemovalMoveBack, id, []ElementRef{e.ref(id, k)}, "library %d: %s %d lists %s as library_contents does, but %s is in %s now; the apply moves it back", id, e.kind, k, b, b, where[b])
		}
	}
}
