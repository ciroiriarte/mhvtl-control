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
	keys    []string // element key of each number
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
		drives := window{kind: "drive", plural: "drives", start: p.StartDrive}
		for _, d := range lib.Drives {
			drives.numbers, drives.keys = append(drives.numbers, d.Slot), append(drives.keys, driveKey(d.ID))
		}
		// vtllibrary also makes a drive element for every "Drive N:" line of
		// library_contents, and an empty one for each number a line skips
		// over, whether device.conf has a drive there or not; elements share
		// one per number (usr/cmd/vtllibrary.c __init_slot_info :1031-1039,
		// init_drive_slot :851-878 → lookup_drive, add_new_slot; check_overflow
		// at :856; upstream master 6458223).
		have := map[int]bool{}
		for _, n := range drives.numbers {
			have[n] = true
		}
		next := 1
		for _, dl := range lib.DriveLines {
			for n := next; n <= dl.Number; n++ {
				if !have[n] {
					have[n] = true
					drives.numbers, drives.keys = append(drives.numbers, n), append(drives.keys, lineKey(lib.ID, dl.Number))
				}
			}
			next = max(next, dl.Number+1)
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
		wins := []window{drives, slots, maps, pickers}
		for _, w := range wins {
			seen := map[int]string{}
			highest := 0
			for i, n := range w.numbers {
				k := w.keys[i]
				if n < 1 {
					r.errorf(FamilyAddress, lib.ID, []string{libKey(lib.ID), k}, "library %d: %s %d is invalid; numbering starts at 1", lib.ID, w.kind, n)
					continue
				}
				if o, dup := seen[n]; dup {
					r.errorf(FamilyAddress, lib.ID, []string{libKey(lib.ID), o, k}, "library %d: %s %d is configured twice", lib.ID, w.kind, n)
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
			els := []string{libKey(lib.ID)}
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
			r.errorf(FamilyAddress, lib.ID, els,
				"library %d (%s): %s %d would sit at element address 0x%04x, %s; the limit is %d %s (%s addresses from 0x%04x, %s)",
				lib.ID, p.Name, w.kind, highest, int(w.start)+highest-1, upto, limit, w.plural, w.kind, w.start, p.WindowSource)
		}
	}
}
