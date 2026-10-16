package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

func TestMedia(t *testing.T) {
	// Library 10 of the shipped configuration: two ULT3580-TD8, two
	// ULT3580-TD6, LTO-8 and LTO-6 cartridges.
	for _, tc := range []struct {
		name          string
		edit          func(*config.Library)
		errors, warns []string
	}{
		{"shipped", func(*config.Library) {}, nil, nil},
		{"LTO-6 with LTO-8 drives only", func(l *config.Library) {
			l.Drives = l.Drives[:2]
		}, []string{
			"library 10 slot 23: CLN102L6 is LTO6 media (from its barcode), which none of the library's drives (ULT3580-TD8, ULT3580-TD8) loads",
			"slot 30: F01030L6", "slot 31", "slot 32", "slot 33", "slot 34", "slot 35", "slot 36", "slot 37", "slot 38",
			"slot 39: F01039L6 is LTO6 media",
		}, nil},
		{"pre-filled MAP slot", func(l *config.Library) {
			l.MAPs[0].Barcode = "E09001L3"
		}, []string{"library 10 MAP slot 1: E09001L3 is LTO3 media (from its barcode), which none of the library's drives (ULT3580-TD8, ULT3580-TD8, ULT3580-TD6, ULT3580-TD6) loads"}, nil},
		{"LTO-4 read-only in an LTO-6 drive", func(l *config.Library) {
			l.Slots[0].Barcode = "E09001L4"
		}, nil, nil},
		{"barcode mktape rejects", func(l *config.Library) {
			l.Slots[0].Barcode = "ABC"
		}, []string{"library 10 slot 1: make_vtl_media cannot create ABC: its barcode gives density UNKNOWN, which mktape does not accept"}, nil},
		{"an existing cartridge with an odd barcode", func(l *config.Library) {
			l.Slots[0].Barcode = "ABC"
			l.HomeDir = "testdata/homes/a"
		}, nil, []string{"library 10 slot 1: the media type of ABC cannot be told from its barcode"}},
		{"a density make_vtl_media derives but mktape rejects", func(l *config.Library) {
			l.Slots[0].Barcode = "E09001TK"
		}, []string{"its barcode gives density T10KK, which mktape does not accept"}, nil},
		{"no drives", func(l *config.Library) {
			l.Drives = nil
			l.Slots = l.Slots[:1]
			l.MAPs = nil
		}, nil, []string{"library 10 slot 1: E01001L8 is LTO8 media, but the library has no drives"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := shipped(t)
			next := clone(cur)
			tc.edit(&next.Libraries[0])
			r := Validate(cur, next, nil, nil, Options{})
			expect(t, "errors", family(r.Errors, FamilyMedia), tc.errors)
			expect(t, "warnings", family(r.Warnings, FamilyMedia), tc.warns)
		})
	}
}
