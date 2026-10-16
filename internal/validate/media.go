package validate

import (
	"fmt"
	"strings"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/personality"
)

// checkMedia refuses a pre-filled slot or MAP slot whose cartridge none of
// the library's drives loads. The media type comes from the barcode, as
// make_vtl_media decides it when it creates the cartridge (set_density in
// mhvtl usr/cmd/make_vtl_media.in).
func checkMedia(r *Result, next config.Inventory, opts Options) {
	for _, lib := range next.Libraries {
		var models []personality.DriveModel
		var products []string
		for _, d := range lib.Drives {
			models = append(models, personality.DriveFor(d.Product))
			products = append(products, d.Product)
		}
		// Changing the drives changes what loads.
		drives := []string{libKey(lib.ID)}
		for _, d := range lib.Drives {
			drives = append(drives, driveKey(d.ID))
		}
		home := homeOf(lib, opts)
		check := func(where, key, barcode string) {
			els := append(drives[:len(drives):len(drives)], key)
			if barcode == "" {
				return
			}
			density := personality.BarcodeDensity(barcode)
			name, ok := personality.DensityMedia[density]
			if !ok {
				if home != "" && media.Exists(home, barcode) {
					r.warnf(FamilyMedia, lib.ID, "library %d %s: the media type of %s cannot be told from its barcode; the existing cartridge is used as it is", lib.ID, where, barcode)
					return
				}
				r.errorf(FamilyMedia, lib.ID, els, "library %d %s: make_vtl_media cannot create %s: its barcode gives density %s, which mktape does not accept", lib.ID, where, barcode, density)
				return
			}
			if len(models) == 0 {
				r.warnf(FamilyMedia, lib.ID, "library %d %s: %s is %s media, but the library has no drives", lib.ID, where, barcode, name)
				return
			}
			for _, m := range models {
				if m.Reads(name) {
					return
				}
			}
			r.errorf(FamilyMedia, lib.ID, els, "library %d %s: %s is %s media (from its barcode), which none of the library's drives (%s) loads",
				lib.ID, where, barcode, name, strings.Join(products, ", "))
		}
		for _, s := range lib.Slots {
			check(fmt.Sprintf("slot %d", s.Number), slotKey(lib.ID, s.Number), s.Barcode)
		}
		for _, m := range lib.MAPs {
			check(fmt.Sprintf("MAP slot %d", m.Number), mapKey(lib.ID, m.Number), m.Barcode)
		}
	}
}
