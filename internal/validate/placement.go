package validate

import (
	"os"
	"path/filepath"
	"strconv"

	"mhvtl-control/internal/config"
)

// Baseline returns the placement in force for library cur: with the live
// placement known, the live cartridge of each storage slot (including the
// slots the Slot lines skip, which vtllibrary makes empty) and MAP slot of
// cur; otherwise cur's library_contents. Pickers come from cur's file. A MAP
// entry bay reports true for (a cartridge the bay monitor placed) is left
// out: the bay monitor places it again itself (plan 3.3, exec step 5). A bay
// cartridge a host moved into a storage slot or drive stays: the bay monitor
// only re-places MAP entries.
func Baseline(cur config.Library, live *Placement, bay func(barcode string) bool) Placement {
	out := newPlacement()
	slots, maps := fileSlots(cur), fileMAPs(cur)
	if live != nil {
		for k := range slots {
			slots[k] = live.Slots[k]
		}
		for k := range maps {
			maps[k] = live.MAPs[k]
		}
	}
	keep(out.Slots, slots, nil)
	keep(out.MAPs, maps, bay)
	keep(out.Pickers, pickerMedia(cur), nil)
	return out
}

// Merge returns where library next holds its cartridges once an apply has
// written next's library_contents merged with the live placement, as the
// plan's placement preservation does (P1: the live storage and MAP
// placements are merged into the written library_contents, never Drive
// lines). cur is the library in force.
//
// next is taken as an edit of Baseline(cur, live, bay), the view a client
// shows when the live placement is known. A storage slot (including skipped
// ones) or MAP slot of next is edited when next's file gives it another
// barcode than the baseline, or none where the baseline has one: a cleared
// slot or a new gap. An edited element takes next's value; any other keeps
// the baseline's. A cartridge an edit places elsewhere (or next lists on a
// picker) has moved and is not kept at its old element. A cartridge edits
// displace and place nowhere leaves the library. With the live placement
// unknown, the baseline is cur's file, so the result is next's file.
// Pickers come from next's file. Bay MAP entries are left out as in Baseline.
// Live drives are not part of the result.
func Merge(cur, next config.Library, live *Placement, bay func(barcode string) bool) Placement {
	out := newPlacement()
	base := Baseline(cur, live, nil)
	nextS, nextM := fileSlots(next), fileMAPs(next)

	placed := map[string]bool{} // barcodes next edits into an element
	for _, b := range pickerMedia(next) {
		placed[b] = true
	}
	for _, e := range []struct{ base, next map[int]string }{{base.Slots, nextS}, {base.MAPs, nextM}} {
		for k, b := range e.next {
			if b != e.base[k] && b != "" {
				placed[b] = true
			}
		}
	}

	fill := func(dst, base, next map[int]string, bay func(string) bool) {
		vals := map[int]string{}
		for k, b := range next {
			if b == base[k] {
				// Not edited: the baseline's cartridge stays, unless an
				// edit moved it.
				if placed[b] {
					b = ""
				}
			}
			vals[k] = b
		}
		keep(dst, vals, bay)
	}
	fill(out.Slots, base.Slots, nextS, nil)
	fill(out.MAPs, base.MAPs, nextM, bay)
	keep(out.Pickers, pickerMedia(next), nil)
	return out
}

func newPlacement() Placement {
	return Placement{Slots: map[int]string{}, MAPs: map[int]string{}, Drives: map[int]string{}, Pickers: map[int]string{}}
}

// keep copies the non-empty barcodes of src that bay does not claim to dst.
func keep(dst, src map[int]string, bay func(string) bool) {
	for k, b := range src {
		if b != "" && (bay == nil || !bay(b)) {
			dst[k] = b
		}
	}
}

// pickerMedia maps lib's pickers to the barcode library_contents lists.
func pickerMedia(lib config.Library) map[int]string {
	m := map[int]string{}
	for _, p := range lib.PickerMedia {
		m[p.Number] = p.Barcode
	}
	return m
}

// fileSlots maps every storage slot vtllibrary makes from lib's
// library_contents to the barcode listed there ("" for empty): the Slot lines
// and the slots they skip (usr/cmd/vtllibrary.c:1054-1064, upstream master
// c65bff0).
func fileSlots(lib config.Library) map[int]string {
	m := map[int]string{}
	highest := 0
	for _, s := range lib.Slots {
		m[s.Number] = s.Barcode
		highest = max(highest, s.Number)
	}
	for n := 1; n < highest; n++ {
		if _, ok := m[n]; !ok {
			m[n] = ""
		}
	}
	return m
}

// fileMAPs maps lib's MAP slots to the barcode listed there.
func fileMAPs(lib config.Library) map[int]string {
	m := map[int]string{}
	for _, s := range lib.MAPs {
		m[s.Number] = s.Barcode
	}
	return m
}

// bayCartridge reports whether <home>/<barcode> is a symlink: a raw-disk
// cartridge the bay monitor placed.
func bayCartridge(home string) func(string) bool {
	return func(barcode string) bool {
		if home == "" {
			return false
		}
		fi, err := os.Lstat(filepath.Join(home, barcode))
		return err == nil && fi.Mode()&os.ModeSymlink != 0
	}
}

// cartridges lists the cartridges of p with where each is, storage slots
// first, then MAP slots, pickers and drives, each by number.
func (p Placement) cartridges() []displaced {
	var out []displaced
	seen := map[string]bool{}
	for _, e := range []struct {
		kind string
		els  map[int]string
	}{{"slot", p.Slots}, {"MAP slot", p.MAPs}, {"picker", p.Pickers}, {"drive", p.Drives}} {
		for _, n := range sortedKeys(e.els) {
			if b := e.els[n]; b != "" && !seen[b] {
				seen[b] = true
				out = append(out, displaced{b, e.kind + " " + strconv.Itoa(n)})
			}
		}
	}
	return out
}
