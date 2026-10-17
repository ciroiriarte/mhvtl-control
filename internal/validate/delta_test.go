package validate

import (
	"testing"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
)

// An error the configuration in force already has blocks only when the change
// touches what it concerns; a host collision blocks either way.
func TestPreExisting(t *testing.T) {
	// Drives 11 and 12 share a serial in the configuration in force.
	dup := func(i *config.Inventory) { i.Libraries[0].Drives[1].Serial = "XYZZY_A1" }
	const msg = `drive 11 and drive 12 share serial "XYZZY_A1"`
	for _, tc := range []struct {
		name        string
		cur, edit   func(*config.Inventory)
		host        []hostids.Device
		errors, pre []string
	}{
		{"untouched error warns", dup, func(i *config.Inventory) {
			i.Libraries[1].Slots = append(i.Libraries[1].Slots, config.Slot{Number: 99})
		}, nil, nil, []string{msg}},
		{"touching an unrelated field of the library warns", dup, func(i *config.Inventory) {
			i.Libraries[1].Serial = "XYZZY_C"
		}, nil, nil, []string{msg}},
		{"touching the same drive blocks", dup, func(i *config.Inventory) {
			i.Libraries[0].Drives[1].Revision = "0102"
		}, nil, []string{msg}, nil},
		{"the error moved to another element blocks", dup, func(i *config.Inventory) {
			i.Libraries[0].Drives[1].Serial = "XYZZY_A2"
			i.Libraries[0].Drives[2].Serial = "XYZZY_A1"
		}, nil, []string{`drive 11 and drive 13 share serial "XYZZY_A1"`}, nil},
		{"a new error blocks", func(*config.Inventory) {}, dup, nil, []string{msg}, nil},
		{"a fixed error is gone", dup, func(i *config.Inventory) {
			i.Libraries[0].Drives[1].Serial = "XYZZY_A2"
		}, nil, nil, nil},
		{"a host collision in force still blocks", func(*config.Inventory) {}, func(*config.Inventory) {},
			[]hostids.Device{{Host: 0, Driver: "ahci", Serial: "00XYZZY_A1"}},
			[]string{`drive 11: serial "XYZZY_A1" (reported as "00XYZZY_A1") is already used by host device 0:0:0:0 (ahci)`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := shipped(t)
			tc.cur(&cur)
			next := clone(cur)
			tc.edit(&next)
			r := Validate(cur, next, nil, tc.host, Options{})
			expect(t, "errors", family(r.Errors, FamilyIdentity), tc.errors)
			var pre []string
			for _, w := range r.Warnings {
				if w.Tag == TagPreExisting {
					pre = append(pre, w.Message)
				}
			}
			expect(t, "pre-existing warnings", pre, tc.pre)
		})
	}
}

// Each family names the elements its errors concern, so editing one of them
// keeps the error blocking.
func TestPreExistingOtherFamilies(t *testing.T) {
	pickers := func(i *config.Inventory) { i.Libraries[0].Pickers = []int{1, 1} }
	barcode := func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = i.Libraries[0].Slots[0].Barcode }
	media := func(i *config.Inventory) { i.Libraries[0].Slots[0].Barcode = "BAD001JA" }
	other := func(i *config.Inventory) { i.Libraries[1].Serial = "XYZZY_C" }
	for _, tc := range []struct {
		name   string
		cur    func(*config.Inventory)
		edit   func(*config.Inventory)
		pbs    bool
		family string
		blocks bool
	}{
		{"address: untouched", pickers, other, false, FamilyAddress, false},
		{"address: another picker added", pickers,
			func(i *config.Inventory) { i.Libraries[0].Pickers = []int{1, 1, 2} }, false, FamilyAddress, false},
		{"address: a third copy of the picker", pickers,
			func(i *config.Inventory) { i.Libraries[0].Pickers = []int{1, 1, 1} }, false, FamilyAddress, true},
		{"barcode: untouched", barcode, other, false, FamilyBarcode, false},
		{"barcode: a third copy", barcode,
			func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = i.Libraries[0].Slots[0].Barcode }, false, FamilyBarcode, true},
		{"media: untouched", media, other, false, FamilyMedia, false},
		{"media: a drive of the library edited", media,
			func(i *config.Inventory) { i.Libraries[0].Drives[0].Revision = "0102" }, false, FamilyMedia, true},
		{"preset: the target application asked for always blocks", func(*config.Inventory) {}, other, true, FamilyPreset, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := shipped(t)
			tc.cur(&cur)
			next := clone(cur)
			tc.edit(&next)
			var opts Options
			if tc.pbs {
				opts.TargetApps = map[int]string{10: PresetPBS}
			}
			r := Validate(cur, next, nil, nil, opts)
			if got := len(family(r.Errors, tc.family)) > 0; got != tc.blocks {
				t.Errorf("blocks = %v, want %v; errors %+v, warnings %+v", got, tc.blocks, r.Errors, r.Warnings)
			}
			pre := 0
			for _, w := range r.Warnings {
				if w.Family == tc.family && w.Tag == TagPreExisting {
					pre++
				}
			}
			if !tc.blocks && pre != 1 {
				t.Errorf("%d pre-existing %s warnings, want 1: %+v", pre, tc.family, r.Warnings)
			}
		})
	}
}

// A drive serial over the policy limit that the configuration in force
// already has does not block an edit of another drive; it blocks an edit of
// that drive.
func TestLongSerialPreExisting(t *testing.T) {
	cur := shipped(t)
	cur.Libraries[0].Drives[0].Serial = "ABCDEFGHIJKL"
	other := clone(cur)
	other.Libraries[0].Drives[1].Revision = "0102"
	r := Validate(cur, other, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyIdentity), nil)
	var pre []string
	for _, w := range r.Warnings {
		if w.Code == CodeIdentSerialLength && w.Tag == TagPreExisting {
			pre = append(pre, w.Message)
		}
	}
	expect(t, "pre-existing warnings", pre, []string{`drive 11: serial "ABCDEFGHIJKL" is 12 characters; mhvtl-control's policy limits drive serials to 10 (mhVTL itself takes 40)`})

	same := clone(cur)
	same.Libraries[0].Drives[0].Revision = "0102"
	r = Validate(cur, same, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyIdentity), []string{`drive 11: serial "ABCDEFGHIJKL" is 12 characters`})
}
