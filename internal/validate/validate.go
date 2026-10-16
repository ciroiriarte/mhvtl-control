// Package validate checks a proposed mhVTL configuration before it is
// written: element addresses fit the emulated library (addr.go), identities
// are unique in device.conf and on the host (ident.go), the target backup
// application's needs are met (presets.go), pre-filled media can be read by
// the library's drives (media.go), barcodes are unique (barcodes.go), and no
// cartridge is dropped with a removed slot (removal.go).
//
// Every family blocks: a finding in Result.Errors means the configuration
// must not be applied. Warnings are advisory.
//
// Validate judges the change, not the whole file: an error the configuration
// in force already has, word for word, and which the change does not touch
// (none of its libraries, drives or slots differ) is reported as a warning
// tagged TagPreExisting, so a hand edit made earlier does not block every
// later apply. A collision with another device of the host or a cartridge
// another library holds now always blocks, being a live conflict, and so does
// a library that fails the target application asked for.
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

// TagPreExisting marks a warning that is an error the configuration in force
// already has, untouched by the change.
const TagPreExisting = "pre-existing"

// Finding is one problem found in a configuration.
type Finding struct {
	Family string `json:"family"`
	// Library is the ID of the library concerned, or 0.
	Library int    `json:"library,omitempty"`
	Message string `json:"message"`
	Tag     string `json:"tag,omitempty"`

	elements []string // keys of the libraries, drives and slots concerned
	sticky   bool     // never reported as pre-existing
}

// Result holds the findings of Validate.
type Result struct {
	Errors   []Finding `json:"errors"`
	Warnings []Finding `json:"warnings"`
}

// OK reports whether nothing blocks the configuration.
func (r Result) OK() bool { return len(r.Errors) == 0 }

func (r *Result) errorf(family string, lib int, els []string, format string, a ...any) {
	r.Errors = append(r.Errors, Finding{Family: family, Library: lib, Message: fmt.Sprintf(format, a...), elements: els})
}

// stickyErrorf records an error that blocks even when the configuration in
// force already has it: a conflict with the live host, or a library that does
// not meet the target application asked for.
func (r *Result) stickyErrorf(family string, lib int, els []string, format string, a ...any) {
	r.Errors = append(r.Errors, Finding{Family: family, Library: lib, Message: fmt.Sprintf(format, a...), elements: els, sticky: true})
}

func (r *Result) warnf(family string, lib int, format string, a ...any) {
	r.Warnings = append(r.Warnings, Finding{Family: family, Library: lib, Message: fmt.Sprintf(format, a...)})
}

// Element keys name what a finding concerns.
func libKey(id int) string        { return fmt.Sprintf("library %d", id) }
func driveKey(id int) string      { return fmt.Sprintf("drive %d", id) }
func slotKey(lib, n int) string   { return fmt.Sprintf("library %d slot %d", lib, n) }
func mapKey(lib, n int) string    { return fmt.Sprintf("library %d MAP slot %d", lib, n) }
func pickerKey(lib, n int) string { return fmt.Sprintf("library %d picker %d", lib, n) }
func lineKey(lib, n int) string   { return fmt.Sprintf("library %d Drive line %d", lib, n) }

// prints fingerprints every element of inv by its key, to tell what a change
// touches. An element listed twice under one key gets both values, so adding
// or removing a copy touches it.
func prints(inv config.Inventory) map[string]string {
	m := map[string]string{}
	for _, l := range inv.Libraries {
		bare := l
		bare.Drives, bare.Slots, bare.MAPs, bare.Pickers, bare.DriveLines = nil, nil, nil, nil, nil
		m[libKey(l.ID)] = fmt.Sprintf("%+v", bare)
		for _, d := range l.Drives {
			m[driveKey(d.ID)] += fmt.Sprintf("%+v", d)
		}
		for _, s := range l.Slots {
			m[slotKey(l.ID, s.Number)] += "=" + s.Barcode
		}
		for _, s := range l.MAPs {
			m[mapKey(l.ID, s.Number)] += "=" + s.Barcode
		}
		for _, n := range l.Pickers {
			m[pickerKey(l.ID, n)] += "="
		}
		for _, d := range l.DriveLines {
			m[lineKey(l.ID, d.Number)] += "=" + d.Serial
		}
	}
	for _, d := range inv.Standalone {
		m[driveKey(d.ID)] += fmt.Sprintf("%+v", d)
	}
	return m
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
	r := check(cur, next, live, host, opts)
	before := check(cur, cur, live, host, Options{TargetApps: opts.TargetApps, ConfigDir: opts.ConfigDir, DefaultHome: opts.DefaultHome})
	type key struct {
		family  string
		lib     int
		message string
	}
	had := map[key]bool{}
	for _, f := range before.Errors {
		had[key{f.Family, f.Library, f.Message}] = true
	}
	pc, pn := prints(cur), prints(next)
	errs := r.Errors[:0]
	for _, f := range r.Errors {
		if f.sticky || !had[key{f.Family, f.Library, f.Message}] || touches(f, pc, pn) {
			errs = append(errs, f)
			continue
		}
		f.Tag = TagPreExisting
		r.Warnings = append(r.Warnings, f)
	}
	r.Errors = errs
	return r
}

// touches reports whether the change alters an element f concerns.
func touches(f Finding, cur, next map[string]string) bool {
	for _, k := range f.elements {
		c, inCur := cur[k]
		n, inNext := next[k]
		if inCur != inNext || c != n {
			return true
		}
	}
	return false
}

// check runs every family on next.
func check(cur, next config.Inventory, live *Live, host []hostids.Device, opts Options) Result {
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
