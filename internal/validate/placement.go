package validate

import (
	"os"
	"path/filepath"
	"strconv"

	"mhvtl-control/internal/config"
)

// Merge returns where library next holds its cartridges once an apply has
// written next's library_contents merged with the live placement, as the
// plan's placement preservation does (P1: the live storage and MAP
// placements are merged into the written library_contents, never Drive
// lines). cur is the library in force, whose library_contents tells which
// elements next edits.
//
// For each storage slot (including the slots the Slot lines skip, which
// vtllibrary makes empty) and MAP slot of next:
//   - when next edits the element to a barcode (one cur's file does not have
//     there), that barcode is placed;
//   - otherwise, with the live placement known, the live cartridge stays
//     there, and an element live shows empty is empty;
//   - otherwise next's file decides.
//
// A live cartridge that an edit places in another element has moved there
// and is not kept at its live element. Pickers come from next's file. A
// barcode bay reports true for (a cartridge the bay monitor placed) is left
// out: the bay monitor places it again itself. Merge(cur, cur, live, bay)
// is the placement in force. Live drives are not part of the result.
func Merge(cur, next config.Library, live *Placement, bay func(barcode string) bool) Placement {
	out := Placement{Slots: map[int]string{}, MAPs: map[int]string{}, Drives: map[int]string{}, Pickers: map[int]string{}}
	curS, nextS := fileSlots(cur), fileSlots(next)
	curM, nextM := fileMAPs(cur), fileMAPs(next)
	edited := func(c, n map[int]string, k int) bool { return n[k] != "" && n[k] != c[k] }

	placed := map[string]bool{} // barcodes next edits into an element
	for k := range nextS {
		if edited(curS, nextS, k) {
			placed[nextS[k]] = true
		}
	}
	for k := range nextM {
		if edited(curM, nextM, k) {
			placed[nextM[k]] = true
		}
	}

	fill := func(dst, c, n, l map[int]string) {
		for k, b := range n {
			if !edited(c, n, k) && live != nil {
				if b = l[k]; placed[b] {
					b = "" // moved by an edit
				}
			}
			if b != "" && (bay == nil || !bay(b)) {
				dst[k] = b
			}
		}
	}
	var ls, lm map[int]string
	if live != nil {
		ls, lm = live.Slots, live.MAPs
	}
	fill(out.Slots, curS, nextS, ls)
	fill(out.MAPs, curM, nextM, lm)
	for _, p := range next.PickerMedia {
		if bay == nil || !bay(p.Barcode) {
			out.Pickers[p.Number] = p.Barcode
		}
	}
	return out
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
