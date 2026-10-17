package apply

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// mergeLibrary is the placement the apply writes to library next's
// library_contents (plan 3.3, exec step 5; P1): next's file merged with
// src, the source guardedStop returned, by validate.Merge; then the
// dispositions of the cartridges leaving; then the cartridges of the
// forced drives. bay reports a bay-monitor cartridge, left out of the MAP.
//
// A cartridge of a forced drive goes to the slot it came from (SValid and
// the source address of READ ELEMENT STATUS) when next has that slot and
// the merge leaves it empty, else to the first empty storage slot, else
// the merge is refused. It is never written on a Drive line: Drive lines
// of library_contents carry drive serials, and save_config unloads every
// drive logically (Phase 0 amendment 2). A cartridge the source already
// places (a trusted .persist lists it in its slot) is left there.
func mergeLibrary(cur, next config.Library, src validate.Placement, disp []validate.Disposition, forced []changer.DriveState, bay func(string) bool) (validate.Placement, error) {
	merged := validate.Merge(cur, next, &src, bay)
	present := placedBarcodes(merged)
	gone := map[string]bool{}
	base := validate.Baseline(cur, &src, bay)
	for _, m := range []map[int]string{base.Slots, base.MAPs, base.Pickers} {
		for _, b := range m {
			if !present[b] {
				gone[b] = true
			}
		}
	}
	slots := slotNumbers(next)
	for _, d := range disp {
		if d.Library != next.ID || !gone[d.Barcode] {
			continue
		}
		delete(gone, d.Barcode)
		switch d.To {
		case validate.ToSlot:
			if !slots[d.Slot] || merged.Slots[d.Slot] != "" {
				return merged, fmt.Errorf("library %d: slot %d for %s is not free", next.ID, d.Slot, d.Barcode)
			}
			merged.Slots[d.Slot] = d.Barcode
		case validate.ToExport:
			n := 0
			for _, m := range next.MAPs {
				if merged.MAPs[m.Number] == "" && (src.MAPs[m.Number] == "" || !bay(src.MAPs[m.Number])) {
					n = m.Number
					break
				}
			}
			if n == 0 {
				return merged, fmt.Errorf("library %d: no free MAP slot to export %s", next.ID, d.Barcode)
			}
			merged.MAPs[n] = d.Barcode
		}
		present[d.Barcode] = true
	}
	for b := range gone {
		if !bay(b) {
			return merged, fmt.Errorf("library %d: %s would leave the library without a disposition", next.ID, b)
		}
	}
	for _, d := range forced {
		if !d.Full || d.Barcode == "" || present[d.Barcode] {
			continue
		}
		n := d.SourceSlot
		if !slots[n] || merged.Slots[n] != "" {
			n = firstEmpty(slots, merged.Slots)
		}
		if n == 0 {
			return merged, fmt.Errorf("library %d: no free storage slot for %s, unloaded from drive slot %d", next.ID, d.Barcode, d.Number)
		}
		merged.Slots[n] = d.Barcode
		present[d.Barcode] = true
	}
	return merged, nil
}

// mapBack is the placement a rollback or recovery writes into the
// original library_contents of orig (plan 3.2, "Mapping back to the
// original layout"). primary are the placements taken against the new
// layout; extra are cartridges of an earlier source the primary misses
// (a cartridge shelved by a disposition, say), placed after it. Each
// cartridge keeps its slot or MAP slot number when orig has it and it is
// free, else goes to the first empty storage slot of orig; one with no
// free slot is returned in unplaced, and stays on the shelf: its media is
// never deleted. Pickers come from orig's file; bay cartridges are left
// out of the MAP, as in the merge.
func mapBack(orig config.Library, primary validate.Placement, extra []validate.Placement, bay func(string) bool) (validate.Placement, []string) {
	out := newPlacement()
	for _, p := range orig.PickerMedia {
		out.Pickers[p.Number] = p.Barcode
	}
	present := placedBarcodes(out)
	slots, maps := slotNumbers(orig), map[int]bool{}
	for _, m := range orig.MAPs {
		maps[m.Number] = true
	}
	var unplaced []string
	put := func(isMAP bool, n int, b string) {
		if b == "" || present[b] || isMAP && bay(b) {
			return
		}
		present[b] = true
		switch {
		case isMAP && maps[n] && out.MAPs[n] == "":
			out.MAPs[n] = b
		case !isMAP && slots[n] && out.Slots[n] == "":
			out.Slots[n] = b
		default:
			if k := firstEmpty(slots, out.Slots); k != 0 {
				out.Slots[k] = b
			} else {
				unplaced = append(unplaced, b)
			}
		}
	}
	for _, p := range append([]validate.Placement{primary}, extra...) {
		for _, n := range sortedInts(p.Slots) {
			put(false, n, p.Slots[n])
		}
		for _, n := range sortedInts(p.MAPs) {
			put(true, n, p.MAPs[n])
		}
	}
	return out, unplaced
}

// renderContents returns data, a library_contents, with its Slot and MAP
// lines set to p: a line already right stays as it is, another keeps its
// text up to the colon, and a slot p fills that has no line (a gap) gets
// one in order. Drive and Picker lines
// and comments are kept as they are.
func renderContents(data []byte, p validate.Placement) ([]byte, error) {
	c := conffile.ParseLibContents(data)
	for _, k := range []struct {
		kind string
		els  map[int]string
	}{{"Slot", p.Slots}, {"MAP", p.MAPs}} {
		have := map[int]string{}
		for _, e := range c.Entries() {
			if e.Kind == k.kind {
				have[e.Number] = e.Barcode
			}
		}
		for n, b := range have {
			if b == k.els[n] {
				continue // the line stays byte for byte
			}
			if err := c.SetBarcode(k.kind, n, k.els[n]); err != nil {
				return nil, err
			}
		}
		for _, n := range sortedInts(k.els) {
			if _, ok := have[n]; !ok {
				if err := c.AddEntry(k.kind, n, k.els[n]); err != nil {
					return nil, err
				}
			}
		}
	}
	return c.Render(), nil
}

// persistDiffers compares a trusted .persist with R1 (plan 3.3 exec step
// 4, Q7): the storage and MAP slots must hold the same cartridges. A
// cartridge R1 shows in a drive is not compared: save_config puts it back
// in its slot logically.
func persistDiffers(persist, r1 validate.Placement) []string {
	inDrive := map[string]bool{}
	for _, b := range r1.Drives {
		inDrive[b] = true
	}
	var diff []string
	for _, k := range []struct {
		kind    string
		pe, r1e map[int]string
	}{{"slot", persist.Slots, r1.Slots}, {"MAP slot", persist.MAPs, r1.MAPs}} {
		nums := map[int]bool{}
		for n := range k.pe {
			nums[n] = true
		}
		for n := range k.r1e {
			nums[n] = true
		}
		for _, n := range sortedInts(nums) {
			a, b := k.pe[n], k.r1e[n]
			if a != b && !inDrive[a] {
				diff = append(diff, fmt.Sprintf("%s %d: .persist %q, READ ELEMENT STATUS %q", k.kind, n, a, b))
			}
		}
	}
	return diff
}

// bayIn reports whether <home>/<barcode> is a symlink: a raw-disk
// cartridge the bay monitor placed (ensure_symlink, mhvtl
// usr/raw_bay_ops.c:96-130 at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/raw_bay_ops.c#L96-L130).
func bayIn(home string) func(string) bool {
	return func(barcode string) bool {
		if home == "" || barcode == "" {
			return false
		}
		fi, err := os.Lstat(filepath.Join(home, barcode))
		return err == nil && fi.Mode()&os.ModeSymlink != 0
	}
}

// slotNumbers are the storage slots vtllibrary makes from lib's
// library_contents: every Slot line and the numbers below the highest it
// skips (usr/cmd/vtllibrary.c:1054-1064 at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/cmd/vtllibrary.c#L1054-L1064).
func slotNumbers(lib config.Library) map[int]bool {
	m := map[int]bool{}
	for _, s := range lib.Slots {
		for n := 1; n <= s.Number; n++ {
			m[n] = true
		}
	}
	return m
}

func firstEmpty(slots map[int]bool, used map[int]string) int {
	for _, n := range sortedInts(slots) {
		if used[n] == "" {
			return n
		}
	}
	return 0
}

func placedBarcodes(p validate.Placement) map[string]bool {
	m := map[string]bool{}
	for _, els := range []map[int]string{p.Slots, p.MAPs, p.Pickers} {
		for _, b := range els {
			if b != "" {
				m[b] = true
			}
		}
	}
	return m
}

func sortedInts[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
