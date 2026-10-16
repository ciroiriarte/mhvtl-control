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
			&Live{Libraries: map[int]Placement{10: {Drives: map[int]string{1: "A00002L8"}}}}, nil, nil},
		{"to the shelf", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}}, nil},
		{"to a free slot", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 2}}, nil},
		{"to a slot that lists another cartridge", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 1}},
			[]string{"library 10: A00002L8 is to go to slot 1, which lists A00001L8"}},
		{"to a slot that holds a cartridge now", dropSlot3,
			&Live{Libraries: map[int]Placement{10: {Slots: map[int]string{1: "A00001L8", 2: "X00001L8", 3: "A00002L8"}}}},
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 2}},
			// X00001L8 is not in the file, so it leaves too.
			[]string{"library 10: A00002L8 is to go to slot 2, which holds X00001L8 now",
				"library 10: X00001L8 (slot 2) would leave the library"}},
		{"to a slot that is gone", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 9}},
			[]string{"library 10: A00002L8 is to go to slot 9, which the new configuration does not have"}},
		{"export with free MAP slots", dropSlot3, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}}, nil},
		{"export with the MAP full", func(i *config.Inventory) {
			dropSlot3(i)
			i.Libraries[0].MAPs[0].Barcode = "M00001L8"
		}, &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "M00002L8"}}}},
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}},
			[]string{"library 10: M00002L8 (MAP slot 2) would leave the library",
				"library 10: 1 cartridges are to be exported, but only 0 MAP slots are free"}},
		{"a cartridge moved there at run time", dropSlot3,
			&Live{Libraries: map[int]Placement{10: {Slots: map[int]string{4: "L00009L8"}}}},
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
		}, &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "M00002L8"}}}},
			[]Disposition{{Library: 10, Barcode: "M00002L8", To: ToSlot, Slot: 6}, {Library: 10, Barcode: "A00002L8", To: ToShelf}}, nil},
		{"remove a MAP slot holding a cartridge", func(i *config.Inventory) { i.Libraries[0].MAPs = i.Libraries[0].MAPs[:1] },
			&Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "M00002L8"}}}}, nil,
			[]string{"library 10: M00002L8 (MAP slot 2) would leave the library"}},
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
