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
		{"MAP slot naming a cartridge that does not exist", func(l *config.Library) {
			l.MAPs[0].Barcode = "E09001L8"
			l.HomeDir = "testdata/homes/a"
		}, []string{"library 10 MAP slot 1: E09001L8: MAP and Picker entries are not created by make_vtl_media, and it is not in testdata/homes/a"}, nil},
		{"picker naming a cartridge with no home known", func(l *config.Library) {
			l.PickerMedia = []config.Slot{{Number: 1, Barcode: "E09001L8"}}
			l.HomeDir = ""
		}, nil, []string{"library 10 picker 1: E09001L8: MAP and Picker entries are not created by make_vtl_media, and no media home is known to check that it exists"}},
		{"MAP slot with an existing cartridge", func(l *config.Library) {
			l.MAPs[0].Barcode = "E01001L8"
			l.HomeDir = "testdata/homes/a"
		}, nil, nil},
		{"picker with an existing cartridge no drive loads", func(l *config.Library) {
			l.PickerMedia = []config.Slot{{Number: 1, Barcode: "E01001L8"}}
			l.HomeDir = "testdata/homes/a"
			l.Drives = l.Drives[2:]
			l.Slots = nil
		}, []string{"library 10 picker 1: E01001L8 is LTO8 media (from its barcode), which none of the library's drives (ULT3580-TD6, ULT3580-TD6) loads"}, nil},
		{"LTO-4 read-only in an LTO-6 drive", func(l *config.Library) {
			l.Slots[0].Barcode = "E09001L4"
		}, nil, nil},
		// make_vtl_media reads the media type from the tail, as set_density
		// does (make_vtl_media.in:120); 14 characters is within vtllibrary's 16.
		{"a long barcode", func(l *config.Library) {
			l.Slots[4].Barcode = "ABCDEFGHIJ01L8"
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
		{"WORM media in a drive without WORM support", func(l *config.Library) {
			l.Drives = l.Drives[:1]
			l.Drives[0].Product = "ULT3580-TD2"
			l.Slots = []config.Slot{{Number: 1, Barcode: "W00001L2"}, {Number: 2, Barcode: "E00002L2"}}
			l.MAPs = nil
		}, []string{"library 10 slot 1: W00001L2 is LTO2 WORM media (from its barcode), and none of the library's drives (ULT3580-TD2) that load LTO2 supports WORM"}, nil},
		{"WORM media in a drive with WORM support", func(l *config.Library) {
			l.Slots[0].Barcode = "W00001L6"
		}, nil, nil},
		{"a home ProtectHome hides", func(l *config.Library) {
			l.HomeDir = "/home/tapes"
		}, nil, []string{"library 10: media home /home/tapes is under /home, /root or /run/user"}},
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

// A MAP entry the live placement already holds in that MAP slot is in the
// library, not a pre-fill: it need not be in the home. The same barcode in
// another MAP slot, or with the live placement unknown, still must be.
func TestMediaLiveMAP(t *testing.T) {
	cur := shipped(t)
	next := clone(cur)
	next.Libraries[0].HomeDir = "testdata/homes/a"
	next.Libraries[0].MAPs[0].Barcode = "E09001L8"
	notInHome := []string{"library 10 MAP slot 1: E09001L8: MAP and Picker entries are not created by make_vtl_media, and it is not in testdata/homes/a"}
	for _, tc := range []struct {
		name   string
		live   *Live
		errors []string
	}{
		{"in that MAP slot live", &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{1: "E09001L8"}}}}, nil},
		{"in another MAP slot live", &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "E09001L8"}}}}, notInHome},
		{"live unknown", nil, notInHome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(cur, next, tc.live, nil, Options{})
			expect(t, "errors", family(r.Errors, FamilyMedia), tc.errors)
		})
	}
}
