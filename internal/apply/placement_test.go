package apply

import (
	"fmt"
	"strings"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// plib is library 10 with slots and MAP slots as listed ("" empty).
func plib(slots, maps []string) config.Library {
	l := config.Library{ID: 10}
	for i, b := range slots {
		l.Slots = append(l.Slots, config.Slot{Number: i + 1, Barcode: b})
	}
	for i, b := range maps {
		l.MAPs = append(l.MAPs, config.MAPSlot{Number: i + 1, Barcode: b})
	}
	return l
}

func pl(slots, maps map[int]string) validate.Placement {
	p := newPlacement()
	for k, v := range slots {
		p.Slots[k] = v
	}
	for k, v := range maps {
		p.MAPs[k] = v
	}
	return p
}

func shortP(p validate.Placement) string {
	var b strings.Builder
	for _, n := range sortedInts(p.Slots) {
		fmt.Fprintf(&b, "S%d=%s ", n, p.Slots[n])
	}
	for _, n := range sortedInts(p.MAPs) {
		fmt.Fprintf(&b, "M%d=%s ", n, p.MAPs[n])
	}
	return strings.TrimSpace(b.String())
}

func noBay(string) bool { return false }

// TestMergeLibrary covers the merge of plan 3.3 exec step 5
// (placement_test.go): run-time moves and exports kept, dispositions, and
// a forced drive's cartridge to its source slot, else the first empty
// one, else refused.
func TestMergeLibrary(t *testing.T) {
	cur := plib([]string{"A", "B", "C", ""}, []string{"", ""})
	for _, tc := range []struct {
		name   string
		next   config.Library
		src    validate.Placement
		disp   []validate.Disposition
		forced []changer.DriveState
		want   string
		err    string
	}{
		{name: "moved at run time, next from the live view",
			next: plib([]string{"", "B", "C", "A"}, []string{"", ""}),
			src:  pl(map[int]string{2: "B", 3: "C", 4: "A"}, nil),
			want: "S2=B S3=C S4=A"},
		{name: "exported at run time",
			next: plib([]string{"A", "B", "", ""}, []string{"C", ""}),
			src:  pl(map[int]string{1: "A", 2: "B"}, map[int]string{1: "C"}),
			want: "S1=A S2=B M1=C"},
		{name: "a slot deleted, its cartridge to slot 2's place",
			next: plib([]string{"A", ""}, []string{"", ""}),
			src:  pl(map[int]string{1: "A", 2: "B", 3: "C"}, nil),
			disp: []validate.Disposition{{Library: 10, Barcode: "B", To: validate.ToShelf}, {Library: 10, Barcode: "C", To: validate.ToSlot, Slot: 2}},
			want: "S1=A S2=C"},
		{name: "export to the first free MAP slot",
			next: plib([]string{"A", "B"}, []string{"X", ""}),
			src:  pl(map[int]string{1: "A", 2: "B", 3: "C"}, map[int]string{1: "X"}),
			disp: []validate.Disposition{{Library: 10, Barcode: "C", To: validate.ToExport}},
			want: "S1=A S2=B M1=X M2=C"},
		{name: "no disposition",
			next: plib([]string{"A", "B"}, []string{"", ""}),
			src:  pl(map[int]string{1: "A", 2: "B", 3: "C"}, nil),
			err:  "C would leave"},
		{name: "forced drive to its source slot",
			next:   plib([]string{"A", "", "C", ""}, []string{"", ""}),
			src:    pl(map[int]string{1: "A", 3: "C"}, nil),
			forced: []changer.DriveState{{Number: 1, Full: true, Barcode: "B", SourceSlot: 2}},
			want:   "S1=A S2=B S3=C"},
		{name: "source occupied, first empty slot",
			next:   plib([]string{"A", "D", "C", ""}, []string{"", ""}),
			src:    pl(map[int]string{1: "A", 3: "C"}, nil),
			forced: []changer.DriveState{{Number: 1, Full: true, Barcode: "B", SourceSlot: 2}},
			want:   "S1=A S2=D S3=C S4=B"},
		{name: "no free slot",
			next:   plib([]string{"A", "D", "C"}, []string{"", ""}),
			src:    pl(map[int]string{1: "A", 3: "C"}, nil),
			forced: []changer.DriveState{{Number: 1, Full: true, Barcode: "B", SourceSlot: 2}},
			err:    "no free storage slot for B"},
		{name: "a trusted .persist already has it back in its slot",
			next:   plib([]string{"A", "B", "C", ""}, []string{"", ""}),
			src:    pl(map[int]string{1: "A", 2: "B", 3: "C"}, nil),
			forced: []changer.DriveState{{Number: 1, Full: true, Barcode: "B", SourceSlot: 2}},
			want:   "S1=A S2=B S3=C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeLibrary(cur, tc.next, tc.src, tc.disp, tc.forced, noBay)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || shortP(got) != tc.want {
				t.Errorf("got %q, %v; want %q", shortP(got), err, tc.want)
			}
		})
	}
}

// TestRenderContents: Slot and MAP lines take the placement, Drive lines
// keep their serials and never get a cartridge, unchanged lines stay byte
// for byte, and a gap filled gets a line in order.
func TestRenderContents(t *testing.T) {
	in := "VERSION: 2\n\nDrive 1: XYZZY_A1\nDrive 2:\n\nMAP 1:\n\nSlot 1: A\nSlot 2: \nSlot 4: C\n"
	out, err := renderContents([]byte(in), pl(map[int]string{2: "B", 3: "D", 4: "C"}, map[int]string{1: "A"}))
	if err != nil {
		t.Fatal(err)
	}
	want := "VERSION: 2\n\nDrive 1: XYZZY_A1\nDrive 2:\n\nMAP 1: A\n\nSlot 1:\nSlot 2: B\nSlot 3: D\nSlot 4: C\n"
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestMapBack covers the mapping back to the original layout (plan 3.2):
// a slot the original has keeps its number, one only the new layout has
// goes to the first empty original slot, none free leaves it unplaced; a
// MAP slot keeps its number; extra cartridges come after the primary; bay
// cartridges stay out of the MAP.
func TestMapBack(t *testing.T) {
	orig := plib([]string{"", "", ""}, []string{""})
	bay := func(b string) bool { return b == "R" }
	got, unplaced := mapBack(orig,
		pl(map[int]string{1: "A", 5: "E"}, map[int]string{1: "M", 2: "N"}),
		[]validate.Placement{pl(map[int]string{1: "X", 3: "A"}, map[int]string{1: "R"})}, bay)
	if shortP(got) != "S1=A S2=E S3=N M1=M" || strings.Join(unplaced, ",") != "X" {
		t.Errorf("got %q, unplaced %v", shortP(got), unplaced)
	}
}

// TestPersistDiffers is the .persist vs R1 comparison of Q7: a cartridge
// R1 shows in a drive may be back in its slot in .persist.
func TestPersistDiffers(t *testing.T) {
	r1 := pl(map[int]string{1: "A"}, map[int]string{1: "M"})
	r1.Drives[1] = "B"
	if d := persistDiffers(pl(map[int]string{1: "A", 2: "B"}, map[int]string{1: "M"}), r1); len(d) != 0 {
		t.Errorf("drive cartridge back in its slot: %v", d)
	}
	if d := persistDiffers(pl(map[int]string{2: "A"}, map[int]string{1: "M"}), r1); len(d) != 2 {
		t.Errorf("moved: %v", d)
	}
}
