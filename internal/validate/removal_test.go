package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

func TestRemovals(t *testing.T) {
	cur := func() config.Inventory {
		l := lib(10, "IBM", "03584L32", 1, "A00001L8", "", "A00002L8", "")
		l.MAPs = []config.MAPSlot{{Number: 1}, {Number: 2}}
		return inv(l)
	}
	// live is the live placement of cur's file plus extra.
	live := func(extra Placement) *Live {
		p := Placement{Slots: map[int]string{1: "A00001L8", 3: "A00002L8"}, MAPs: map[int]string{}, Drives: extra.Drives}
		for n, b := range extra.Slots {
			p.Slots[n] = b
		}
		for n, b := range extra.MAPs {
			p.MAPs[n] = b
		}
		return &Live{Libraries: map[int]Placement{10: p}}
	}
	// Removes slots 3 (holding A00002L8) and 4 (empty).
	dropSlot3 := func(i *config.Inventory) { i.Libraries[0].Slots = i.Libraries[0].Slots[:2] }
	for _, tc := range []struct {
		name   string
		next   func(*config.Inventory)
		live   *Live
		disp   []Disposition
		errors []string
	}{
		{"remove an empty slot", func(i *config.Inventory) { i.Libraries[0].Slots = i.Libraries[0].Slots[:3] }, nil, nil, nil},
		{"remove a full slot without saying", dropSlot3, nil, nil,
			[]string{"library 10: A00002L8 (slot 3) would leave the library; say where it goes: a storage slot, export, or shelf"}},
		{"clear a kept slot without saying", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "" }, nil, nil,
			[]string{"library 10: A00002L8 (slot 3) would leave the library"}},
		{"clear a kept slot to the shelf", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "" }, nil,
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}}, nil},
		{"turn a full slot into a gap", func(i *config.Inventory) {
			l := &i.Libraries[0]
			l.Slots = []config.Slot{l.Slots[0], l.Slots[1], l.Slots[3]}
		}, nil, nil, []string{"library 10: A00002L8 (slot 3) would leave the library"}},
		{"move within the slots", func(i *config.Inventory) {
			l := &i.Libraries[0]
			l.Slots[1].Barcode, l.Slots[2].Barcode = "A00002L8", ""
		}, nil, nil, nil},
		{"move to a MAP slot", func(i *config.Inventory) {
			l := &i.Libraries[0]
			l.Slots[2].Barcode, l.MAPs[0].Barcode = "", "A00002L8"
		}, nil, nil, nil},
		{"loaded in a drive now", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "" },
			&Live{Libraries: map[int]Placement{10: {Slots: map[int]string{1: "A00001L8"}, Drives: map[int]string{1: "A00002L8"}}}}, nil, nil},
		{"to the shelf", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}}, nil},
		{"to a free slot", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 2}}, nil},
		{"to a slot that lists another cartridge", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 1}},
			[]string{"library 10: A00002L8 is to go to slot 1, which lists A00001L8"}},
		{"to a slot that holds a cartridge now", dropSlot3,
			live(Placement{Slots: map[int]string{2: "X00001L8"}}),
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 2}},
			[]string{"library 10: A00002L8 is to go to slot 2, which holds X00001L8 now"}},
		{"to a slot that is gone", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 9}},
			[]string{"library 10: A00002L8 is to go to slot 9, which the new configuration does not have"}},
		{"export with free MAP slots", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}}, nil},
		{"export with the MAP full", func(i *config.Inventory) {
			dropSlot3(i)
			i.Libraries[0].MAPs[0].Barcode = "M00001L8"
		}, live(Placement{MAPs: map[int]string{2: "M00002L8"}}),
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}},
			[]string{"library 10: 1 cartridges are to be exported, but only 0 MAP slots are free"}},
		{"a deleted slot holding a cartridge moved there at run time", dropSlot3,
			live(Placement{Slots: map[int]string{4: "L00009L8"}}),
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}},
			[]string{"library 10: L00009L8 (slot 4) would leave the library"}},
		{"the cartridge is listed elsewhere in the new file", func(i *config.Inventory) {
			dropSlot3(i)
			i.Libraries[0].Slots[1].Barcode = "A00002L8"
		}, nil, nil, nil},
		// vtllibrary makes the slots a gap skips (usr/cmd/vtllibrary.c
		// :1054-1064), so dropping a line below the last one removes no slot.
		{"a gap slot is kept, and only the cartridge leaves", func(i *config.Inventory) {
			i.Libraries[0].Slots = []config.Slot{{Number: 1, Barcode: "A00001L8"}, {Number: 5}}
		}, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 3}}, nil},
		{"to a gap slot", func(i *config.Inventory) {
			dropSlot3(i)
			i.Libraries[0].Slots = append(i.Libraries[0].Slots, config.Slot{Number: 8})
			i.Libraries[0].MAPs = i.Libraries[0].MAPs[:1]
		}, live(Placement{MAPs: map[int]string{2: "M00002L8"}}),
			// Slot 3 is now a gap, so A00002L8 stays in it.
			[]Disposition{{Library: 10, Barcode: "M00002L8", To: ToSlot, Slot: 6}}, nil},
		{"remove a MAP slot holding a cartridge", func(i *config.Inventory) { i.Libraries[0].MAPs = i.Libraries[0].MAPs[:1] },
			live(Placement{MAPs: map[int]string{2: "M00002L8"}}), nil,
			[]string{"library 10: M00002L8 (MAP slot 2) would leave the library"}},
		// The apply writes next merged with the live placement (plan 3.3 P1).
		{"a cartridge imported at run time stays", func(*config.Inventory) {},
			live(Placement{Slots: map[int]string{4: "NEW001L8"}, MAPs: map[int]string{1: "NEW002L8"}}), nil, nil},
		{"a live cartridge stays in a slot next clears", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "" },
			live(Placement{}), nil, nil},
		{"a slot reassigned to another barcode", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "B00009L8" },
			live(Placement{}), nil, []string{"library 10: A00002L8 (slot 3) would leave the library"}},
		{"a slot reassigned to another barcode, live unknown", func(i *config.Inventory) { i.Libraries[0].Slots[2].Barcode = "B00009L8" },
			nil, nil, []string{"library 10: A00002L8 (slot 3) would leave the library"}},
		{"a live cartridge moved by an edit", func(i *config.Inventory) {
			i.Libraries[0].Slots[1].Barcode = "NEW001L8"
		}, live(Placement{Slots: map[int]string{4: "NEW001L8"}}), nil, nil},
		{"a disposition for nothing removed", func(*config.Inventory) {}, nil, []Disposition{{Library: 10, Barcode: "A00001L8", To: ToShelf}},
			[]string{"library 10: a disposition names A00001L8, but it does not leave the library"}},
		{"two dispositions", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}, {Library: 10, Barcode: "A00002L8", To: ToExport}},
			[]string{"library 10: two dispositions for A00002L8"}},
		{"unknown destination", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: "bin"}},
			[]string{`library 10: unknown destination "bin" for A00002L8; want slot, export or shelf`}},
		{"unknown library", func(*config.Inventory) {}, nil, []Disposition{{Library: 99, Barcode: "A00002L8", To: ToShelf}},
			[]string{"dispositions name library 99, which is not in both the current and the new configuration"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := cur()
			tc.next(&next)
			r := Validate(cur(), next, tc.live, nil, Options{Dispositions: tc.disp})
			expect(t, "errors", family(r.Errors, FamilyRemoval), tc.errors)
		})
	}
}

// Probes from the PR 2a re-review on the configuration upstream c65bff0's
// generate_device_conf and generate_library_contents write: cartridges that
// were imported at run time (I/E API, vtlcmd, bay monitor) and are not in
// library_contents stay, since the apply merges the live placement.
func TestLiveImports(t *testing.T) {
	cur := gen(t)
	for _, tc := range []struct {
		name string
		edit func(*config.Inventory)
		live Placement
	}{
		{"an unrelated edit, a cartridge imported to a slot", func(i *config.Inventory) { i.Libraries[1].Drives[0].Product = "T10000C" },
			Placement{Slots: map[int]string{5: "NEW001L8"}}},
		{"no change, a cartridge imported to a slot", func(*config.Inventory) {}, Placement{Slots: map[int]string{5: "NEW001L8"}}},
		{"no change, a cartridge imported to a MAP slot", func(*config.Inventory) {}, Placement{MAPs: map[int]string{1: "NEW002L8"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := clone(cur)
			tc.edit(&next)
			r := Validate(cur, next, &Live{Libraries: map[int]Placement{10: tc.live}}, nil, Options{})
			expect(t, "errors", family(r.Errors, FamilyRemoval), nil)
		})
	}
}

// A cartridge the bay monitor placed (a symlink in the home) never leaves:
// the bay monitor places it again after the apply.
func TestBayCartridgeNeverLeaves(t *testing.T) {
	l := lib(10, "IBM", "03584L32", 1, "")
	l.HomeDir = "testdata/homes/b"
	l.MAPs = []config.MAPSlot{{Number: 1}, {Number: 2}}
	cur := inv(l)
	next := clone(cur)
	next.Libraries[0].MAPs = next.Libraries[0].MAPs[:1]
	live := &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "R00001L8"}}}}
	r := Validate(cur, next, live, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyRemoval), nil)
	live.Libraries[10].MAPs[2] = "G03001L8" // a file cartridge
	r = Validate(cur, next, live, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyRemoval), []string{"library 10: G03001L8 (MAP slot 2) would leave the library"})
}
