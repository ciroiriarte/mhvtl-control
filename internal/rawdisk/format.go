package rawdisk

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// barcodePattern bounds a cartridge barcode to the characters real pcls use.
// It must start with an alphanumeric, so the value can never look like a flag.
var barcodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// FormatRequest describes a request to initialize a device with an MVTL
// superblock.
type FormatRequest struct {
	Device  string // target device/image path
	Barcode string // cartridge barcode (pcl), e.g. "VOL001L5"
	Vendor  string // emulated vendor id (advisory; see ToolFormatter.Format)
	Product string // emulated product id (advisory; see ToolFormatter.Format)
	// CapacityBytes is advisory: the raw-format tool derives capacity from the
	// device geometry. Retained for callers/UI that pre-compute it.
	CapacityBytes uint64
	Force         bool // overwrite an existing valid superblock
}

// ErrAlreadyFormatted is returned by a Formatter when the device already
// carries a valid MVTL superblock and Force was not set.
var ErrAlreadyFormatted = errors.New("device already carries an MVTL superblock; use force")

// ErrDeviceInUse is returned when the target device is mounted (or a partition
// of it is) or backs a critical system mount. It is refused unconditionally,
// even with Force, to prevent wiping a live or system disk.
var ErrDeviceInUse = errors.New("rawdisk: device is mounted or a system disk; refusing to format")

// ErrMountCheckUnavailable is returned when the mount table cannot be read and
// the target is a real block device. The in-use guard cannot be evaluated, so
// formatting fails closed rather than risk wiping a mounted or system disk.
var ErrMountCheckUnavailable = errors.New("rawdisk: cannot read mount table to verify a block device is not in use; refusing to format")

// Formatter initializes a device with an MVTL superblock and returns the
// resulting superblock summary.
type Formatter interface {
	Format(req FormatRequest) (*Superblock, error)
}

// ToolFormatter formats a device by invoking the existing C tool
// (mhvtl-raw-format / mhvtl-raw-provision) rather than reimplementing the
// on-disk writer.
type ToolFormatter struct {
	// ToolPath is the path to the mhvtl-raw-format executable.
	ToolPath string
	// Mounts is the mount-state seam used to refuse formatting a mounted or
	// system disk. When nil, DefaultMounts() is consulted at call time.
	Mounts MountChecker
}

// Format guards against clobbering an existing cartridge, then execs the
// raw-format tool and re-reads the resulting superblock.
//
// CLI mapping (mhvtl/usr/cmd/mhvtl-raw-format.c):
//
//	mhvtl-raw-format <device> --barcode <id>
//	    [--partition-data <pct,...>] [--min-record-size <size>] [--partitions N]
//
// The tool takes capacity from the device geometry and derives the emulated
// vendor/product from the barcode's density suffix, so FormatRequest.Vendor,
// Product, and CapacityBytes are not passed as flags; they remain on the
// request for callers that display or pre-validate them.
func (t ToolFormatter) Format(req FormatRequest) (*Superblock, error) {
	if req.Device == "" {
		return nil, errors.New("rawdisk: Format requires a device")
	}
	if req.Barcode == "" {
		return nil, errors.New("rawdisk: Format requires a barcode")
	}
	if t.ToolPath == "" {
		return nil, errors.New("rawdisk: ToolFormatter requires ToolPath")
	}
	// Argv-injection guards: Device and Barcode become subprocess arguments, so
	// reject anything that could be mistaken for a flag or carry an unexpected
	// payload. A device must be an absolute path; a barcode must match the
	// restricted pcl charset.
	if !strings.HasPrefix(req.Device, "/") {
		return nil, fmt.Errorf("rawdisk: device must be an absolute path, got %q", req.Device)
	}
	if !barcodePattern.MatchString(req.Barcode) {
		return nil, fmt.Errorf("rawdisk: invalid barcode %q", req.Barcode)
	}

	// Refuse a mounted or system disk unconditionally, before any force logic:
	// Force may overwrite an existing cartridge, never a live/system disk.
	mounts := t.Mounts
	if mounts == nil {
		m, err := DefaultMounts()
		if err != nil {
			// Fail closed: without a mount table we cannot prove the target is
			// not mounted or a system disk. For a real block device that risks
			// wiping a live disk, so refuse. A regular file (image, as used in
			// tests) cannot be a mount source, so proceed.
			if isBlockDevice(req.Device) {
				return nil, ErrMountCheckUnavailable
			}
		} else {
			mounts = m
		}
	}
	if mounts != nil && (mounts.IsDeviceInUse(req.Device) || mounts.IsSystemDisk(req.Device)) {
		return nil, ErrDeviceInUse
	}

	if !req.Force {
		if _, err := ReadSuperblock(req.Device); err == nil {
			return nil, ErrAlreadyFormatted
		} else if !isAbsentSuperblock(err) {
			// A read error other than "no/invalid superblock" (e.g. permission
			// denied, device missing) is reported rather than silently formatted.
			return nil, fmt.Errorf("rawdisk: probing %s before format: %w", req.Device, err)
		}
	}

	cmd := exec.Command(t.ToolPath, formatArgs(req.Barcode, req.Device)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("rawdisk: %s failed: %w: %s", t.ToolPath, err, string(out))
	}

	sb, err := ReadSuperblock(req.Device)
	if err != nil {
		return nil, fmt.Errorf("rawdisk: re-reading superblock after format: %w", err)
	}
	return sb, nil
}

// formatArgs builds the mhvtl-raw-format argument vector. "--" terminates
// option parsing so the positional device can never be interpreted as a flag,
// even if the Device validation in Format were ever loosened.
func formatArgs(barcode, device string) []string {
	return []string{"--barcode", barcode, "--", device}
}

// isBlockDevice reports whether path is a block special device (the S_ISBLK
// equivalent). A stat failure or any non-block mode returns false, so the
// fail-closed guard fires only for a confirmed block device.
func isBlockDevice(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := fi.Mode()
	return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
}

// isAbsentSuperblock reports whether err means "no usable MVTL superblock",
// as opposed to an I/O/permission error.
func isAbsentSuperblock(err error) bool {
	return errors.Is(err, ErrNotFormatted) ||
		errors.Is(err, ErrBadCRC) ||
		errors.Is(err, ErrUnsupportedVersion)
}
