package validate

import (
	"fmt"
	"strings"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/personality"
)

// checkMedia refuses a pre-filled slot, MAP slot or picker whose cartridge
// none of the library's drives loads. The media type comes from the barcode,
// as make_vtl_media decides it when it creates the cartridge (set_density in
// mhvtl usr/cmd/make_vtl_media.in). make_vtl_media creates cartridges only
// for the Slot lines of library_contents, in the library's home
// (make_vtl_media.in:349-364, upstream master 993b193; see homeOf), so a MAP
// or Picker barcode must name a cartridge that already exists there. A MAP
// entry the live placement already holds in that MAP slot is no pre-fill:
// the cartridge is in the library (imported at run time, or placed by the
// bay monitor), and the client that builds next from the live view (plan,
// "Phase 4 client contract for next") writes it back as it is. That holds
// only while the library keeps its media home: in another home, the
// cartridge must exist there like any other MAP entry.
func checkMedia(r *Result, cur, next config.Inventory, live *Live, opts Options) {
	for _, lib := range next.Libraries {
		var models []personality.DriveModel
		var products []string
		for _, d := range lib.Drives {
			models = append(models, personality.DriveFor(d.Product))
			products = append(products, d.Product)
		}
		// Changing the drives changes what loads.
		drives := []ElementRef{libKey(lib.ID)}
		for _, d := range lib.Drives {
			drives = append(drives, driveKey(d.ID))
		}
		home := homeOf(lib, opts)
		if hiddenHome(home) {
			r.warnf(CodeMediaHomeHidden, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: media home %s is under /home, /root or /run/user, which mhvtl-control's unit hides (ProtectHome=yes); cartridges there look missing to these checks", lib.ID, home)
		}
		// The live MAP counts only for a library that keeps its home.
		var lp Placement
		if old, ok := findLibrary(cur, lib.ID); ok && homeOf(old, opts) == home {
			lp, _ = live.library(lib.ID)
		}
		check := func(where string, key ElementRef, barcode string, created, present bool) {
			els := append(drives[:len(drives):len(drives)], key)
			if barcode == "" {
				return
			}
			if present {
				// In the library now: neither created nor looked for.
			} else if !created && home == "" {
				r.warnf(CodeMediaNoHome, lib.ID, els, "library %d %s: %s: MAP and Picker entries are not created by make_vtl_media, and no media home is known to check that it exists", lib.ID, where, barcode)
			} else if !created && !media.Exists(home, barcode) {
				r.errorf(CodeMediaNotInHome, lib.ID, els, "library %d %s: %s: MAP and Picker entries are not created by make_vtl_media, and it is not in %s", lib.ID, where, barcode, home)
				return
			}
			density := personality.BarcodeDensity(barcode)
			name, ok := personality.DensityMedia[density]
			if !ok {
				if present || home != "" && media.Exists(home, barcode) {
					r.warnf(CodeMediaUnknownType, lib.ID, els, "library %d %s: the media type of %s cannot be told from its barcode; the existing cartridge is used as it is", lib.ID, where, barcode)
					return
				}
				r.errorf(CodeMediaDensity, lib.ID, els, "library %d %s: make_vtl_media cannot create %s: its barcode gives density %s, which mktape does not accept", lib.ID, where, barcode, density)
				return
			}
			if len(models) == 0 {
				r.warnf(CodeMediaNoDrives, lib.ID, els, "library %d %s: %s is %s media, but the library has no drives", lib.ID, where, barcode, name)
				return
			}
			// make_vtl_media makes it a WORM cartridge by its barcode
			// (set_media_type), and vtltape loads one only in a drive whose
			// personality supports WORM (personality.DriveModel.WORM).
			worm := personality.BarcodeMediaType(barcode) == "WORM"
			reads := false
			for _, m := range models {
				if m.Reads(name) && (!worm || m.WORM) {
					return
				}
				reads = reads || m.Reads(name)
			}
			if reads {
				r.errorf(CodeMediaWORM, lib.ID, els, "library %d %s: %s is %s WORM media (from its barcode), and none of the library's drives (%s) that load %s supports WORM",
					lib.ID, where, barcode, name, strings.Join(products, ", "), name)
				return
			}
			r.errorf(CodeMediaUnreadable, lib.ID, els, "library %d %s: %s is %s media (from its barcode), which none of the library's drives (%s) loads",
				lib.ID, where, barcode, name, strings.Join(products, ", "))
		}
		for _, s := range lib.Slots {
			check(fmt.Sprintf("slot %d", s.Number), slotKey(lib.ID, s.Number), s.Barcode, true, false)
		}
		for _, m := range lib.MAPs {
			check(fmt.Sprintf("MAP slot %d", m.Number), mapKey(lib.ID, m.Number), m.Barcode, false, lp.MAPs[m.Number] == m.Barcode)
		}
		for _, p := range lib.PickerMedia {
			check(fmt.Sprintf("picker %d", p.Number), pickerKey(lib.ID, p.Number), p.Barcode, false, false)
		}
	}
}

// hiddenHome reports whether home lies where ProtectHome=yes in
// dist/mhvtl-control.service hides it from mhvtl-control.
func hiddenHome(home string) bool {
	for _, p := range []string{"/home", "/root", "/run/user"} {
		if home == p || strings.HasPrefix(home, p+"/") {
			return true
		}
	}
	return false
}
