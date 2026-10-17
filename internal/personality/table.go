// Package personality mirrors how mhVTL picks the emulation of a library and
// a drive from the vendor and product in device.conf: the library dispatch in
// mhvtl usr/cmd/vtllibrary.c (customise_lu and its customise_*_lu helpers),
// the element address windows and identifier layout each usr/pm/*_pm.c
// personality registers, and the media each drive personality loads.
//
// Every entry cites the C line it mirrors. crosscheck_test.go parses the C
// sources ($MHVTL_SRC) and fails on any difference, so an upstream change to
// the dispatch, a window or a media list cannot go unnoticed.
package personality

import "strings"

// Rule is one branch of the library dispatch, flattened: the first rule whose
// prefixes match selects InitFn. A prefix is compared case-insensitively over
// its own length, as strncasecmp(id, "X", len) does; an empty prefix matches
// anything (an else branch).
type Rule struct {
	Vendor        string // vendor_id prefix
	ProductPrefix string // product_id prefix
	InitFn        string
	Source        string // the strncasecmp, or the call of an else branch
}

// Model is what a library personality's init function registers.
type Model struct {
	InitFn string
	Name   string // a short human name for messages

	// First element address of each element type (smc_pm.start_*).
	StartPicker, StartMAP, StartDrive, StartStorage uint16
	WindowSource                                    string // the first start_* the init function uses

	// VPD83 is whether the changer answers INQUIRY VPD page 83h (Device
	// Identification). Linux udev derives ID_SCSI_SERIAL from it.
	VPD83 bool
	// SendsNAA is whether that page carries an NAA designator, from the
	// device.conf NAA or else the serial (mhvtl usr/vtllib.c update_vpd_83,
	// usr/pm/hp_smc_pm.c and usr/pm/overland_pm.c update_eml_vpd_83). The
	// IBM and Scalar personalities build a page 83h without one.
	SendsNAA bool

	// Layout of the drive identifier in READ ELEMENT STATUS with DVCID set
	// (mhvtl usr/smc.c fill_element_descriptor).
	DvcidLen           int
	DvcidSerialOnly    bool
	DvcidNulTerminated bool
	StkDteLayout       bool

	// UnitSerialCall is the call by which the init function replaces
	// vtllibrary's VPD page 80h (Unit Serial Number), "" when it keeps it.
	// UnitSerialPage builds the page from it.
	UnitSerialCall string
}

// Personality is the result of a lookup: the model a library gets and the
// rule that selected it.
type Personality struct {
	Model
	Rule Rule
}

// Rules is customise_lu (vtllibrary.c:1536-1553) and its helpers
// (vtllibrary.c:1490-1534) in evaluation order. An OR of two strncasecmp is
// two rules.
var Rules = []Rule{
	{Vendor: "stk", ProductPrefix: "SL500", InitFn: "init_stkslxx", Source: "usr/cmd/vtllibrary.c:1500"},
	{Vendor: "stk", ProductPrefix: "L20", InitFn: "init_stkl20", Source: "usr/cmd/vtllibrary.c:1502"},
	{Vendor: "stk", ProductPrefix: "L40", InitFn: "init_stkl20", Source: "usr/cmd/vtllibrary.c:1504"},
	{Vendor: "stk", ProductPrefix: "L80", InitFn: "init_stkl20", Source: "usr/cmd/vtllibrary.c:1506"},
	{Vendor: "stk", InitFn: "init_stklxx", Source: "usr/cmd/vtllibrary.c:1509"},
	{Vendor: "IBM", ProductPrefix: "3573-TL", InitFn: "init_ibmts3100", Source: "usr/cmd/vtllibrary.c:1491"},
	{Vendor: "IBM", ProductPrefix: "03584", InitFn: "init_ibm3584", Source: "usr/cmd/vtllibrary.c:1493"},
	{Vendor: "IBM", InitFn: "init_default_smc", Source: "usr/cmd/vtllibrary.c:1496"},
	{Vendor: "HP", ProductPrefix: "MSL", InitFn: "init_hp_msl_smc", Source: "usr/cmd/vtllibrary.c:1518"},
	{Vendor: "HP", ProductPrefix: "1x8", InitFn: "init_hp_msl_smc", Source: "usr/cmd/vtllibrary.c:1519"},
	{Vendor: "HP", InitFn: "init_hp_eml_smc", Source: "usr/cmd/vtllibrary.c:1522"},
	{ProductPrefix: "OVERLAND", InitFn: "init_overland_smc", Source: "usr/cmd/vtllibrary.c:1543"},
	{Vendor: "ADIC", InitFn: "init_scalar_smc", Source: "usr/cmd/vtllibrary.c:1545"},
	{Vendor: "QUANTUM", InitFn: "init_scalar_smc", Source: "usr/cmd/vtllibrary.c:1546"},
	{ProductPrefix: "Scalar", InitFn: "init_scalar_smc", Source: "usr/cmd/vtllibrary.c:1547"},
	// strncasecmp(vendor_id, "SPECTRA ", 7) compares "SPECTRA" only.
	{Vendor: "SPECTRA", ProductPrefix: "PYTHON", InitFn: "init_spectra_logic_smc", Source: "usr/cmd/vtllibrary.c:1526"},
	{Vendor: "SPECTRA", ProductPrefix: "GATOR", InitFn: "init_spectra_gator_smc", Source: "usr/cmd/vtllibrary.c:1528"},
	{Vendor: "SPECTRA", ProductPrefix: "215", InitFn: "init_spectra_215_smc", Source: "usr/cmd/vtllibrary.c:1530"},
	{Vendor: "SPECTRA", InitFn: "init_spectra_logic_smc", Source: "usr/cmd/vtllibrary.c:1533"},
	{InitFn: "init_default_smc", Source: "usr/cmd/vtllibrary.c:1552"},
}

// Models holds the library personalities by init function. Values are the
// last assignment in the init function, else the static smc_pm initializer of
// its file: one vtllibrary process runs one personality.
var Models = map[string]Model{
	"init_default_smc": {
		InitFn: "init_default_smc", Name: "mhVTL default library",
		StartPicker: 0x02f0, StartMAP: 0x0300, StartDrive: 0x0001, StartStorage: 0x0400,
		WindowSource: "usr/pm/default_smc_pm.c:53",
		VPD83:        true, SendsNAA: true,
		DvcidLen: 34,
	},
	"init_hp_eml_smc": {
		InitFn: "init_hp_eml_smc", Name: "HP EML E-Series",
		StartPicker: 0x0001, StartMAP: 0x000a, StartDrive: 0x01f4, StartStorage: 0x03e8,
		WindowSource: "usr/pm/hp_smc_pm.c:128",
		VPD83:        true, SendsNAA: true,
		DvcidLen:       34,
		UnitSerialCall: "update_eml_vpd_80(lu)",
	},
	// init_hp_msl_smc sets a 34-byte identifier for the models HPE documents
	// (MSL6480, MSL3040, MSL2024 G4, 1/8 G3; usr/pm/hp_smc_pm.c
	// hp_msl_documented :156-161) and a 20-byte serial only identifier for
	// the others; the table keeps the second, the last assignment. So
	// SerialArea gives 20 for every MSL, where the documented models show 10
	// characters of a drive serial (34 less the 8-byte vendor and 16-byte
	// product). No warning is lost: device.conf serials are at most 10
	// characters (MaxSerialLen in package validate).
	"init_hp_msl_smc": {
		InitFn: "init_hp_msl_smc", Name: "HP MSL",
		StartPicker: 0x0001, StartMAP: 0x01c0, StartDrive: 0x01e0, StartStorage: 0x0020,
		WindowSource: "usr/pm/hp_smc_pm.c:169",
		VPD83:        true, SendsNAA: true,
		DvcidLen: 20, DvcidSerialOnly: true,
		UnitSerialCall: "update_eml_vpd_80(lu)",
	},
	"init_ibmts3100": {
		InitFn: "init_ibmts3100", Name: "IBM TS3100",
		StartPicker: 0x0001, StartMAP: 0x0010, StartDrive: 0x0100, StartStorage: 0x1000,
		WindowSource:   "usr/pm/ibm_smc_pm.c:343",
		VPD83:          true,
		DvcidLen:       34,
		UnitSerialCall: "update_ibm_3100_vpd_80(lu)",
	},
	// markh794/mhvtl#287 moved the 03584 MAP from 0x0300 to 0x0301, so the
	// window allows 512 drives. With an mhVTL before #287, drive 512 sits
	// on MAP 0x0300, and the limit there is 511.
	"init_ibm3584": {
		InitFn: "init_ibm3584", Name: "IBM 03584",
		StartPicker: 0x0001, StartMAP: 0x0301, StartDrive: 0x0101, StartStorage: 0x0400,
		WindowSource:   "usr/pm/ibm_smc_pm.c:438",
		VPD83:          true,
		DvcidLen:       34,
		UnitSerialCall: "update_ibm_3584_vpd_80(lu)",
	},
	"init_overland_smc": {
		InitFn: "init_overland_smc", Name: "Overland",
		StartPicker: 0x0001, StartMAP: 0x0000, StartDrive: 0x00ff, StartStorage: 0x0002,
		WindowSource: "usr/pm/overland_pm.c:106",
		VPD83:        true, SendsNAA: true,
		DvcidLen:       34,
		UnitSerialCall: "update_eml_vpd_80(lu)",
	},
	"init_scalar_smc": {
		InitFn: "init_scalar_smc", Name: "Quantum Scalar",
		StartPicker: 0x0001, StartMAP: 0x0010, StartDrive: 0x0100, StartStorage: 0x1000,
		WindowSource:   "usr/pm/scalar_pm.c:113",
		VPD83:          true,
		DvcidLen:       64,
		UnitSerialCall: "update_scalar_vpd_80(lu)",
	},
	"init_spectra_215_smc": {
		InitFn: "init_spectra_215_smc", Name: "Spectra Treefrog (215)",
		StartPicker: 86, StartMAP: 99, StartDrive: 31, StartStorage: 1,
		WindowSource: "usr/pm/spectra_pm.c:87",
		DvcidLen:     0, DvcidSerialOnly: true,
	},
	"init_spectra_gator_smc": {
		InitFn: "init_spectra_gator_smc", Name: "Spectra Gator",
		StartPicker: 0x02c3, StartMAP: 0x0001, StartDrive: 0x02a3, StartStorage: 0x001e,
		WindowSource: "usr/pm/spectra_pm.c:113",
		DvcidLen:     10, DvcidSerialOnly: true,
	},
	"init_spectra_logic_smc": {
		InitFn: "init_spectra_logic_smc", Name: "Spectra T-Series",
		StartPicker: 0x0001, StartMAP: 0x0010, StartDrive: 0x0100, StartStorage: 0x1000,
		WindowSource: "usr/pm/spectra_pm.c:142",
		DvcidLen:     32, DvcidSerialOnly: true, DvcidNulTerminated: true,
	},
	"init_stkl20": {
		InitFn: "init_stkl20", Name: "STK L20/L40/L80",
		StartPicker: 0x0001, StartMAP: 0x000a, StartDrive: 0x01f4, StartStorage: 0x03e8,
		WindowSource: "usr/pm/stklxx_pm.c:79",
		DvcidLen:     32, StkDteLayout: true,
		UnitSerialCall: "update_stk_l_vpd_80(lu, 11)",
	},
	"init_stklxx": {
		InitFn: "init_stklxx", Name: "STK L-series (L180/L700)",
		StartPicker: 0x0001, StartMAP: 0x000a, StartDrive: 0x01f4, StartStorage: 0x03e8,
		WindowSource: "usr/pm/stklxx_pm.c:109",
		DvcidLen:     32, StkDteLayout: true,
		UnitSerialCall: "update_stk_l_vpd_80(lu, 11)",
	},
	"init_stkslxx": {
		InitFn: "init_stkslxx", Name: "STK SL500",
		StartPicker: 0x0001, StartMAP: 0x000a, StartDrive: 0x01f4, StartStorage: 0x03e8,
		WindowSource: "usr/pm/stklxx_pm.c:135",
		DvcidLen:     32, StkDteLayout: true,
		UnitSerialCall: "update_stk_l_vpd_80(lu, 12)",
	},
}

// Lookup returns the personality vtllibrary gives a library with this vendor
// and product identification.
func Lookup(vendor, product string) Personality {
	for _, r := range Rules {
		if hasPrefixFold(vendor, r.Vendor) && hasPrefixFold(product, r.ProductPrefix) {
			return Personality{Model: Models[r.InitFn], Rule: r}
		}
	}
	// Unreachable: the last rule matches anything.
	return Personality{}
}

// hasPrefixFold is strncasecmp(s, prefix, len(prefix)) == 0.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// SerialArea is how many characters of a drive's serial number the library
// shows in its READ ELEMENT STATUS drive identifier (mhvtl usr/smc.c
// fill_element_descriptor): the whole area in the STK layout and the serial
// only layout (less the NUL when terminated), else what the area leaves after
// the 8-byte vendor and 16-byte product.
func (m Model) SerialArea() int {
	n := m.DvcidLen
	switch {
	case m.StkDteLayout:
	case m.DvcidSerialOnly:
		if m.DvcidNulTerminated {
			n--
		}
	default:
		n -= 24
	}
	return max(n, 0)
}

// Limits is how many elements of each type a library personality holds.
type Limits struct {
	Pickers int `json:"pickers"`
	MAPs    int `json:"maps"`
	Drives  int `json:"drives"`
	Slots   int `json:"slots"`
}

// Limits returns the most elements of each type the model's windows hold.
// Element N of a type sits at start+N-1, so with the next higher start at
// next the largest N is next-start, and with none, 0x10000-start: past the
// next start vtllibrary drops the element (check_overflow, mhvtl
// usr/cmd/vtllibrary.c:761-849, upstream master 852b78a), and package
// validate refuses it, as it does past the 16-bit address space.
func (m Model) Limits() Limits {
	starts := []uint16{m.StartPicker, m.StartMAP, m.StartDrive, m.StartStorage}
	limit := func(start uint16) int {
		n := 0x10000 - int(start)
		for _, o := range starts {
			if o > start && int(o-start) < n {
				n = int(o - start)
			}
		}
		return n
	}
	return Limits{
		Pickers: limit(m.StartPicker),
		MAPs:    limit(m.StartMAP),
		Drives:  limit(m.StartDrive),
		Slots:   limit(m.StartStorage),
	}
}
