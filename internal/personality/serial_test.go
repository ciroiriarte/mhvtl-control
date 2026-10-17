package personality

import (
	"bytes"
	"strings"
	"testing"

	"mhvtl-control/internal/scsi"
)

// vm03584 is the VPD page 80h of an IBM 03584 library with serial XYZZY_40,
// read on an mhVTL 852b78a test VM.
var vm03584 = []byte{
	0x08, 0x80, 0x00, 0x16,
	0x58, 0x59, 0x5a, 0x5a, 0x59, 0x5f, 0x34, 0x30, 0x20, 0x20, 0x00, 0x00,
	0x30, 0x34, 0x30, 0x30, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func nuls(n int) string { return strings.Repeat("\x00", n) }

func TestUnitSerialPage(t *testing.T) {
	cases := []struct {
		init, vendor, serial string
		want                 string
	}{
		// vtllibrary's own page: the serial as held, "%-14s".
		{"init_default_smc", "mhVTL", "XYZZY_40", "XYZZY_40      "},
		{"init_spectra_215_smc", "SPECTRA", "XYZZY_40", "XYZZY_40      "},
		{"init_spectra_gator_smc", "SPECTRA", "XYZZY_40", "XYZZY_40      "},
		{"init_spectra_logic_smc", "SPECTRA", "XYZZY_40", "XYZZY_40      "},
		// SCSI_SN_LEN cuts the held serial to 15 characters.
		{"init_default_smc", "mhVTL", "ABCDEFGHIJKLMNOPQ", "ABCDEFGHIJKLMNO"},
		// "%-10.10s", two NULs, the first storage address as "%04x".
		{"init_ibm3584", "IBM", "XYZZY_40", "XYZZY_40  " + nuls(2) + "0400" + nuls(6)},
		{"init_ibm3584", "IBM", "XYZZY_4", "XYZZY_4   " + nuls(2) + "0400" + nuls(6)},
		{"init_ibm3584", "IBM", "XYZZY_400001", "XYZZY_4000" + nuls(2) + "0400" + nuls(6)},
		// "%-12.12s", then "_LL0".
		{"init_ibmts3100", "IBM", "XYZZY_40", "XYZZY_40    _LL0"},
		// "%-10.10s" in an 18-byte page.
		{"init_hp_eml_smc", "HP", "XYZZY_40", "XYZZY_40  " + nuls(8)},
		{"init_hp_msl_smc", "HP", "XYZZY_40", "XYZZY_40  " + nuls(8)},
		{"init_overland_smc", "ACME", "XYZZY_40", "XYZZY_40  " + nuls(8)},
		// The vendor "%-8.8s", then the serial "%-16.16s".
		{"init_scalar_smc", "QUANTUM", "XYZZY_40", "QUANTUM XYZZY_40        "},
		{"init_scalar_smc", "ADIC", "XYZZY_40", "ADIC    XYZZY_40        "},
		// Blank-filled to 11 or 12 bytes, cut there.
		{"init_stkl20", "STK", "XYZZY_40", "XYZZY_40   "},
		{"init_stklxx", "STK", "XYZZY_40", "XYZZY_40   "},
		{"init_stkslxx", "STK", "XYZZY_40", "XYZZY_40    "},
		{"init_stklxx", "STK", "XYZZY_400001", "XYZZY_40000"},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.init] = true
		m, ok := Models[c.init]
		if !ok {
			t.Fatalf("no Models[%q]", c.init)
		}
		if got := m.UnitSerialPage(c.vendor, c.serial); string(got) != c.want {
			t.Errorf("%s %q %q: page %q, want %q", c.init, c.vendor, c.serial, got, c.want)
		}
	}
	for fn := range Models {
		if !covered[fn] {
			t.Errorf("Models[%q] has no VPD 80h case", fn)
		}
	}
}

func TestChangerUnitSerialVM(t *testing.T) {
	page := Models["init_ibm3584"].UnitSerialPage("IBM", "XYZZY_40")
	if !bytes.Equal(page, vm03584[4:]) || int(vm03584[3]) != len(page) {
		t.Errorf("03584 page % x, want % x", page, vm03584[4:])
	}
	got, err := scsi.ParseVPDSerial(vm03584)
	if err != nil {
		t.Fatal(err)
	}
	if want := ChangerUnitSerial("IBM", "03584L32", "XYZZY_40"); got != want {
		t.Errorf("VM reports %q, ChangerUnitSerial %q", got, want)
	}
	// One serial a prefix of another must not match it.
	if ChangerUnitSerial("IBM", "03584L32", "XYZZY_4") == got {
		t.Errorf("XYZZY_4 matches XYZZY_40's page")
	}
}

func TestChangerUnitSerial(t *testing.T) {
	cases := []struct{ vendor, product, serial, want string }{
		{"STK", "L700", "XYZZY_A", "XYZZY_A"},
		{"IBM", "03584L32", "XYZZY_40", "XYZZY_40  \x00\x000400"},
		{"IBM", "3573-TL", "XYZZY_40", "XYZZY_40    _LL0"},
		{"QUANTUM", "Scalar i6000", "XYZZY_40", "QUANTUM XYZZY_40"},
		{"HP", "MSL6480", "XYZZY_40", "XYZZY_40"},
		{"SPECTRA", "PYTHON", "XYZZY_40", "XYZZY_40"},
	}
	for _, c := range cases {
		if got := ChangerUnitSerial(c.vendor, c.product, c.serial); got != c.want {
			t.Errorf("ChangerUnitSerial(%q, %q, %q) = %q, want %q", c.vendor, c.product, c.serial, got, c.want)
		}
	}
}

func TestDriveUnitSerial(t *testing.T) {
	cases := []struct{ vendor, product, serial, want string }{
		{"IBM", "ULT3580-TD8", "XYZZY_A1", "00XYZZY_A1"},
		{"IBM ", "ULTRIUM-HH9", "ABC", "0000000ABC"},
		{"IBM", "ULT3580-TD8", "1234567890", "1234567890"},
		{"IBM", "03592E07", "XYZZY_A1", "XYZZY_A1"},
		{"HP", "Ultrium 6-SCSI", "XYZZY_A1", "XYZZY_A1"},
		{"STK", "T10000B", "XYZZY_B1 ", "XYZZY_B1"},
		{"IBM", "ULT3580-TD8", "", ""},
	}
	for _, c := range cases {
		if got := DriveUnitSerial(c.vendor, c.product, c.serial); got != c.want {
			t.Errorf("DriveUnitSerial(%q, %q, %q) = %q, want %q", c.vendor, c.product, c.serial, got, c.want)
		}
	}
}

// vmULT3580 is the VPD page 80h of an IBM ULT3580-TD8 drive with serial
// XYZZY_41, read on an mhVTL 852b78a test VM.
var vmULT3580 = []byte{0x01, 0x80, 0x00, 0x0a, 0x30, 0x30, 0x58, 0x59, 0x5a, 0x5a, 0x59, 0x5f, 0x34, 0x31}

func TestDriveUnitSerialPage(t *testing.T) {
	const blank = "XYZZY_41  " // "%-10s"
	cases := []struct{ vendor, product, serial, want string }{
		// IBM LTO: shorter than 10, zero-filled on the left.
		{"IBM", "ULT3580-TD1", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD2", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD3", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD4", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD5", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD6", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD7", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD8", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TD8", "XYZZY_A1", "00XYZZY_A1"},
		{"IBM", "ULT3580-TD9", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULT3580-TDA", "XYZZY_41", "00XYZZY_41"},
		{"IBM", "ULTRIUM-HH9", "ABC", "0000000ABC"},
		{"IBM ", "ULTRIUM-TD5", "XYZZY_41", "00XYZZY_41"},
		// 10 or more: as configured.
		{"IBM", "ULT3580-TD8", "1234567890", "1234567890"},
		{"IBM", "ULT3580-TD8", "XYZZY_400001", "XYZZY_400001"},
		// Only vendor IBM pads.
		{"HP", "ULTRIUM-TD5", "XYZZY_41", blank},
		// Every other drive: "%-10s".
		{"HP", "Ultrium 1-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 2-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 3-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 4-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 5-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 6-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 7-SCSI", "XYZZY_41", blank},
		{"HP", "Ultrium 8-SCSI", "XYZZY_41", blank},
		{"SONY", "SDX-300C", "XYZZY_41", blank},
		{"SONY", "SDX-500C", "XYZZY_41", blank},
		{"SONY", "SDX-700C", "XYZZY_41", blank},
		{"SONY", "SDX-900V", "XYZZY_41", blank},
		{"IBM", "03592J1A", "XYZZY_41", blank},
		{"IBM", "03592E05", "XYZZY_41", blank},
		{"IBM", "03592E06", "XYZZY_41", blank},
		{"IBM", "03592E07", "XYZZY_41", blank},
		{"STK", "T10000A", "XYZZY_41", blank},
		{"STK", "T10000B", "XYZZY_41", blank},
		{"STK", "T10000C", "XYZZY_41", blank},
		{"STK", "T9840A", "XYZZY_41", blank},
		{"STK", "T9840B", "XYZZY_41", blank},
		{"STK", "T9840C", "XYZZY_41", blank},
		{"STK", "T9840D", "XYZZY_41", blank},
		{"STK", "T9940A", "XYZZY_41", blank},
		{"STK", "T9940B", "XYZZY_41", blank},
		{"QUANTUM", "DLT7000", "XYZZY_41", blank},
		{"QUANTUM", "DLT8000", "XYZZY_41", blank},
		{"QUANTUM", "SDLT 320", "XYZZY_41", blank},
		{"QUANTUM", "SDLT600", "XYZZY_41", blank},
		{"mhVTL", "VIRTUAL TAPE", "XYZZY_41", blank},
		{"STK", "T10000B", "XYZZY_B1 ", "XYZZY_B1  "},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[DriveFor(c.product).InitFn] = true
		if got := DriveUnitSerialPage(c.vendor, c.product, c.serial); string(got) != c.want {
			t.Errorf("%q %q %q: page %q, want %q", c.vendor, c.product, c.serial, got, c.want)
		}
	}
	for _, m := range DriveModels {
		if !covered[m.InitFn] {
			t.Errorf("drive personality %s has no VPD 80h case", m.InitFn)
		}
	}

	page := DriveUnitSerialPage("IBM", "ULT3580-TD8", "XYZZY_41")
	if !bytes.Equal(page, vmULT3580[4:]) || int(vmULT3580[3]) != len(page) {
		t.Errorf("ULT3580-TD8 page % x, want % x", page, vmULT3580[4:])
	}
	got, err := scsi.ParseVPDSerial(vmULT3580)
	if err != nil {
		t.Fatal(err)
	}
	if want := DriveUnitSerial("IBM", "ULT3580-TD8", "XYZZY_41"); got != want {
		t.Errorf("VM reports %q, DriveUnitSerial %q", got, want)
	}
}
