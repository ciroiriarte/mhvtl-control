// Package validate checks a proposed mhVTL configuration before it is
// written: element addresses fit the emulated library (addr.go), identities
// are unique in device.conf and on the host (ident.go), the target backup
// application's needs are met (presets.go), pre-filled media can be read by
// the library's drives (media.go), barcodes are unique (barcodes.go), and no
// cartridge is dropped with a removed slot (removal.go).
//
// Every family blocks: a finding in Result.Errors means the configuration
// must not be applied. Warnings are advisory.
package validate

import (
	"fmt"
	"sort"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
)

// Check families.
const (
	FamilyAddress  = "address"
	FamilyIdentity = "identity"
	FamilyPreset   = "preset"
	FamilyMedia    = "media"
	FamilyBarcode  = "barcode"
	FamilyRemoval  = "removal"
)

// Finding is one problem found in a configuration.
type Finding struct {
	Family string `json:"family"`
	// Library is the ID of the library concerned, or 0.
	Library int    `json:"library,omitempty"`
	Message string `json:"message"`
}

// Result holds the findings of Validate.
type Result struct {
	Errors   []Finding `json:"errors"`
	Warnings []Finding `json:"warnings"`
}

// OK reports whether nothing blocks the configuration.
func (r Result) OK() bool { return len(r.Errors) == 0 }

func (r *Result) errorf(family string, lib int, format string, a ...any) {
	r.Errors = append(r.Errors, Finding{Family: family, Library: lib, Message: fmt.Sprintf(format, a...)})
}

func (r *Result) warnf(family string, lib int, format string, a ...any) {
	r.Warnings = append(r.Warnings, Finding{Family: family, Library: lib, Message: fmt.Sprintf(format, a...)})
}

// Live is where the running libraries hold cartridges now, from READ ELEMENT
// STATUS. A nil *Live means the live state is unknown.
type Live struct {
	Libraries map[int]Placement
}

// Placement maps element numbers (1-based, as in library_contents) to the
// barcode of the cartridge they hold. Empty elements are absent.
type Placement struct {
	Slots  map[int]string
	MAPs   map[int]string
	Drives map[int]string
}

// library returns the placement of library id; ok is false when it is not
// known.
func (l *Live) library(id int) (Placement, bool) {
	if l == nil {
		return Placement{}, false
	}
	p, ok := l.Libraries[id]
	return p, ok
}

// Disposition says what happens to a cartridge whose slot or MAP slot is
// removed.
type Disposition struct {
	Library int    `json:"library"`
	Barcode string `json:"barcode"`
	// To is "slot" (move to storage slot Slot), "export" (to a MAP slot) or
	// "shelf" (leave the library; the cartridge stays in its home).
	To   string `json:"to"`
	Slot int    `json:"slot,omitempty"`
}

// Destinations of a Disposition.
const (
	ToSlot   = "slot"
	ToExport = "export"
	ToShelf  = "shelf"
)

// Options carries the context of a validation beyond the two configurations.
type Options struct {
	// TargetApps maps a library ID to the backup application it is set up
	// for (a Presets key). A library without one gets the preset checks of
	// every application as warnings.
	TargetApps map[int]string
	// Dispositions say where the cartridges of removed slots go.
	Dispositions []Disposition
	// ConfigDir holds library_contents.<id>.persist; empty skips them.
	ConfigDir string
	// DefaultHome is the media home of libraries without a "Home directory"
	// (MHVTL_HOME_PATH); empty skips the home checks for them.
	DefaultHome string
}

// Validate checks next, the proposed configuration, against cur, the one in
// force, the live placements and the host's SCSI devices.
func Validate(cur, next config.Inventory, live *Live, host []hostids.Device, opts Options) Result {
	r := Result{Errors: []Finding{}, Warnings: []Finding{}}
	checkAddresses(&r, next)
	checkIdentities(&r, cur, next, host)
	checkPresets(&r, next, opts)
	checkMedia(&r, next, opts)
	checkBarcodes(&r, cur, next, live, opts)
	checkRemovals(&r, cur, next, live, opts)
	return r
}

// homeOf returns the media home of lib, resolved as mhVTL does (mhvtl
// usr/vtllib.c find_media_home_directory), or "" when unknown.
func homeOf(lib config.Library, opts Options) string {
	if lib.HomeDir != "" {
		return lib.HomeDir
	}
	return opts.DefaultHome
}

func findLibrary(inv config.Inventory, id int) (config.Library, bool) {
	for _, l := range inv.Libraries {
		if l.ID == id {
			return l, true
		}
	}
	return config.Library{}, false
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
