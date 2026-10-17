package main

import (
	"bytes"
	"strings"
	"testing"
)

// mtxStatus is mtx 1.3.12 "status" output for a library with two drives,
// five slots and two MAP slots, one drive loaded from slot 3, one cartridge
// exported to the second MAP slot, the slots listed before the MAP ones.
const mtxStatus = `  Storage Changer /dev/sg9:2 Drives, 7 Slots ( 2 Import/Export )
Data Transfer Element 0:Full (Storage Element 3 Loaded):VolumeTag = F40003L8
Data Transfer Element 1:Empty
      Storage Element 1:Full :VolumeTag=F40001L8
      Storage Element 2:Empty
      Storage Element 3:Empty
      Storage Element 4:Full :VolumeTag=F40002L8
      Storage Element 5:Empty
      Storage Element 6 IMPORT/EXPORT:Empty
      Storage Element 7 IMPORT/EXPORT:Full :VolumeTag=F40005L8
`

func TestParseMtx(t *testing.T) {
	p, err := parseMtx(strings.NewReader(mtxStatus))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"F40001L8", "", "", "F40002L8", ""}; strings.Join(p.Slots, ",") != strings.Join(want, ",") {
		t.Fatalf("slots %q, want %q", p.Slots, want)
	}
	if want := []string{"", "F40005L8"}; strings.Join(p.MAPs, ",") != strings.Join(want, ",") {
		t.Fatalf("maps %q, want %q", p.MAPs, want)
	}
	if len(p.Drives) != 2 || p.Drives[0] != (drive{Barcode: "F40003L8", Source: 3}) || p.Drives[1] != (drive{}) {
		t.Fatalf("drives %+v", p.Drives)
	}
}

func TestParseMtxMAPFirstAndUnknownSource(t *testing.T) {
	in := `  Storage Changer /dev/sg2:1 Drives, 3 Slots ( 1 Import/Export )
Data Transfer Element 0:Full (Unknown Storage Element Loaded):VolumeTag = X1
      Storage Element 1 IMPORT/EXPORT:Full :VolumeTag=M1
      Storage Element 2:Empty
      Storage Element 3:Full :VolumeTag=S2
`
	p, err := parseMtx(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Slots, ",") != ",S2" || strings.Join(p.MAPs, ",") != "M1" {
		t.Fatalf("slots %q maps %q", p.Slots, p.MAPs)
	}
	if p.Drives[0] != (drive{Barcode: "X1"}) {
		t.Fatalf("drive %+v", p.Drives[0])
	}
}

func TestParseMtxRejectsOtherOutput(t *testing.T) {
	if _, err := parseMtx(strings.NewReader("mtx: Request Sense: Sense Key=Not Ready\n")); err == nil {
		t.Fatal("parsed output without a Storage Changer line")
	}
}

const baseContents = `VERSION: 2

Drive 1:
Drive 2:

Picker 1:

MAP 1:
MAP 2:

# pre-filled
Slot 1: F40001L8
Slot 2: F40002L8
Slot 3: F40003L8
Slot 4:
`

func TestContentsFromLiveView(t *testing.T) {
	var out bytes.Buffer
	from := write(t, t.TempDir(), "library_contents.40", baseContents)
	mtx := write(t, t.TempDir(), "mtx.txt", mtxStatus)
	if err := run([]string{"contents", "-from", from, "-mtx", mtx, "-slots", "7", "-drives", "3"}, &out); err != nil {
		t.Fatal(err)
	}
	want := `VERSION: 2

Drive 1:
Drive 2:
Drive 3:

Picker 1:

MAP 1:
MAP 2: F40005L8

# pre-filled
Slot 1: F40001L8
Slot 2:
Slot 3:
Slot 4: F40002L8
Slot 5:
Slot 6:
Slot 7:
`
	if out.String() != want {
		t.Fatalf("contents\n%s\nwant\n%s", out.String(), want)
	}
}

func TestContentsRefusesFewerSlots(t *testing.T) {
	var out bytes.Buffer
	from := write(t, t.TempDir(), "library_contents.40", baseContents)
	mtx := write(t, t.TempDir(), "mtx.txt", mtxStatus)
	if err := run([]string{"contents", "-from", from, "-mtx", mtx, "-slots", "3"}, &out); err == nil {
		t.Fatal("-slots below the live slot count was accepted")
	}
}

func TestPlacementOfMtxAndContentsAgree(t *testing.T) {
	var fromMtx, fromFile bytes.Buffer
	mtx := write(t, t.TempDir(), "mtx.txt", mtxStatus)
	if err := run([]string{"placement", "-mtx", mtx}, &fromMtx); err != nil {
		t.Fatal(err)
	}
	want := "Drive 1 F40003L8 from Slot 3\nMAP 2 F40005L8\nSlot 1 F40001L8\nSlot 4 F40002L8\n"
	if fromMtx.String() != want {
		t.Fatalf("mtx placement\n%s\nwant\n%s", fromMtx.String(), want)
	}
	file := write(t, t.TempDir(), "library_contents.40", "VERSION: 2\nDrive 1:\nMAP 2: F40005L8\nSlot 1: F40001L8\nSlot 2:\nSlot 4: F40002L8\n")
	if err := run([]string{"placement", "-contents", file}, &fromFile); err != nil {
		t.Fatal(err)
	}
	if want := "MAP 2 F40005L8\nSlot 1 F40001L8\nSlot 4 F40002L8\n"; fromFile.String() != want {
		t.Fatalf("contents placement\n%s\nwant\n%s", fromFile.String(), want)
	}
}
