package drivestats

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
	"mhvtld/internal/scsi"
)

// param builds one log parameter with control byte 0x40.
func param(code uint16, value []byte) []byte {
	return append([]byte{byte(code >> 8), byte(code), 0x40, byte(len(value))}, value...)
}

func be(n int, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b[8-n:]
}

// page builds a log page from parameters.
func page(pg uint8, params ...[]byte) []byte {
	var body []byte
	for _, p := range params {
		body = append(body, p...)
	}
	return append([]byte{pg, 0, byte(len(body) >> 8), byte(len(body))}, body...)
}

func supported(pages ...uint8) []byte {
	return append([]byte{0, 0, 0, byte(len(pages))}, pages...)
}

// drive is a fake tape drive serving canned log pages.
type drive struct {
	pages map[uint8][]byte
	// fail maps a page to the error LOG SENSE of it returns.
	fail map[uint8]error
	// ua is how many UNIT ATTENTIONs to report before answering.
	ua int
}

func (d *drive) device() *scsi.FakeDevice {
	return &scsi.FakeDevice{Respond: func(cdb, buf []byte) (int, error) {
		if cdb[0] != 0x4d {
			return 0, scsi.DecodeSense(scsi.FixedSense(scsi.KeyIllegalRequest, 0x20, 0))
		}
		if d.ua > 0 {
			d.ua--
			return 0, scsi.DecodeSense(scsi.FixedSense(scsi.KeyUnitAttention, 0x28, 0))
		}
		pg := cdb[2] & 0x3f
		if err := d.fail[pg]; err != nil {
			return 0, err
		}
		b, ok := d.pages[pg]
		if !ok {
			return 0, scsi.DecodeSense(scsi.FixedSense(scsi.KeyIllegalRequest, 0x24, 0))
		}
		return copy(buf, b), nil
	}}
}

func illegal() error {
	return scsi.DecodeSense(scsi.FixedSense(scsi.KeyIllegalRequest, 0x24, 0))
}

// mhvtlDrive returns page sets laid out as mhVTL builds them.
func mhvtlDrive() *drive {
	return &drive{pages: map[uint8][]byte{
		0x00: supported(0x00, 0x02, 0x03, 0x0c, 0x0d, 0x14, 0x17, 0x2e, 0x30),
		0x02: page(0x02,
			param(0x0000, be(4, 1)), param(0x0001, be(4, 2)), param(0x0002, be(4, 3)),
			param(0x0003, be(4, 4)), param(0x0004, be(4, 5)), param(0x0005, be(8, 1<<40)),
			param(0x0006, be(4, 7)), param(0x8000, be(4, 99))),
		0x03: page(0x03, param(0x0005, be(8, 4096)), param(0x0006, be(4, 0))),
		0x0c: page(0x0c,
			param(0x0000, be(8, 3000)), param(0x0001, be(8, 1000)),
			param(0x0002, be(8, 500)), param(0x0003, be(8, 1000)),
			param(0x0004, be(4, 1430511)), param(0x0005, be(4, 1400000)),
			param(0x0006, be(4, 30511)), param(0x0007, be(4, 42)),
			param(0x0008, be(4, 0)), param(0x0100, be(8, 0))),
		0x0d: page(0x0d, param(0x0000, []byte{0, 38})),
		0x14: page(0x14,
			param(0x0000, be(4, 12)), param(0x0001, be(4, 1)), param(0x0002, be(4, 900)),
			param(0x0003, be(4, 80)), param(0x0004, be(4, 123456)),
			param(0x0040, make([]byte, 12))),
		0x17: page(0x17,
			param(0x0000, []byte{1}), param(0x0001, be(4, 9)), param(0x0002, be(8, 77)),
			param(0x0007, be(8, 5)), param(0x000e, be(4, 2000)), param(0x000f, be(4, 1000)),
			param(0x0010, be(8, 50000)), param(0x0011, be(8, 40000)),
			param(0x0012, be(2, 0)), param(0x0013, be(2, 0)),
			param(0x0016, be(4, 1430511)), param(0x0017, be(4, 2861)),
			param(0x0040, append([]byte("SN123"), make([]byte, 27)...)),
			param(0x0042, append([]byte("VOL001L8"), make([]byte, 24)...)),
			param(0x0080, []byte{0}), param(0x0081, []byte{1})),
	}}
}

func reader(dev *scsi.FakeDevice) *Reader {
	return &Reader{
		Overrides: map[int]string{11: "/dev/sg5"},
		Open: func(path string) (changer.Device, error) {
			if path != "/dev/sg5" {
				return nil, os.ErrNotExist
			}
			return dev, nil
		},
	}
}

func eq(t *testing.T, name string, got *uint64, want uint64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", name, got, want)
	}
}

func TestReadFull(t *testing.T) {
	dev := mhvtlDrive().device()
	st, err := reader(dev).Read(config.Drive{ID: 11, Serial: "XYZZY_A1"})
	if err != nil {
		t.Fatal(err)
	}
	if !dev.Closed {
		t.Error("device not closed")
	}
	if st.Device != "/dev/sg5" {
		t.Errorf("device = %q", st.Device)
	}
	if got := strings.Join(st.SupportedPages, ","); got != "0x00,0x02,0x03,0x0c,0x0d,0x14,0x17,0x2e,0x30" {
		t.Errorf("supported = %s", got)
	}
	if len(st.PageErrors) != 0 {
		t.Errorf("page errors: %v", st.PageErrors)
	}

	w := st.WriteErrors
	eq(t, "write corrected w/o delay", w.CorrectedWithoutDelay, 1)
	eq(t, "write corrected with delay", w.CorrectedWithDelay, 2)
	eq(t, "write retries", w.Retries, 3)
	eq(t, "write corrected", w.Corrected, 4)
	eq(t, "write algorithm", w.AlgorithmInvocations, 5)
	eq(t, "write bytes", w.BytesProcessed, 1<<40)
	eq(t, "write uncorrected", w.Uncorrected, 7)
	eq(t, "read bytes", st.ReadErrors.BytesProcessed, 4096)
	if st.ReadErrors.Retries != nil {
		t.Error("absent read retries reported")
	}

	s := st.Sequential
	eq(t, "host written", s.HostBytesWritten, 3000)
	eq(t, "medium written", s.MediumBytesWritten, 1000)
	eq(t, "medium read", s.MediumBytesRead, 500)
	eq(t, "host read", s.HostBytesRead, 1000)
	eq(t, "capacity", s.CapacityMiB, 1430511)
	eq(t, "to EW", s.CapacityToEWMiB, 1400000)
	eq(t, "EW to LEOP", s.EWToLEOPMiB, 30511)
	eq(t, "position", s.PositionMiB, 42)
	if s.WriteCompressionRatio == nil || *s.WriteCompressionRatio != 3 {
		t.Errorf("write ratio = %v, want 3", s.WriteCompressionRatio)
	}
	if s.ReadCompressionRatio == nil || *s.ReadCompressionRatio != 2 {
		t.Errorf("read ratio = %v, want 2", s.ReadCompressionRatio)
	}

	if st.TemperatureC == nil || *st.TemperatureC != 38 {
		t.Errorf("temperature = %v, want 38", st.TemperatureC)
	}

	ds := st.Device14
	eq(t, "loads", ds.VolumeLoads, 12)
	eq(t, "cleanings", ds.CleaningOperations, 1)
	eq(t, "power-on hours", ds.PowerOnHours, 900)
	eq(t, "motion hours", ds.MediumMotionHours, 80)
	eq(t, "metres", ds.MetresProcessed, 123456)

	v := st.Volume
	eq(t, "mounts", v.Mounts, 9)
	eq(t, "datasets written", v.DatasetsWritten, 77)
	eq(t, "datasets read", v.DatasetsRead, 5)
	eq(t, "last MB written", v.LastMountMBWritten, 2000)
	eq(t, "last MB read", v.LastMountMBRead, 1000)
	eq(t, "lifetime MB written", v.LifetimeMBWritten, 50000)
	eq(t, "lifetime MB read", v.LifetimeMBRead, 40000)
	eq(t, "native capacity", v.NativeCapacity, 1430511)
	eq(t, "used native capacity", v.UsedNativeCapacity, 2861)
	if v.LastLoadWriteCompression != nil || v.LastLoadReadCompression != nil {
		t.Error("zero last-load compression ratios not omitted")
	}
	if v.Barcode != "VOL001L8" || v.SerialNumber != "SN123" {
		t.Errorf("barcode %q serial %q", v.Barcode, v.SerialNumber)
	}
	if v.WriteProtect == nil || *v.WriteProtect || v.WORM == nil || !*v.WORM {
		t.Errorf("write protect %v WORM %v", v.WriteProtect, v.WORM)
	}

	js, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"temperature_c":38`, `"device_statistics":`, `"barcode":"VOL001L8"`, `"write_compression_ratio":3`} {
		if !strings.Contains(string(js), key) {
			t.Errorf("JSON lacks %s: %s", key, js)
		}
	}
	if strings.Contains(string(js), "page_errors") || strings.Contains(string(js), "last_load_write") {
		t.Errorf("JSON carries omitted fields: %s", js)
	}
}

// TestNeverReadsTapeAlert pins that page 2Eh is never requested even when
// the drive lists it: reading it clears the flags.
func TestNeverReadsTapeAlert(t *testing.T) {
	d := mhvtlDrive()
	d.pages[0x2e] = page(0x2e, param(0x0001, []byte{1}))
	dev := d.device()
	if _, err := reader(dev).Read(config.Drive{ID: 11}); err != nil {
		t.Fatal(err)
	}
	cdbs := dev.Recorded()
	if len(cdbs) != 7 {
		t.Errorf("%d commands, want 7 (page 0 + six pages)", len(cdbs))
	}
	for _, cdb := range cdbs {
		if cdb[2]&0x3f == 0x2e {
			t.Errorf("TapeAlert page requested: % x", cdb)
		}
		if cdb[0] != 0x4d || cdb[2]&0xc0 != 0x40 {
			t.Errorf("not LOG SENSE of cumulative values: % x", cdb)
		}
	}
}

func TestUnsupportedPagesOmitted(t *testing.T) {
	d := mhvtlDrive()
	d.pages[0x00] = supported(0x00, 0x0c)
	dev := d.device()
	st, err := reader(dev).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if st.Sequential == nil {
		t.Fatal("sequential page missing")
	}
	if st.Volume != nil || st.WriteErrors != nil || st.ReadErrors != nil || st.TemperatureC != nil || st.Device14 != nil {
		t.Errorf("unsupported pages decoded: %+v", st)
	}
	if n := len(dev.Recorded()); n != 2 {
		t.Errorf("%d commands, want 2", n)
	}
	js, _ := json.Marshal(st)
	for _, key := range []string{"volume", "write_errors", "read_errors", "temperature_c", "device_statistics"} {
		if strings.Contains(string(js), `"`+key+`"`) {
			t.Errorf("JSON carries %s: %s", key, js)
		}
	}
}

func TestPageErrorRecorded(t *testing.T) {
	d := mhvtlDrive()
	d.fail = map[uint8]error{0x14: illegal(), 0x0d: errors.New("boom")}
	st, err := reader(d.device()).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if st.Device14 != nil || st.TemperatureC != nil {
		t.Error("failed pages decoded")
	}
	if !strings.Contains(st.PageErrors["0x14"], "ILLEGAL REQUEST") || st.PageErrors["0x0d"] != "boom" {
		t.Errorf("page errors = %v", st.PageErrors)
	}
	if st.Sequential == nil || st.Volume == nil {
		t.Error("other pages lost")
	}
}

func TestReservationConflict(t *testing.T) {
	d := mhvtlDrive()
	d.fail = map[uint8]error{0x00: &scsi.StatusError{Status: 0x18}}
	_, err := reader(d.device()).Read(config.Drive{ID: 11})
	if !errors.Is(err, ErrReserved) {
		t.Fatalf("err = %v, want ErrReserved", err)
	}
	if !strings.Contains(err.Error(), "reserved by another initiator") {
		t.Errorf("message = %q", err)
	}

	// BUSY is not a reservation.
	d.fail = map[uint8]error{0x00: &scsi.StatusError{Status: 0x08}}
	if _, err := reader(d.device()).Read(config.Drive{ID: 11}); err == nil || errors.Is(err, ErrReserved) {
		t.Errorf("BUSY: err = %v", err)
	}
}

func TestUnitAttentionRetried(t *testing.T) {
	d := mhvtlDrive()
	d.ua = 2
	dev := d.device()
	st, err := reader(dev).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.SupportedPages) == 0 {
		t.Error("no supported pages after UNIT ATTENTION")
	}
	if n := len(dev.Recorded()); n != 9 {
		t.Errorf("%d commands, want 9 (two retries of page 0)", n)
	}

	d = mhvtlDrive()
	d.ua = 3 // more than the retry budget
	if _, err := reader(d.device()).Read(config.Drive{ID: 11}); err == nil {
		t.Error("persistent UNIT ATTENTION succeeded")
	}
}

func TestUnknownCapacitiesOmitted(t *testing.T) {
	d := mhvtlDrive()
	d.pages[0x0c] = page(0x0c,
		param(0x0000, be(8, 0)), param(0x0001, be(8, 0)),
		param(0x0002, be(8, 0)), param(0x0003, be(8, 0)),
		param(0x0004, be(4, 0xffffffff)), param(0x0005, be(4, 0xffffffff)),
		param(0x0006, be(4, 0xffffffff)), param(0x0007, be(4, 0xffffffff)),
		param(0x0100, be(8, 1<<19)))
	d.pages[0x17] = page(0x17,
		param(0x0016, be(4, 0xfffffffe)), param(0x0017, be(4, 0xfffffffe)),
		param(0x0042, make([]byte, 32)))
	d.pages[0x0d] = page(0x0d, param(0x0000, []byte{0, 0xff}))
	st, err := reader(d.device()).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sequential
	if s.CapacityMiB != nil || s.CapacityToEWMiB != nil || s.EWToLEOPMiB != nil || s.PositionMiB != nil {
		t.Errorf("unknown capacities reported: %+v", s)
	}
	if s.WriteCompressionRatio != nil || s.ReadCompressionRatio != nil {
		t.Error("ratio reported with zero medium bytes")
	}
	if st.Volume.NativeCapacity != nil || st.Volume.UsedNativeCapacity != nil || st.Volume.Barcode != "" {
		t.Errorf("unknown volume values reported: %+v", st.Volume)
	}
	if st.TemperatureC != nil {
		t.Error("unknown temperature reported")
	}
}

// TestTruncatedPageTailIgnored covers a page length larger than what was
// transferred (mhVTL's Volume Statistics page).
func TestTruncatedPageTailIgnored(t *testing.T) {
	d := mhvtlDrive()
	b := page(0x17, param(0x0001, be(4, 3)), param(0x0042, []byte("VOL002L8")))
	binary.BigEndian.PutUint16(b[2:4], 400)
	d.pages[0x17] = b
	st, err := reader(d.device()).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if st.PageErrors["0x17"] != "" {
		t.Errorf("page error: %s", st.PageErrors["0x17"])
	}
	eq(t, "mounts", st.Volume.Mounts, 3)
	if st.Volume.Barcode != "VOL002L8" {
		t.Errorf("barcode = %q", st.Volume.Barcode)
	}
}

func TestOverrideSkipsLocator(t *testing.T) {
	r := reader(mhvtlDrive().device())
	// A locator over an empty sysfs would fail; the override must win.
	r.Locator = &changer.Locator{SysRoot: t.TempDir(), DevRoot: "/dev"}
	if _, err := r.Read(config.Drive{ID: 11, Serial: "XYZZY_A1"}); err != nil {
		t.Fatalf("override: %v", err)
	}
}

func TestLocatorUsed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "class", "scsi_generic", "sg5", "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serial := "XYZZY_A1"
	if err := os.WriteFile(filepath.Join(dir, "vpd_pg80"), append([]byte{1, 0x80, 0, byte(len(serial))}, serial...), 0o644); err != nil {
		t.Fatal(err)
	}
	r := reader(mhvtlDrive().device())
	r.Overrides = nil
	r.Locator = &changer.Locator{SysRoot: root, DevRoot: "/dev"}
	st, err := r.Read(config.Drive{ID: 11, Serial: serial})
	if err != nil || st.Device != "/dev/sg5" {
		t.Fatalf("located = %q, %v", st.Device, err)
	}

	_, err = r.Read(config.Drive{ID: 12, Serial: "NOPE"})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, changer.ErrUnavailable) {
		t.Errorf("unknown serial: err = %v, want ErrUnavailable and changer.ErrUnavailable", err)
	}
}

func TestOpenFailureUnavailable(t *testing.T) {
	r := reader(nil)
	r.Overrides = map[int]string{11: "/dev/sg9"}
	if _, err := r.Read(config.Drive{ID: 11}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if _, err := (&Reader{}).Read(config.Drive{ID: 11}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no locator: err = %v, want ErrUnavailable", err)
	}
}

// mhVTL never sets the temperature and reports 0, which is not shown as 0 °C.
func TestZeroTemperatureUnknown(t *testing.T) {
	d := mhvtlDrive()
	d.pages[0x0d] = page(0x0d, param(0x0000, []byte{0, 0}))
	st, err := reader(d.device()).Read(config.Drive{ID: 11})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if st.TemperatureC != nil {
		t.Errorf("temperature = %d, want unknown", *st.TemperatureC)
	}
}

func TestUnitSerial(t *testing.T) {
	cases := []struct {
		d    config.Drive
		want string
	}{
		{config.Drive{Vendor: "IBM", Product: "ULT3580-TD8", Serial: "XYZZY_A1"}, "00XYZZY_A1"},
		{config.Drive{Vendor: "IBM ", Product: "ULTRIUM-HH9", Serial: "ABC"}, "0000000ABC"},
		{config.Drive{Vendor: "IBM", Product: "ULT3580-TD8", Serial: "1234567890"}, "1234567890"},
		{config.Drive{Vendor: "IBM", Product: "03592E07", Serial: "XYZZY_A1"}, "XYZZY_A1"},
		{config.Drive{Vendor: "HP", Product: "Ultrium 6-SCSI", Serial: "XYZZY_A1"}, "XYZZY_A1"},
		{config.Drive{Vendor: "STK", Product: "T10000B", Serial: "XYZZY_B1 "}, "XYZZY_B1"},
	}
	for _, c := range cases {
		if got := unitSerial(c.d); got != c.want {
			t.Errorf("unitSerial(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}
