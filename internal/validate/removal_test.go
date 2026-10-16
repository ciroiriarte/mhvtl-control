package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

func TestRemovals(t *testing.T) {
	cur := func() config.Inventory {
		l := lib(10, "IBM", "03584L32", 1, "A00001L8", "A00002L8", "")
		l.MAPs = []config.MAPSlot{{Number: 1}, {Number: 2}}
		return inv(l)
	}
	dropSlot2 := func(i *config.Inventory) {
		l := &i.Libraries[0]
		l.Slots = []config.Slot{l.Slots[0], l.Slots[2]}
	}
	for _, tc := range []struct {
		name   string
		next   func(*config.Inventory)
		live   *Live
		disp   []Disposition
		errors []string
	}{
		{"remove an empty slot", func(i *config.Inventory) { i.Libraries[0].Slots = i.Libraries[0].Slots[:2] }, nil, nil, nil},
		{"remove a full slot without saying", dropSlot2, nil, nil,
			[]string{"library 10: removing slot 2 would drop A00002L8; say where it goes: a storage slot, export, or shelf"}},
		{"to the shelf", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}}, nil},
		{"to a free slot", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 3}}, nil},
		{"to a slot that lists another cartridge", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 1}},
			[]string{"library 10: A00002L8 is to go to slot 1, which lists A00001L8"}},
		{"to a slot that holds a cartridge now", dropSlot2,
			&Live{Libraries: map[int]Placement{10: {Slots: map[int]string{1: "A00001L8", 2: "A00002L8", 3: "X00001L8"}}}},
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 3}},
			[]string{"library 10: A00002L8 is to go to slot 3, which holds X00001L8 now"}},
		{"to a slot that is gone", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToSlot, Slot: 9}},
			[]string{"library 10: A00002L8 is to go to slot 9, which the new configuration does not have"}},
		{"export with free MAP slots", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}}, nil},
		{"export with the MAP full", func(i *config.Inventory) {
			dropSlot2(i)
			i.Libraries[0].MAPs[0].Barcode = "M00001L8"
		}, &Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "M00002L8"}}}},
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToExport}},
			[]string{"library 10: 1 cartridges are to be exported, but only 0 MAP slots are free"}},
		{"a cartridge moved there at run time", dropSlot2,
			&Live{Libraries: map[int]Placement{10: {Slots: map[int]string{2: "L00009L8"}}}},
			[]Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}},
			[]string{"library 10: removing slot 2 would drop L00009L8"}},
		{"the cartridge is listed elsewhere in the new file", func(i *config.Inventory) {
			dropSlot2(i)
			i.Libraries[0].Slots[1].Barcode = "A00002L8"
		}, nil, nil, nil},
		{"remove a MAP slot holding a cartridge", func(i *config.Inventory) { i.Libraries[0].MAPs = i.Libraries[0].MAPs[:1] },
			&Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{2: "M00002L8"}}}}, nil,
			[]string{"library 10: removing MAP slot 2 would drop M00002L8"}},
		{"a disposition for nothing removed", func(*config.Inventory) {}, nil, []Disposition{{Library: 10, Barcode: "A00001L8", To: ToShelf}},
			[]string{"library 10: a disposition names A00001L8, but no removed slot holds it"}},
		{"two dispositions", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: ToShelf}, {Library: 10, Barcode: "A00002L8", To: ToExport}},
			[]string{"library 10: two dispositions for A00002L8"}},
		{"unknown destination", dropSlot2, nil, []Disposition{{Library: 10, Barcode: "A00002L8", To: "bin"}},
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
