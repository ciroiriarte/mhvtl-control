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
		var drives []int
		for _, d := range lib.Drives {
			drives = append(drives, d.Slot)
		}
		var slots, maps []int
		for _, s := range lib.Slots {
			slots = append(slots, s.Number)
		}
		for _, m := range lib.MAPs {
			maps = append(maps, m.Number)
		}
		wins := []window{
			{"drive", "drives", p.StartDrive, drives},
			{"storage slot", "storage slots", p.StartStorage, slots},
			{"MAP slot", "MAP slots", p.StartMAP, maps},
			{"picker", "pickers", p.StartPicker, lib.Pickers},
		}
		for _, w := range wins {
			seen := map[int]bool{}
			highest := 0
			for _, n := range w.numbers {
				if n < 1 {
					r.errorf(FamilyAddress, lib.ID, "library %d: %s %d is invalid; numbering starts at 1", lib.ID, w.kind, n)
					continue
				}
				if seen[n] {
					r.errorf(FamilyAddress, lib.ID, "library %d: %s %d is configured twice", lib.ID, w.kind, n)
				}
				seen[n] = true
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
			if upto == "" {
				upto = "beyond the 16-bit element address space"
			} else {
				upto = fmt.Sprintf("which overlaps the %s addresses from 0x%04x", upto, int(w.start)+limit)
			}
			r.errorf(FamilyAddress, lib.ID,
				"library %d (%s): %s %d would sit at element address 0x%04x, %s; the limit is %d %s (%s addresses from 0x%04x, %s)",
				lib.ID, p.Name, w.kind, highest, int(w.start)+highest-1, upto, limit, w.plural, w.kind, w.start, p.WindowSource)
		}
	}
}
