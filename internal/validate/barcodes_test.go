package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

// Fixtures: testdata/homes/a holds the file cartridges E01001L8 and ABC,
// testdata/homes/b the file cartridge G03001L8 and the raw cartridge R00001L8
// (a bay monitor symlink); testdata/conf/library_contents.30.persist lists
// P00001L8.
func TestBarcodes(t *testing.T) {
	base := func() config.Inventory {
		a := lib(10, "IBM", "03584L32", 2, "A00001L8", "")
		a.HomeDir = "testdata/homes/a"
		b := lib(30, "IBM", "03584L32", 2, "B00001L8", "")
		b.HomeDir = "testdata/homes/b"
		return inv(a, b)
	}
	for _, tc := range []struct {
		name   string
		cur    func(*config.Inventory)
		next   func(*config.Inventory)
		live   *Live
		errors []string
	}{
		{"unchanged", nil, nil, nil, nil},
		{"listed by two libraries", nil, func(i *config.Inventory) { i.Libraries[1].Slots[1].Barcode = "A00001L8" }, nil,
			[]string{"barcode A00001L8 is listed 2 times: library 10 slot 1, library 30 slot 2"}},
		{"listed twice in one library", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "A00001L8" }, nil,
			[]string{"barcode A00001L8 is listed 2 times: library 10 slot 1, library 10 slot 2"}},
		{"in another library's .persist", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "P00001L8" }, nil,
			[]string{"barcode P00001L8 (library 10 slot 2) is in library 30's library_contents.30.persist"}},
		{"in its own library's .persist", nil, func(i *config.Inventory) { i.Libraries[1].Slots[1].Barcode = "P00001L8" }, nil, nil},
		{"held live by another library", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "C00001L8" },
			&Live{Libraries: map[int]Placement{30: {Drives: map[int]string{1: "C00001L8"}}}},
			[]string{"barcode C00001L8 (library 10 slot 2) is held by library 30 now"}},
		{"new, but a cartridge in another library's home", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "G03001L8" }, nil,
			[]string{"barcode G03001L8 (library 10 slot 2) is new to library 10, but a cartridge G03001L8 already exists in testdata/homes/b, the home of library 30"}},
		{"new, but a raw cartridge in another library's home", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "R00001L8" }, nil,
			[]string{"a cartridge R00001L8 already exists in testdata/homes/b"}},
		{"new, in its own home", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "E01001L8" }, nil, nil},
		{"already listed before", func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "G03001L8" },
			func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "G03001L8" }, nil,
			[]string{}},
		{"held live by the same library", nil, func(i *config.Inventory) { i.Libraries[0].Slots[1].Barcode = "G03001L8" },
			&Live{Libraries: map[int]Placement{10: {MAPs: map[int]string{1: "G03001L8"}}}}, nil},
		{"the home of a deleted library counts", func(i *config.Inventory) {
			i.Libraries[1].ID = 50
		}, func(i *config.Inventory) {
			i.Libraries = i.Libraries[:1]
			i.Libraries[0].Slots[1].Barcode = "G03001L8"
		}, nil, []string{"already exists in testdata/homes/b, the home of library 50"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur, next := base(), base()
			if tc.cur != nil {
				tc.cur(&cur)
			}
			if tc.next != nil {
				tc.next(&next)
			}
			r := Validate(cur, next, tc.live, nil, Options{ConfigDir: "testdata/conf"})
			expect(t, "errors", family(r.Errors, FamilyBarcode), tc.errors)
		})
	}
}

func TestBarcodesDefaultHome(t *testing.T) {
	// Library 10 uses MHVTL_HOME_PATH (a); library 30 has its own home (b).
	a := lib(10, "IBM", "03584L32", 1, "")
	b := lib(30, "IBM", "03584L32", 1, "")
	b.HomeDir = "testdata/homes/b"
	next := clone(inv(a, b))
	next.Libraries[1].Slots[0].Barcode = "E01001L8"
	r := Validate(inv(a, b), next, nil, nil, Options{DefaultHome: "testdata/homes/a"})
	expect(t, "errors", family(r.Errors, FamilyBarcode),
		[]string{"a cartridge E01001L8 already exists in testdata/homes/a, the home of library 10"})
	// Without a known default home, library 10's home is not checked.
	r = Validate(inv(a, b), next, nil, nil, Options{})
	expect(t, "errors", family(r.Errors, FamilyBarcode), nil)
}
