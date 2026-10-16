package validate

import (
	"fmt"
	"sort"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/personality"
)

// Preset is what a backup application needs from a library.
type Preset struct {
	Name string // for messages
	// Check returns why the library does not suit the application, or "".
	Check func(lib config.Library, p personality.Personality) string
}

// PresetPBS is the TargetApps value for Proxmox Backup Server.
const PresetPBS = "pbs"

// Presets holds the supported target applications by TargetApps value.
var Presets = map[string]Preset{
	PresetPBS: {
		Name: "Proxmox Backup Server",
		// PBS lists changers by udev's ID_SCSI_SERIAL, which udev takes from
		// INQUIRY VPD page 83h. A changer without that page is not found
		// (seen on a test VM with the shipped STK L700, which faithfully
		// omits it).
		Check: func(_ config.Library, p personality.Personality) string {
			if p.VPD83 {
				return ""
			}
			return fmt.Sprintf("it finds changers by udev's ID_SCSI_SERIAL, which comes from VPD page 83h, and the %s personality (%s, selected at %s) has no such page; use a model that has it, such as IBM 03584 or Quantum Scalar",
				p.Name, p.InitFn, p.Rule.Source)
		},
	},
}

// checkPresets enforces the target application of each library. A library
// without one gets every preset's findings as warnings.
func checkPresets(r *Result, next config.Inventory, opts Options) {
	names := make([]string, 0, len(Presets))
	for k := range Presets {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, lib := range next.Libraries {
		p := personality.Lookup(lib.Vendor, lib.Product)
		app := opts.TargetApps[lib.ID]
		if app == "" {
			for _, k := range names {
				if why := Presets[k].Check(lib, p); why != "" {
					r.warnf(CodePresetUnmet, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d would not work with %s: %s", lib.ID, Presets[k].Name, why)
				}
			}
			continue
		}
		pr, ok := Presets[app]
		if !ok {
			r.stickyErrorf(CodePresetUnknownApp, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d: unknown target application %q", lib.ID, app)
			continue
		}
		if why := pr.Check(lib, p); why != "" {
			r.stickyErrorf(CodePresetUnmet, lib.ID, []ElementRef{libKey(lib.ID)}, "library %d is set up for %s, but %s", lib.ID, pr.Name, why)
		}
	}
}
