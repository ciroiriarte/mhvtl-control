// Package validate checks a proposed mhVTL configuration before it is
// written: element addresses fit the emulated library (addr.go), identities
// are unique in device.conf and on the host (ident.go), the target backup
// application's needs are met (presets.go), pre-filled media can be read by
// the library's drives (media.go), barcodes are unique (barcodes.go), and no
// cartridge leaves its library unaccounted for (removal.go). A line of the
// proposed files that mhVTL would not read as written (a conffile Problem)
// blocks as well.
//
// Every family blocks: a finding in Result.Errors means the configuration
// must not be applied. Warnings are advisory.
//
// Validate judges the change, not the whole file: an error the configuration
// in force already has, word for word, and which the change does not touch
// (none of its libraries, drives or slots differ) is reported as a warning
// tagged TagPreExisting, so a hand edit made earlier does not block every
// later apply. A collision with another device of the host or a cartridge
// another library holds now always blocks, being a live conflict, and so do a
// library that fails the target application asked for and a cartridge the
// change makes leave its library.
package validate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
	FamilyFile     = "file"
)

// TagPreExisting marks a warning that is an error the configuration in force
// already has, untouched by the change.
const TagPreExisting = "pre-existing"

// Finding is one problem found in a configuration.
type Finding struct {
	Family string `json:"family"`
	// Code names the check (see codes.go): "<family>.<check>".
	Code string `json:"code"`
	// Library is the ID of the library concerned, or 0.
	Library int    `json:"library,omitempty"`
	Message string `json:"message"`
	Tag     string `json:"tag,omitempty"`
	// Elements are the libraries, drives and elements the finding concerns.
	// A change to any of them makes an error the configuration in force
	// already has block again.
	Elements []ElementRef `json:"elements,omitempty"`

	sticky bool // never reported as pre-existing
}

// ElementRef names a library, a drive, or an element of a library.
type ElementRef struct {
	Kind string `json:"kind"` // one of the Element* kinds
	// Library is the library the element belongs to. It is 0 for a drive:
	// device.conf numbers drives apart from libraries, and a drive can move
	// from one library to another.
	Library int `json:"library,omitempty"`
	// Number is the library or drive ID, or the element number (1-based, as
	// in library_contents).
	Number int `json:"number"`
}

// Element kinds.
const (
	ElementLibrary = "library"
	ElementDrive   = "drive"
	ElementSlot    = "slot"
	ElementMAP     = "map"
	ElementPicker  = "picker"
	// ElementDriveLine is a "Drive N:" line of library_contents.
	ElementDriveLine = "drive_line"
)

// Result holds the findings of Validate.
type Result struct {
	Errors   []Finding `json:"errors"`
	Warnings []Finding `json:"warnings"`
}

// OK reports whether nothing blocks the configuration.
func (r Result) OK() bool { return len(r.Errors) == 0 }

func newFinding(code string, lib int, els []ElementRef, format string, a []any) Finding {
	family, _, _ := strings.Cut(code, ".")
	return Finding{Family: family, Code: code, Library: lib, Message: fmt.Sprintf(format, a...), Elements: els}
}

func (r *Result) errorf(code string, lib int, els []ElementRef, format string, a ...any) {
	r.Errors = append(r.Errors, newFinding(code, lib, els, format, a))
}

// stickyErrorf records an error that blocks even when the configuration in
// force already has it: a conflict with the live host, or a library that does
// not meet the target application asked for.
func (r *Result) stickyErrorf(code string, lib int, els []ElementRef, format string, a ...any) {
	f := newFinding(code, lib, els, format, a)
	f.sticky = true
	r.Errors = append(r.Errors, f)
}

func (r *Result) warnf(code string, lib int, els []ElementRef, format string, a ...any) {
	r.Warnings = append(r.Warnings, newFinding(code, lib, els, format, a))
}

// Element references.
func libKey(id int) ElementRef        { return ElementRef{ElementLibrary, id, id} }
func driveKey(id int) ElementRef      { return ElementRef{ElementDrive, 0, id} }
func slotKey(lib, n int) ElementRef   { return ElementRef{ElementSlot, lib, n} }
func mapKey(lib, n int) ElementRef    { return ElementRef{ElementMAP, lib, n} }
func pickerKey(lib, n int) ElementRef { return ElementRef{ElementPicker, lib, n} }
func lineKey(lib, n int) ElementRef   { return ElementRef{ElementDriveLine, lib, n} }

// prints fingerprints every element of inv, to tell what a change touches.
// An element listed twice gets both values, so adding or removing a copy
// touches it.
func prints(inv config.Inventory) map[ElementRef]string {
	m := map[ElementRef]string{}
	for _, l := range inv.Libraries {
		bare := l
		bare.Drives, bare.Slots, bare.MAPs, bare.Pickers, bare.PickerMedia, bare.DriveLines = nil, nil, nil, nil, nil, nil
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
		for _, s := range l.PickerMedia {
			m[pickerKey(l.ID, s.Number)] += s.Barcode
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
	// Pickers is filled by Merge, from library_contents; READ ELEMENT
	// STATUS results leave it empty.
	Pickers map[int]string
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

// Disposition says what happens to a cartridge the change makes leave its
// library.
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
	// Dispositions say where the cartridges leaving a library go.
	Dispositions []Disposition
	// ConfigDir holds library_contents.<id>.persist; empty skips them.
	ConfigDir string
	// DefaultHome is the media home of libraries without a "Home directory"
	// (MHVTL_HOME_PATH); empty skips the home checks for them.
	DefaultHome string
	// CurDefaultHome is the default home of the configuration in force,
	// when the change may set another; empty means it equals DefaultHome.
	// A DefaultHome HomeProblem rejects is an error when it differs, a
	// pre-existing warning when not.
	CurDefaultHome string
	// HostErr is the error hostids.Read returned with the host's devices:
	// the serials and NAAs of the host were then not all compared.
	HostErr error
}

// Validate checks next, the proposed configuration, against cur, the one in
// force, the live placements and the host's SCSI devices.
func Validate(cur, next config.Inventory, live *Live, host []hostids.Device, opts Options) Result {
	r := check(cur, next, live, host, opts)
	before := check(cur, cur, live, host, Options{TargetApps: opts.TargetApps, ConfigDir: opts.ConfigDir, DefaultHome: opts.DefaultHome})
	type key struct {
		code    string
		lib     int
		message string
	}
	had := map[key]bool{}
	for _, f := range before.Errors {
		had[key{f.Code, f.Library, f.Message}] = true
	}
	pc, pn := prints(cur), prints(next)
	errs := r.Errors[:0]
	for _, f := range r.Errors {
		if f.sticky || !had[key{f.Code, f.Library, f.Message}] || touches(f, pc, pn) {
			errs = append(errs, f)
			continue
		}
		f.Tag = TagPreExisting
		r.Warnings = append(r.Warnings, f)
	}
	r.Errors = errs
	checkProblems(&r, cur, next)
	checkHomes(&r, cur, next)
	checkDefaultHome(&r, opts)
	return r
}

// checkDefaultHome applies HomeProblem to MHVTL_HOME_PATH, as the apply
// helper does when it derives mhvtl.conf: make_vtl_media creates the home
// of every library without its own there, as root.
func checkDefaultHome(r *Result, opts Options) {
	p := HomeProblem(opts.DefaultHome)
	if opts.DefaultHome == "" || p == "" {
		return
	}
	f := newFinding(CodeMediaHomeForbidden, 0, nil,
		"MHVTL_HOME_PATH %q cannot be a media home: %s", []any{opts.DefaultHome, p})
	if opts.CurDefaultHome == "" || opts.CurDefaultHome == opts.DefaultHome {
		f.Tag = TagPreExisting
		r.Warnings = append(r.Warnings, f)
		return
	}
	r.Errors = append(r.Errors, f)
}

// checkProblems reports the lines of the files read that mhVTL does not read
// as written (config.Inventory.Problems, from conffile). Every writer refuses
// a file with one (conffile Writable), so one in next blocks; one in cur
// that next no longer has is a pre-existing warning.
func checkProblems(r *Result, cur, next config.Inventory) {
	inNext := map[config.Problem]bool{}
	for _, p := range next.Problems {
		inNext[p] = true
		r.stickyErrorf(CodeFileProblem, 0, nil, "%v", p)
	}
	for _, p := range cur.Problems {
		if !inNext[p] {
			f := newFinding(CodeFileProblem, 0, nil, "%v", []any{p})
			f.Tag = TagPreExisting
			r.Warnings = append(r.Warnings, f)
		}
	}
}

// touches reports whether the change alters an element f concerns.
func touches(f Finding, cur, next map[ElementRef]string) bool {
	for _, k := range f.Elements {
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
	checkIdentities(&r, cur, next, host, opts.HostErr)
	checkPresets(&r, next, opts)
	checkMedia(&r, cur, next, live, opts)
	checkBarcodes(&r, cur, next, live, opts)
	checkRemovals(&r, cur, next, live, opts)
	return r
}

// forbiddenHomes are the directories a media home may not be, nor lie
// under: system trees, pseudo-filesystems, logs and spools, systemd's and
// mhvtl-control's own state. mktape and make_vtl_media create directories
// and files in the home as root, so a home there would let a configuration
// change write into them. /home and /root are also hidden from
// mhvtl-control by its unit's ProtectHome=yes, as /run/user is (under
// /run), so cartridges there would look missing to the daemon.
var forbiddenHomes = []string{
	"/etc", "/proc", "/sys", "/dev", "/run", "/boot", "/usr",
	"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/root", "/home",
	"/var/lib/systemd", "/var/spool", "/var/log",
	"/var/lib/mhvtl-control-apply", "/var/lib/mhvtl-control",
}

var homeChars = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// HomeProblem says why path cannot be a media home, or returns "". A home
// is an absolute, clean path of letters, digits, '.', '_', '-' and '/'
// (so the shell that sources mhvtl.conf and splits $LIBHOME reads it as
// one word), and is not "/", nor one of forbiddenHomes or under one.
func HomeProblem(path string) string {
	switch {
	case !homeChars.MatchString(path):
		return "not an absolute path of letters, digits, '.', '_', '-' and '/'"
	case filepath.Clean(path) != path:
		return "not a clean path (no '..', '.', '//' or trailing '/')"
	case path == "/":
		return "the root directory"
	}
	for _, f := range forbiddenHomes {
		if path == f || strings.HasPrefix(path, f+"/") {
			return "under " + f
		}
	}
	return ""
}

// checkHomes refuses a "Home directory" with a blank in it, and one
// HomeProblem rejects.
// find_media_home_directory reads it with sscanf "%s" and stops at the blank
// (usr/vtllib.c:1723, upstream master 852b78a), and make_vtl_media uses
// $LIBHOME unquoted (usr/cmd/make_vtl_media.in:355-377), so neither finds the
// directory meant. One the configuration in force already has unchanged is a
// pre-existing warning, whatever else the change touches in the library.
func checkHomes(r *Result, cur, next config.Inventory) {
	for _, lib := range next.Libraries {
		var f Finding
		switch {
		case lib.HomeDir == "":
			continue
		case strings.ContainsAny(lib.HomeDir, " \t\v\f\r"):
			f = newFinding(CodeMediaHomeBlank, lib.ID, []ElementRef{libKey(lib.ID)},
				"library %d: Home directory %q has a blank in it; vtltape reads the home up to the first blank and make_vtl_media splits it into words", []any{lib.ID, lib.HomeDir})
		case HomeProblem(lib.HomeDir) != "":
			f = newFinding(CodeMediaHomeForbidden, lib.ID, []ElementRef{libKey(lib.ID)},
				"library %d: Home directory %q cannot be a media home: %s", []any{lib.ID, lib.HomeDir, HomeProblem(lib.HomeDir)})
		default:
			continue
		}
		if c, ok := findLibrary(cur, lib.ID); ok && c.HomeDir == lib.HomeDir {
			f.Tag = TagPreExisting
			r.Warnings = append(r.Warnings, f)
			continue
		}
		r.Errors = append(r.Errors, f)
	}
}

// homeOf returns the media home of lib, or "" when unknown: its own
// "Home directory", else the default. That is how vtltape finds a library's
// cartridges (find_media_home_directory, mhvtl usr/vtllib.c:1673-1746) and
// where make_vtl_media creates them (library_home, usr/cmd/make_vtl_media.in
// :58-73, used by default since PER_LIBRARY_HOME is 1, :32 and :349-353)
// from upstream master 993b193 on, with markh794/mhvtl#275 and #276
// (f20c8bc, 62c0d01). Before them, a library without a home took the next
// library's, and make_vtl_media put every cartridge in MHVTL_HOME_PATH.
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
