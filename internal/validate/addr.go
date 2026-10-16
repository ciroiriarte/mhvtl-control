package validate

import (
	"fmt"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/personality"
)

// window is one element type of a library: its first element address and
// the element numbers configured for it.
type window struct {
	kind    string // for messages: "drive", "storage slot", ...
	plural  string
	start   uint16
	numbers []int
	keys    []ElementRef // element of each number
}

// checkAddresses refuses drives, slots, MAP slots and pickers whose element
// address runs into the next element type of the library's personality, or
// past the 16-bit address space. Element N of a type sits at start+N-1, so
// with the next type at next the largest N is next-start. vtllibrary
// silently drops an element that overflows (mhvtl usr/cmd/vtllibrary.c
// check_overflow); here it is refused instead.
func checkAddresses(r *Result, next config.Inventory) {
	for _, lib := range next.Libraries {
		p := personality.Lookup(lib.Vendor, lib.Product)
		// vtllibrary makes a drive element for every "Drive N:" line of
		// library_contents, and an empty one for each number a line skips
		// over, one per number (usr/cmd/vtllibrary.c __init_slot_info
		// :1031-1039, init_drive_slot :851-878 → lookup_drive, add_new_slot;
		// check_overflow at :856). update_drive_details then places each
		// device.conf drive on the element of its Slot (:689). A drive whose
		// Slot no Drive line reaches gets a new element whose address is
		// never set, so it reports element address 0, and check_overflow is
		// never asked (:689-704, after customise_lu at :1711-1713). Line
		// numbers are upstream master 993b193. The writer that saves the
		// configuration adds the Drive lines; a file edited by hand may lack
		// them.
		drives := window{kind: "drive", plural: "drives", start: p.StartDrive}
		have := map[int]bool{}
		fill := 1
		for _, dl := range lib.DriveLines {
			for n := fill; n <= dl.Number; n++ {
				if !have[n] {
					have[n] = true
					drives.numbers, drives.keys = append(drives.numbers, n), append(drives.keys, lineKey(lib.ID, dl.Number))
				}
			}
			fill = max(fill, dl.Number+1)
		}
		// A Drive line numbered at or below an earlier one reuses that
		// drive element (lookup_drive) but counts it again in num_drives
		// (usr/cmd/vtllibrary.c:859-874, upstream master 993b193), which
		// mode page 1Dh reports (usr/mode.c:1332) and READ ELEMENT STATUS
		// takes as the drive address range (usr/smc.c:80-81).
		prevLine := 0
		for _, dl := range lib.DriveLines {
			switch {
			case dl.Number == prevLine:
				r.errorf(CodeAddrDuplicate, lib.ID, []ElementRef{libKey(lib.ID), lineKey(lib.ID, dl.Number)},
					"library %d: library_contents.%d lists \"Drive %d:\" twice; vtllibrary counts the drive twice in num_drives, which mode page 1Dh and READ ELEMENT STATUS report", lib.ID, lib.ID, dl.Number)
			case dl.Number < prevLine:
				r.errorf(CodeAddrOrder, lib.ID, []ElementRef{libKey(lib.ID), lineKey(lib.ID, prevLine), lineKey(lib.ID, dl.Number)},
					"library %d: \"Drive %d:\" is listed after \"Drive %d:\"; vtllibrary counts the drive twice in num_drives, which mode page 1Dh and READ ELEMENT STATUS report; library_contents must list drives in increasing order", lib.ID, dl.Number, prevLine)
			}
			prevLine = max(prevLine, dl.Number)
		}
		placed := map[int]bool{}
		for _, d := range lib.Drives {
			switch {
			case d.Slot >= 1 && !have[d.Slot]:
				r.errorf(CodeAddrNoDriveLine, lib.ID, []ElementRef{libKey(lib.ID), driveKey(d.ID)},
					"library %d: drive %d is in drive slot %d, but library_contents.%d has no \"Drive %d:\" line, so vtllibrary would report it at element address 0",
					lib.ID, d.ID, d.Slot, lib.ID, d.Slot)
			case placed[d.Slot] || d.Slot < 1:
				// A second device.conf drive in a slot, or an invalid slot.
				drives.numbers, drives.keys = append(drives.numbers, d.Slot), append(drives.keys, driveKey(d.ID))
			default:
				placed[d.Slot] = true
				// The element of the Drive line is this drive's.
				for i, n := range drives.numbers {
					if n == d.Slot {
						drives.keys[i] = driveKey(d.ID)
					}
				}
			}
		}
		slots := window{kind: "storage slot", plural: "storage slots", start: p.StartStorage}
		for _, s := range lib.Slots {
			slots.numbers, slots.keys = append(slots.numbers, s.Number), append(slots.keys, slotKey(lib.ID, s.Number))
		}
		maps := window{kind: "MAP slot", plural: "MAP slots", start: p.StartMAP}
		for _, m := range lib.MAPs {
			maps.numbers, maps.keys = append(maps.numbers, m.Number), append(maps.keys, mapKey(lib.ID, m.Number))
		}
		pickers := window{kind: "picker", plural: "pickers", start: p.StartPicker}
		for _, n := range lib.Pickers {
			pickers.numbers, pickers.keys = append(pickers.numbers, n), append(pickers.keys, pickerKey(lib.ID, n))
		}
		// vtllibrary reads Slot and MAP lines in file order. A Slot line
		// numbered below an earlier one adds a second element for a number
		// that already has one, made when the earlier line filled the gap
		// before it (usr/cmd/vtllibrary.c:1054-1064, upstream master
		// 993b193); MAP elements go into the element list in file order,
		// which vtllibrary expects to be address order (:1074).
		for _, w := range []window{slots, maps} {
			prev, prevKey := 0, ElementRef{}
			for i, n := range w.numbers {
				if n >= 1 && n < prev {
					r.errorf(CodeAddrOrder, lib.ID, []ElementRef{libKey(lib.ID), prevKey, w.keys[i]},
						"library %d: %s %d is listed after %s %d; library_contents must list %s in increasing order", lib.ID, w.kind, n, w.kind, prev, w.plural)
				}
				if n > prev {
					prev, prevKey = n, w.keys[i]
				}
			}
		}
		wins := []window{drives, slots, maps, pickers}
		for _, w := range wins {
			seen := map[int]ElementRef{}
			highest := 0
			for i, n := range w.numbers {
				k := w.keys[i]
				if n < 1 {
					r.errorf(CodeAddrNumber, lib.ID, []ElementRef{libKey(lib.ID), k}, "library %d: %s %d is invalid; numbering starts at 1", lib.ID, w.kind, n)
					continue
				}
				if o, dup := seen[n]; dup {
					r.errorf(CodeAddrDuplicate, lib.ID, []ElementRef{libKey(lib.ID), o, k}, "library %d: %s %d is configured twice", lib.ID, w.kind, n)
				}
				seen[n] = k
				highest = max(highest, n)
			}
			if highest == 0 {
				continue
			}
			limit, upto := 0x10000-int(w.start), ""
			for _, o := range wins {
				if o.start > w.start && int(o.start-w.start) < limit {
					limit, upto = int(o.start-w.start), o.kind
				}
			}
			if highest <= limit {
				continue
			}
			els := []ElementRef{libKey(lib.ID)}
			for i, n := range w.numbers {
				if n > limit {
					els = append(els, w.keys[i])
				}
			}
			if upto == "" {
				upto = "beyond the 16-bit element address space"
			} else {
				upto = fmt.Sprintf("which overlaps the %s addresses from 0x%04x", upto, int(w.start)+limit)
			}
			r.errorf(CodeAddrOverflow, lib.ID, els,
				"library %d (%s): %s %d would sit at element address 0x%04x, %s; the limit is %d %s (%s addresses from 0x%04x, %s)",
				lib.ID, p.Name, w.kind, highest, int(w.start)+highest-1, upto, limit, w.plural, w.kind, w.start, p.WindowSource)
		}
	}
}
