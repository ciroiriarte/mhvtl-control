// Package drivestats reads tape drive statistics with LOG SENSE: data
// volumes and compression (Sequential-Access Device page 0Ch), cartridge
// statistics (Volume Statistics page 17h), error counters (02h, 03h),
// temperature (0Dh) and lifetime device statistics (14h).
//
// It never reads the TapeAlert page (2Eh): on mhVTL and on real drives a
// LOG SENSE of that page clears the flags, which would steal the alerts from
// the backup application that owns the drive.
package drivestats

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
	"mhvtld/internal/scsi"
)

// logSenseTimeout bounds one LOG SENSE.
const logSenseTimeout = 30 * time.Second

// allocLen is the LOG SENSE allocation length; pages longer than this are
// read up to it and the tail ignored.
const allocLen = 0xffff

// unitAttentionRetries is how many times a command is reissued after UNIT
// ATTENTION, which a drive reports once after a cartridge load or reset.
const unitAttentionRetries = 2

// statusReservationConflict is the RESERVATION CONFLICT SCSI status (SAM).
const statusReservationConflict = 0x18

// Log pages read, in order. TapeAlert (2Eh) is deliberately absent: reading
// it clears the drive's TapeAlert flags (mhVTL ssc_log_sense calls
// set_TapeAlert(TA_NONE); real drives do the same), stealing the alerts from
// the backup application.
const (
	pageWriteErrors      = 0x02
	pageReadErrors       = 0x03
	pageSequential       = 0x0c
	pageTemperature      = 0x0d
	pageDeviceStatistics = 0x14
	pageVolume           = 0x17
)

var readPages = []uint8{pageWriteErrors, pageReadErrors, pageSequential, pageTemperature, pageDeviceStatistics, pageVolume}

var (
	// ErrUnavailable means the drive's sg node could not be found or opened.
	// When discovery fails the error also wraps changer.ErrUnavailable.
	ErrUnavailable = errors.New("drive unavailable")
	// ErrReserved means the drive answered RESERVATION CONFLICT: another
	// initiator, typically the backup application, holds a reservation.
	ErrReserved = errors.New("drive is reserved by another initiator (e.g. the backup application)")
)

// Reader reads statistics from tape drives over LOG SENSE.
type Reader struct {
	// Locator finds the drive's sg node by its unit serial number.
	Locator *changer.Locator
	// Overrides pins drive IDs to explicit sg paths (--drive), checked
	// before discovery.
	Overrides map[int]string
	// Open opens the node (changer.OpenSG in production).
	Open changer.Opener
}

// Stats is the decoded statistics of one drive. Pages the drive does not
// support, and parameters it does not report, are omitted.
type Stats struct {
	Device         string            `json:"device"`
	SupportedPages []string          `json:"supported_pages"`
	Sequential     *Sequential       `json:"sequential,omitempty"`
	Volume         *Volume           `json:"volume,omitempty"`
	WriteErrors    *ErrorCounters    `json:"write_errors,omitempty"`
	ReadErrors     *ErrorCounters    `json:"read_errors,omitempty"`
	TemperatureC   *int              `json:"temperature_c,omitempty"`
	Device14       *DeviceStatistics `json:"device_statistics,omitempty"`
	// PageErrors maps a page ("0x0c") that could not be read to why.
	PageErrors map[string]string `json:"page_errors,omitempty"`
}

// Sequential is the Sequential-Access Device log page (0Ch). Byte counters
// cover the current load; capacities are in MiB and are omitted when no
// cartridge is loaded.
type Sequential struct {
	HostBytesWritten   *uint64 `json:"host_bytes_written,omitempty"`   // 0000h, before compression
	MediumBytesWritten *uint64 `json:"medium_bytes_written,omitempty"` // 0001h, after compression
	MediumBytesRead    *uint64 `json:"medium_bytes_read,omitempty"`    // 0002h
	HostBytesRead      *uint64 `json:"host_bytes_read,omitempty"`      // 0003h
	// WriteCompressionRatio is host/medium bytes written; omitted when
	// nothing reached the medium.
	WriteCompressionRatio *float64 `json:"write_compression_ratio,omitempty"`
	// ReadCompressionRatio is host/medium bytes read.
	ReadCompressionRatio *float64 `json:"read_compression_ratio,omitempty"`
	CapacityMiB          *uint64  `json:"capacity_mib,omitempty"`       // 0004h, BOP to EOD
	CapacityToEWMiB      *uint64  `json:"capacity_to_ew_mib,omitempty"` // 0005h, BOP to early warning
	EWToLEOPMiB          *uint64  `json:"ew_to_leop_mib,omitempty"`     // 0006h, early warning to LEOP
	PositionMiB          *uint64  `json:"position_mib,omitempty"`       // 0007h, BOP to current position
	// Parameter 0100h (cleaning required) is not reported: mhVTL fills it
	// with the whole TapeAlert bitmap, so any alert would read as a cleaning
	// request.
}

// Volume is the Volume Statistics log page (17h) of the loaded cartridge.
// *MB counters are 10^6 bytes (SSC); native capacities are in the drive's
// capacity unit (MiB for LTO).
type Volume struct {
	Mounts             *uint64 `json:"mounts,omitempty"`                // 0001h
	DatasetsWritten    *uint64 `json:"datasets_written,omitempty"`      // 0002h
	DatasetsRead       *uint64 `json:"datasets_read,omitempty"`         // 0007h
	LastMountMBWritten *uint64 `json:"last_mount_mb_written,omitempty"` // 000Eh
	LastMountMBRead    *uint64 `json:"last_mount_mb_read,omitempty"`    // 000Fh
	LifetimeMBWritten  *uint64 `json:"lifetime_mb_written,omitempty"`   // 0010h
	LifetimeMBRead     *uint64 `json:"lifetime_mb_read,omitempty"`      // 0011h
	// LastLoadWriteCompression and LastLoadReadCompression are the raw
	// compression ratio values the drive reports for the last load; zero
	// (mhVTL never sets them) is omitted.
	LastLoadWriteCompression *uint64 `json:"last_load_write_compression,omitempty"` // 0012h
	LastLoadReadCompression  *uint64 `json:"last_load_read_compression,omitempty"`  // 0013h
	NativeCapacity           *uint64 `json:"native_capacity,omitempty"`             // 0016h
	UsedNativeCapacity       *uint64 `json:"used_native_capacity,omitempty"`        // 0017h
	SerialNumber             string  `json:"serial_number,omitempty"`               // 0040h
	Barcode                  string  `json:"barcode,omitempty"`                     // 0042h
	WriteProtect             *bool   `json:"write_protect,omitempty"`               // 0080h
	WORM                     *bool   `json:"worm,omitempty"`                        // 0081h
}

// ErrorCounters is a Write (02h) or Read (03h) Error Counter log page (SPC).
type ErrorCounters struct {
	CorrectedWithoutDelay *uint64 `json:"corrected_without_delay,omitempty"` // 0000h
	CorrectedWithDelay    *uint64 `json:"corrected_with_delay,omitempty"`    // 0001h
	Retries               *uint64 `json:"retries,omitempty"`                 // 0002h, total rewrites/rereads
	Corrected             *uint64 `json:"corrected,omitempty"`               // 0003h
	AlgorithmInvocations  *uint64 `json:"algorithm_invocations,omitempty"`   // 0004h
	BytesProcessed        *uint64 `json:"bytes_processed,omitempty"`         // 0005h
	Uncorrected           *uint64 `json:"uncorrected,omitempty"`             // 0006h
}

// DeviceStatistics is the Device Statistics log page (14h): lifetime counters
// of the drive itself.
type DeviceStatistics struct {
	VolumeLoads        *uint64 `json:"volume_loads,omitempty"`        // 0000h
	CleaningOperations *uint64 `json:"cleaning_operations,omitempty"` // 0001h
	PowerOnHours       *uint64 `json:"power_on_hours,omitempty"`      // 0002h
	MediumMotionHours  *uint64 `json:"medium_motion_hours,omitempty"` // 0003h
	MetresProcessed    *uint64 `json:"metres_processed,omitempty"`    // 0004h
}

// Read opens d's sg node once, reads the Supported Log Pages page and then
// every supported page of the fixed set. A page that fails is recorded in
// Stats.PageErrors; only failure to find or open the node (ErrUnavailable,
// plus changer.ErrUnavailable when discovery failed) or to read page 00h
// fails the read. RESERVATION CONFLICT on page 00h wraps ErrReserved.
func (r *Reader) Read(d config.Drive) (Stats, error) {
	path, err := r.locate(d)
	if err != nil {
		return Stats{}, err
	}
	if r.Open == nil {
		return Stats{}, fmt.Errorf("%w: drive %d: no opener", ErrUnavailable, d.ID)
	}
	dev, err := r.Open(path)
	if err != nil {
		return Stats{}, fmt.Errorf("%w: drive %d: open %s: %w", ErrUnavailable, d.ID, path, err)
	}
	defer dev.Close()

	st := Stats{Device: path}
	b, err := logSense(dev, scsi.LogPageSupported)
	if err != nil {
		return Stats{}, fmt.Errorf("drive %d: supported log pages: %w", d.ID, err)
	}
	supported, err := scsi.ParseSupportedLogPages(b)
	if err != nil {
		return Stats{}, fmt.Errorf("drive %d: supported log pages: %w", d.ID, err)
	}
	have := map[uint8]bool{}
	st.SupportedPages = make([]string, 0, len(supported))
	for _, pg := range supported {
		have[pg] = true
		st.SupportedPages = append(st.SupportedPages, pageName(pg))
	}

	for _, pg := range readPages {
		if !have[pg] {
			continue
		}
		if err := st.readPage(dev, pg); err != nil {
			if st.PageErrors == nil {
				st.PageErrors = map[string]string{}
			}
			st.PageErrors[pageName(pg)] = err.Error()
		}
	}
	return st, nil
}

// locate returns d's sg path: the override, else discovery by serial.
func (r *Reader) locate(d config.Drive) (string, error) {
	if p, ok := r.Overrides[d.ID]; ok {
		return p, nil
	}
	if r.Locator == nil {
		return "", fmt.Errorf("%w: drive %d: no --drive override and no locator", ErrUnavailable, d.ID)
	}
	p, err := r.Locator.FindBySerial(unitSerial(d), scsi.PeripheralSequential)
	if err != nil {
		return "", fmt.Errorf("%w: drive %d: %w", ErrUnavailable, d.ID, err)
	}
	return p, nil
}

// readPage reads and decodes one log page into st.
func (st *Stats) readPage(dev scsi.Device, pg uint8) error {
	b, err := logSense(dev, pg)
	if err != nil {
		return err
	}
	p, err := scsi.ParseLogPage(b)
	// A page whose length claims more than was transferred (mhVTL's Volume
	// Statistics page with fewer partitions than its maximum) keeps the
	// parameters that arrived whole.
	if err != nil && (!errors.Is(err, scsi.ErrShortResponse) || len(b) < 4) {
		return err
	}
	if p.Page != pg {
		return fmt.Errorf("drive returned log page 0x%02x", p.Page)
	}
	switch pg {
	case pageWriteErrors:
		st.WriteErrors = errorCounters(p)
	case pageReadErrors:
		st.ReadErrors = errorCounters(p)
	case pageSequential:
		st.Sequential = sequential(p)
	case pageTemperature:
		// Temperature parameter 0000h: reserved byte, then degrees Celsius;
		// FFh means the temperature is not available. mhVTL never sets it and
		// reports 0, which no running drive would; that is treated as unknown
		// too rather than shown as 0 °C.
		if v, ok := p.Param(0x0000); ok && len(v) >= 2 && v[1] != 0xff && v[1] != 0 {
			t := int(v[1])
			st.TemperatureC = &t
		}
	case pageDeviceStatistics:
		st.Device14 = &DeviceStatistics{
			VolumeLoads:        uintp(p, 0x0000),
			CleaningOperations: uintp(p, 0x0001),
			PowerOnHours:       uintp(p, 0x0002),
			MediumMotionHours:  uintp(p, 0x0003),
			MetresProcessed:    uintp(p, 0x0004),
		}
	case pageVolume:
		st.Volume = volume(p)
	}
	return nil
}

// logSense issues LOG SENSE for pg, reissuing it after UNIT ATTENTION, and
// returns the bytes transferred. RESERVATION CONFLICT wraps ErrReserved.
func logSense(dev scsi.Device, pg uint8) ([]byte, error) {
	if pg == scsi.LogPageTapeAlert {
		// Reading TapeAlert clears the flags; see the package comment.
		return nil, errors.New("drivestats: refusing to read the TapeAlert page")
	}
	buf := make([]byte, allocLen)
	cdb := scsi.LogSenseCDB(pg, 0, allocLen)
	for i := 0; ; i++ {
		n, _, err := dev.Do(cdb, buf, logSenseTimeout)
		var se *scsi.SenseError
		if i < unitAttentionRetries && errors.As(err, &se) && se.Key == scsi.KeyUnitAttention {
			continue
		}
		var st *scsi.StatusError
		if errors.As(err, &st) && st.Status == statusReservationConflict {
			return nil, fmt.Errorf("%w: %w", ErrReserved, err)
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

func sequential(p scsi.LogPage) *Sequential {
	s := &Sequential{
		HostBytesWritten:   uintp(p, 0x0000),
		MediumBytesWritten: uintp(p, 0x0001),
		MediumBytesRead:    uintp(p, 0x0002),
		HostBytesRead:      uintp(p, 0x0003),
		CapacityMiB:        capacity(p, 0x0004),
		CapacityToEWMiB:    capacity(p, 0x0005),
		EWToLEOPMiB:        capacity(p, 0x0006),
		PositionMiB:        capacity(p, 0x0007),
	}
	s.WriteCompressionRatio = ratio(s.HostBytesWritten, s.MediumBytesWritten)
	s.ReadCompressionRatio = ratio(s.HostBytesRead, s.MediumBytesRead)
	return s
}

func volume(p scsi.LogPage) *Volume {
	return &Volume{
		Mounts:                   uintp(p, 0x0001),
		DatasetsWritten:          uintp(p, 0x0002),
		DatasetsRead:             uintp(p, 0x0007),
		LastMountMBWritten:       uintp(p, 0x000e),
		LastMountMBRead:          uintp(p, 0x000f),
		LifetimeMBWritten:        uintp(p, 0x0010),
		LifetimeMBRead:           uintp(p, 0x0011),
		LastLoadWriteCompression: nonZero(p, 0x0012),
		LastLoadReadCompression:  nonZero(p, 0x0013),
		NativeCapacity:           capacity(p, 0x0016),
		UsedNativeCapacity:       capacity(p, 0x0017),
		SerialNumber:             ascii(p, 0x0040),
		Barcode:                  ascii(p, 0x0042),
		WriteProtect:             flag(p, 0x0080),
		WORM:                     flag(p, 0x0081),
	}
}

func errorCounters(p scsi.LogPage) *ErrorCounters {
	return &ErrorCounters{
		CorrectedWithoutDelay: uintp(p, 0x0000),
		CorrectedWithDelay:    uintp(p, 0x0001),
		Retries:               uintp(p, 0x0002),
		Corrected:             uintp(p, 0x0003),
		AlgorithmInvocations:  uintp(p, 0x0004),
		BytesProcessed:        uintp(p, 0x0005),
		Uncorrected:           uintp(p, 0x0006),
	}
}

// uintp returns parameter code as an integer, nil when absent.
func uintp(p scsi.LogPage, code uint16) *uint64 {
	v, ok := p.Uint(code)
	if !ok {
		return nil
	}
	return &v
}

// nonZero is uintp with zero treated as not reported.
func nonZero(p scsi.LogPage, code uint16) *uint64 {
	if v := uintp(p, code); v != nil && *v != 0 {
		return v
	}
	return nil
}

// capacity is uintp with the 4-byte "unknown" values FFFFFFFEh/FFFFFFFFh
// (no cartridge loaded) treated as not reported.
func capacity(p scsi.LogPage, code uint16) *uint64 {
	if v := uintp(p, code); v != nil && *v != 0xfffffffe && *v != 0xffffffff {
		return v
	}
	return nil
}

// flag reports whether parameter code is non-zero, nil when absent.
func flag(p scsi.LogPage, code uint16) *bool {
	v, ok := p.Param(code)
	if !ok {
		return nil
	}
	set := false
	for _, c := range v {
		set = set || c != 0
	}
	return &set
}

// ascii returns an ASCII parameter trimmed of blank and NUL padding.
func ascii(p scsi.LogPage, code uint16) string {
	v, _ := p.Param(code)
	return strings.Trim(string(v), " \x00")
}

// ratio returns host/medium, nil when either is absent or medium is zero.
func ratio(host, medium *uint64) *float64 {
	if host == nil || medium == nil || *medium == 0 {
		return nil
	}
	r := float64(*host) / float64(*medium)
	return &r
}

func pageName(pg uint8) string { return fmt.Sprintf("0x%02x", pg) }

// unitSerial is the serial the drive reports in VPD page 80h. mhVTL pads the
// device.conf serial of an IBM LTO drive (vendor IBM, product ULT3580-* or
// ULTRIUM-*) shorter than 10 characters to 10 with leading zeroes, as the real
// drives do (mhvtl usr/vtllib.c ibm_lto_serial_pad); other serials are
// reported as configured.
func unitSerial(d config.Drive) string {
	sn := strings.TrimRight(d.Serial, " ")
	if strings.TrimRight(d.Vendor, " ") != "IBM" ||
		!(strings.HasPrefix(d.Product, "ULT3580-") || strings.HasPrefix(d.Product, "ULTRIUM-")) ||
		sn == "" || len(sn) >= 10 {
		return sn
	}
	return strings.Repeat("0", 10-len(sn)) + sn
}
