package validate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/media"
)

// place is where a barcode is listed.
type place struct {
	lib  int
	what string // "slot 3", "MAP slot 1"
	key  ElementRef
}

func (p place) String() string { return fmt.Sprintf("library %d %s", p.lib, p.what) }

// listed returns every barcode lib's library_contents lists, with where.
func listed(lib config.Library) map[string][]place {
	m := map[string][]place{}
	for _, s := range lib.Slots {
		if s.Barcode != "" {
			m[s.Barcode] = append(m[s.Barcode], place{lib.ID, "slot " + strconv.Itoa(s.Number), slotKey(lib.ID, s.Number)})
		}
	}
	for _, s := range lib.MAPs {
		if s.Barcode != "" {
			m[s.Barcode] = append(m[s.Barcode], place{lib.ID, "MAP slot " + strconv.Itoa(s.Number), mapKey(lib.ID, s.Number)})
		}
	}
	for _, s := range lib.PickerMedia {
		m[s.Barcode] = append(m[s.Barcode], place{lib.ID, "picker " + strconv.Itoa(s.Number), pickerKey(lib.ID, s.Number)})
	}
	return m
}

// holds returns the barcodes a live placement holds.
func (p Placement) holds() map[string]bool {
	m := map[string]bool{}
	for _, els := range []map[int]string{p.Slots, p.MAPs, p.Drives} {
		for _, b := range els {
			if b != "" {
				m[b] = true
			}
		}
	}
	return m
}

// MaxBarcodeLen is the longest barcode vtllibrary accepts; add_barcode exits
// on a longer one (mhvtl include/vtllib.h MAX_BARCODE_LEN :90,
// usr/cmd/vtllibrary.c add_barcode :324-328, upstream master 993b193).
const MaxBarcodeLen = 16

// barcodeChars reports whether b is only ASCII letters and digits, the
// barcodes mhvtl-control creates (media.ValidBarcode) and upstream's
// generate_library_contents writes ([A-Z0-9], etc/generate_library_contents.in
// :94-119, upstream master 993b193). vtllibrary takes the first word of a
// Slot, MAP or Picker line as the barcode ("Slot %d: %s",
// usr/cmd/vtllibrary.c:1045-1055), and vtltape opens the cartridge at
// <home>/<barcode> (usr/vtlcart.c:1306); make_vtl_media passes awk's third
// field to "mktape -m" and to "rm -rf $LIBHOME/$a" unchecked
// (usr/cmd/make_vtl_media.in:364-377). So a
// '/', a "..", or a shell metacharacter would reach outside the home as
// root. The media type rules (personality.BarcodeDensity) stay unanchored, as
// make_vtl_media's are; this is checked apart from them.
func barcodeChars(b string) bool {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// checkBarcodes refuses a barcode longer than vtllibrary accepts, one with
// characters other than letters and digits, and one
// that two places would claim: listed twice
// in next, listed by one library while another library's .persist file or
// live state has it, or a new pre-fill naming a cartridge that already
// exists in another library's home. mhVTL itself does not notice: two
// libraries sharing a home would both drive the same cartridge.
func checkBarcodes(r *Result, cur, next config.Inventory, live *Live, opts Options) {
	all := map[string][]place{}
	for _, lib := range next.Libraries {
		for b, ps := range listed(lib) {
			all[b] = append(all[b], ps...)
		}
	}
	for _, b := range sortedStrings(all) {
		for _, p := range all[b] {
			if len(b) > MaxBarcodeLen {
				r.errorf(CodeBarcodeLength, p.lib, []ElementRef{p.key}, "%s: barcode %s is %d characters; vtllibrary stops on a barcode longer than %d",
					p, b, len(b), MaxBarcodeLen)
			}
			if !barcodeChars(b) {
				r.errorf(CodeBarcodeChars, p.lib, []ElementRef{p.key}, "%s: barcode %+q may hold only ASCII letters and digits; make_vtl_media passes it to mktape, run as root, as a directory name under the media home",
					p, b)
			}
		}
	}
	for _, b := range sortedStrings(all) {
		if ps := all[b]; len(ps) > 1 {
			var els []ElementRef
			for _, p := range ps {
				els = append(els, p.key)
			}
			r.errorf(CodeBarcodeDup, ps[1].lib, els, "barcode %s is listed %d times: %s", b, len(ps), joinPlaces(ps))
		}
	}

	persisted := map[int]map[string]bool{}
	if opts.ConfigDir != "" {
		for _, lib := range next.Libraries {
			p := filepath.Join(opts.ConfigDir, "library_contents."+strconv.Itoa(lib.ID)+".persist")
			f, err := os.Open(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				r.errorf(CodeBarcodePersistRead, lib.ID, []ElementRef{libKey(lib.ID)}, "cannot read %s: %v", p, err)
				continue
			}
			// Only the barcodes count here; lines conffile flags are skipped.
			_, pickers, maps, slots, _, err := config.ParseLibraryContents(f)
			f.Close()
			if err != nil {
				r.errorf(CodeBarcodePersistRead, lib.ID, []ElementRef{libKey(lib.ID)}, "%s: %v", p, err)
				continue
			}
			var held []config.Slot
			for _, p := range pickers {
				if p.Barcode != "" {
					held = append(held, p)
				}
			}
			persisted[lib.ID] = barcodes(listed(config.Library{ID: lib.ID, Slots: slots, MAPs: maps, PickerMedia: held}))
		}
	}

	homes := map[string][]int{} // home → libraries using it, in cur or next
	for _, inv := range []config.Inventory{cur, next} {
		for _, lib := range inv.Libraries {
			if h := homeOf(lib, opts); h != "" && !containsInt(homes[h], lib.ID) {
				homes[h] = append(homes[h], lib.ID)
			}
		}
	}

	for _, lib := range next.Libraries {
		mine := listed(lib)
		before := map[string]bool{}
		if c, ok := findLibrary(cur, lib.ID); ok {
			before = barcodes(listed(c))
		}
		if p, ok := live.library(lib.ID); ok {
			for b := range p.holds() {
				before[b] = true
			}
		}
		home := homeOf(lib, opts)
		for _, b := range sortedStrings(mine) {
			for _, other := range sortedKeys(persisted) {
				if other != lib.ID && persisted[other][b] {
					r.errorf(CodeBarcodeInPersist, lib.ID, []ElementRef{mine[b][0].key}, "barcode %s (%s) is in library %d's library_contents.%d.persist", b, mine[b][0], other, other)
				}
			}
			if live != nil {
				for _, other := range sortedKeys(live.Libraries) {
					if other != lib.ID && live.Libraries[other].holds()[b] {
						r.stickyErrorf(CodeBarcodeHeldLive, lib.ID, []ElementRef{mine[b][0].key}, "barcode %s (%s) is held by library %d now", b, mine[b][0], other)
					}
				}
			}
			if before[b] || home == "" {
				continue
			}
			for _, h := range sortedStrings(homes) {
				if h != home && media.Exists(h, b) {
					r.errorf(CodeBarcodeInOtherHome, lib.ID, []ElementRef{mine[b][0].key}, "barcode %s (%s) is new to library %d, but a cartridge %s already exists in %s, the home of library %s",
						b, mine[b][0], lib.ID, b, h, joinInts(homes[h]))
				}
			}
		}
	}
}

// barcodes flattens listed() to a set.
func barcodes(m map[string][]place) map[string]bool {
	s := map[string]bool{}
	for b := range m {
		s[b] = true
	}
	return s
}

func joinPlaces(ps []place) string {
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p.String()
	}
	return s
}

func joinInts(v []int) string {
	s := ""
	for i, n := range v {
		if i > 0 {
			s += ", "
		}
		s += strconv.Itoa(n)
	}
	return s
}

func containsInt(v []int, n int) bool {
	for _, x := range v {
		if x == n {
			return true
		}
	}
	return false
}

func sortedStrings[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
