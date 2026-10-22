package personality

import "testing"

func TestLookup(t *testing.T) {
	for _, tc := range []struct {
		vendor, product, init string
		vpd83                 bool
	}{
		{"STK", "L700", "init_stklxx", false},
		{"stk", "L80", "init_stkl20", false},
		{"STK", "SL500", "init_stkslxx", false},
		{"IBM", "03584L32", "init_ibm3584", true},
		{"IBM", "3573-TL", "init_ibmts3100", true},
		{"IBM", "3584", "init_default_smc", true}, // the 03584 prefix needs its 0
		{"HP", "1x8 G3 AUTOLDR", "init_hp_msl_smc", true},
		{"HP", "EML E-Series", "init_hp_eml_smc", true},
		{"QUANTUM", "Scalar i6000", "init_scalar_smc", true},
		{"ACME", "Scalar 24", "init_scalar_smc", true},
		{"ACME", "OVERLAND NEO", "init_overland_smc", true},
		{"SPECTRA", "GATOR", "init_spectra_gator_smc", false},
		{"SPECTRA", "T950", "init_spectra_logic_smc", false},
		{"ACME", "VTL", "init_default_smc", true},
		{"", "", "init_default_smc", true},
	} {
		p := Lookup(tc.vendor, tc.product)
		if p.InitFn != tc.init || p.VPD83 != tc.vpd83 || p.Rule.InitFn != tc.init {
			t.Errorf("Lookup(%q, %q) = %s (VPD83 %v); want %s (VPD83 %v)",
				tc.vendor, tc.product, p.InitFn, p.VPD83, tc.init, tc.vpd83)
		}
	}
}

func TestEveryRuleHasAModel(t *testing.T) {
	for _, r := range Rules {
		if _, ok := Models[r.InitFn]; !ok {
			t.Errorf("rule %+v selects %s, which has no Model", r, r.InitFn)
		}
	}
	if last := Rules[len(Rules)-1]; last.Vendor != "" || last.ProductPrefix != "" {
		t.Errorf("last rule %+v does not match everything", last)
	}
}

func TestSerialArea(t *testing.T) {
	for _, tc := range []struct {
		init string
		want int
	}{
		{"init_stklxx", 32},            // STK layout: the whole area
		{"init_ibm3584", 10},           // 34 bytes less vendor and product
		{"init_scalar_smc", 40},        // 64 bytes less vendor and product
		{"init_spectra_logic_smc", 31}, // 32 bytes, NUL terminated
		{"init_spectra_gator_smc", 10},
		{"init_spectra_215_smc", 0},
	} {
		if got := Models[tc.init].SerialArea(); got != tc.want {
			t.Errorf("%s SerialArea = %d; want %d", tc.init, got, tc.want)
		}
	}
}

func TestDriveFor(t *testing.T) {
	for _, tc := range []struct {
		product, init string
	}{
		{"ULT3580-TD8", "init_ult3580_td8"},
		{"ULT3580-TD8     ", "init_ult3580_td8"},
		{"T10000B", "init_t10kB_ssc"},
		{"ULT3580-TD", DefaultDriveInit},  // whole-name match, not a prefix
		{"ult3580-td8", DefaultDriveInit}, // strncmp is case sensitive
		{"NOSUCH", DefaultDriveInit},
	} {
		if got := DriveFor(tc.product).InitFn; got != tc.init {
			t.Errorf("DriveFor(%q) = %s; want %s", tc.product, got, tc.init)
		}
	}
}

func TestReads(t *testing.T) {
	td8 := DriveFor("ULT3580-TD8")
	for name, want := range map[string]bool{"LTO7": true, "LTO8": true, "LTO6": false, "LTO9": false} {
		if got := td8.Reads(name); got != want {
			t.Errorf("TD8 Reads(%s) = %v; want %v", name, got, want)
		}
	}
	if !DriveFor("03592E06").Reads("03592 JC") {
		t.Error("03592E06 does not read 03592 JC")
	}
	// M8 is a media type of its own: the LTO-8 drives load it, the LTO-9
	// drive loads LTO-8 cartridges but not M8 ones (markh794/mhvtl#307).
	for _, tc := range []struct {
		product, name string
		want          bool
	}{
		{"ULT3580-TD8", "LTO8 M8", true},
		{"Ultrium 8-SCSI", "LTO8 M8", true},
		{"ULT3580-TD9", "LTO8", true},
		{"ULT3580-TD9", "LTO8 M8", false},
		{"ULT3580-TD7", "LTO8 M8", false},
	} {
		if got := DriveFor(tc.product).Reads(tc.name); got != tc.want {
			t.Errorf("%s Reads(%s) = %v; want %v", tc.product, tc.name, got, tc.want)
		}
	}
}

func TestBarcodeDensity(t *testing.T) {
	for _, tc := range []struct{ barcode, density, typ string }{
		{"E01001L8", "LTO8", "data"},
		{"E01001LY", "LTO8", "WORM"},
		{"CLN001L6", "LTO6", "clean"},
		{"E01001LA", "LTO10", "data"},
		{"E01001PA", "LTO10P", "data"},
		{"E01001M8", "LTO8M8", "data"},
		{"E01001M7", "UNKNOWN", "data"},
		{"W00001M8", "LTO8M8", "WORM"},
		{"E01001JC", "E06", "data"},
		{"E01001JW", "E05", "WORM"},
		{"E01001TB", "T10KB", "data"},
		{"E01001S3", "SDLT600", "data"},
		{"SHORT", "UNKNOWN", "data"},
		// make_vtl_media's regex is not anchored: the tail decides.
		{"ABCDEFGHIJ01L8", "LTO8", "data"},
		{"ABCDEFGHIJ01LY", "LTO8", "WORM"},
	} {
		if got := BarcodeDensity(tc.barcode); got != tc.density {
			t.Errorf("BarcodeDensity(%s) = %s; want %s", tc.barcode, got, tc.density)
		}
		if got := BarcodeMediaType(tc.barcode); got != tc.typ {
			t.Errorf("BarcodeMediaType(%s) = %s; want %s", tc.barcode, got, tc.typ)
		}
	}
}

// The element windows of fact 11 of the plan, with the 03584 MAP moved to
// 0x0301 by markh794/mhvtl#287: 03584 allows 512 drives and 255 MAP slots;
// the STK L-series 500 MAP slots between 0x000a and 0x01f4.
func TestLimits(t *testing.T) {
	for _, tc := range []struct {
		init string
		want Limits
	}{
		{"init_ibm3584", Limits{Pickers: 0x0100, MAPs: 0x00ff, Drives: 0x0200, Slots: 0xfc00}},
		{"init_stklxx", Limits{Pickers: 0x0009, MAPs: 0x01ea, Drives: 0x01f4, Slots: 0xfc18}},
		{"init_scalar_smc", Limits{Pickers: 0x000f, MAPs: 0x00f0, Drives: 0x0f00, Slots: 0xf000}},
	} {
		if got := Models[tc.init].Limits(); got != tc.want {
			t.Errorf("%s: Limits() = %+v, want %+v", tc.init, got, tc.want)
		}
	}
}
