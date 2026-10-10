package rawdisk

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Disk describes a candidate block device (or image file) and, when present,
// the MVTL superblock it carries. Fields that cannot be read degrade to their
// zero value; no probe failure is fatal to enumeration.
type Disk struct {
	Name string // kernel name, e.g. "sdb" (empty for ScanPaths entries)
	Path string // device or image path, e.g. "/dev/sdb"

	SizeBytes          uint64
	LogicalSectorSize  int
	PhysicalSectorSize int

	MVTL          *Superblock
	MVTLFormatted bool

	Model  string
	Serial string

	// TempCelsius is a best-effort SMART/hwmon temperature, nil if unknown.
	TempCelsius *int

	// Status is a coarse per-disk classification: "in_use", "mvtl", "no_mvtl",
	// or "probe_failed". See classifyStatus.
	Status string
}

// Options controls ListDisks.
type Options struct {
	// SysBlockDir is the sysfs block root; defaults to "/sys/block".
	SysBlockDir string
	// IncludeVirtual, when false (default), skips loop*, ram*, dm-*, zram*.
	IncludeVirtual bool
	// ScanPaths are explicit image/device paths inspected directly, without
	// sysfs. This seam lets tests point at a crafted image with no /dev.
	ScanPaths []string
}

const defaultSysBlockDir = "/sys/block"

// virtualPrefixes are kernel-name prefixes skipped unless IncludeVirtual.
var virtualPrefixes = []string{"loop", "ram", "dm-", "zram"}

func isVirtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ListDisks enumerates candidate block devices from sysfs and inspects any
// explicit ScanPaths. Each returned Disk is populated best-effort; unreadable
// attributes are left zero/nil and never cause an error or panic.
func ListDisks(opts Options) ([]Disk, error) {
	sysDir := opts.SysBlockDir
	if sysDir == "" {
		sysDir = defaultSysBlockDir
	}

	var disks []Disk

	// Best-effort mount snapshot: a read failure yields the zero MountSet, which
	// reports nothing in use, so classification still proceeds by superblock.
	mounts, err := DefaultMounts()
	if err != nil {
		mounts = MountSet{}
	}

	entries, err := os.ReadDir(sysDir)
	if err != nil {
		// Missing sysfs (e.g. in a test sandbox) is not fatal; still process
		// ScanPaths. Surface other errors only if there is nothing else to do.
		if !errors.Is(err, os.ErrNotExist) && len(opts.ScanPaths) == 0 {
			return nil, err
		}
	}
	for _, e := range entries {
		name := e.Name()
		if !opts.IncludeVirtual && isVirtual(name) {
			continue
		}
		disks = append(disks, probeSysBlock(sysDir, name, mounts))
	}

	for _, p := range opts.ScanPaths {
		disks = append(disks, probeScanPath(p, mounts))
	}

	return disks, nil
}

// probeSysBlock builds a Disk from /sys/block/<name> and a ReadSuperblock
// attempt against /dev/<name>.
func probeSysBlock(sysDir, name string, mounts MountChecker) Disk {
	base := filepath.Join(sysDir, name)
	d := Disk{Name: name, Path: filepath.Join("/dev", name)}

	if sectors, ok := readUint(filepath.Join(base, "size")); ok {
		d.SizeBytes = sectors * 512 // sysfs "size" is in 512-byte sectors
	}
	if v, ok := readInt(filepath.Join(base, "queue", "logical_block_size")); ok {
		d.LogicalSectorSize = v
	}
	if v, ok := readInt(filepath.Join(base, "queue", "physical_block_size")); ok {
		d.PhysicalSectorSize = v
	}
	d.Model = readStr(filepath.Join(base, "device", "model"))
	d.Serial = readStr(filepath.Join(base, "device", "serial"))

	readErr := attachSuperblock(&d, d.Path)
	d.Status = classifyStatus(d.MVTL, readErr, deviceInUse(mounts, d.Path))
	return d
}

// probeScanPath builds a Disk from an explicit image/device path.
func probeScanPath(path string, mounts MountChecker) Disk {
	d := Disk{Path: path, Name: filepath.Base(path)}
	if fi, err := os.Stat(path); err == nil {
		if fi.Size() >= 0 {
			d.SizeBytes = uint64(fi.Size())
		}
	}
	readErr := attachSuperblock(&d, path)
	d.Status = classifyStatus(d.MVTL, readErr, deviceInUse(mounts, path))
	return d
}

// attachSuperblock runs ReadSuperblock and records the result. Absence and
// permission errors are swallowed from the Disk fields (only a valid superblock
// sets MVTLFormatted) but returned so the caller can classify the disk.
func attachSuperblock(d *Disk, path string) error {
	sb, err := ReadSuperblock(path)
	if err != nil {
		return err
	}
	d.MVTL = sb
	d.MVTLFormatted = true
	return nil
}

// deviceInUse reports whether mounts flags path as mounted or a system disk.
func deviceInUse(mounts MountChecker, path string) bool {
	return mounts != nil && (mounts.IsDeviceInUse(path) || mounts.IsSystemDisk(path))
}

// classifyStatus maps a superblock-read outcome and mount state to a coarse
// per-disk status. "in_use" takes precedence over any superblock state.
func classifyStatus(sb *Superblock, readErr error, inUse bool) string {
	if inUse {
		return "in_use"
	}
	if readErr == nil && sb != nil {
		return "mvtl"
	}
	if errors.Is(readErr, ErrNotFormatted) {
		return "no_mvtl"
	}
	return "probe_failed"
}

// readStr reads a sysfs attribute and trims surrounding whitespace; returns ""
// on any error.
func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readUint reads a base-10 unsigned integer from a sysfs attribute.
func readUint(path string) (uint64, bool) {
	s := readStr(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readInt reads a base-10 signed integer from a sysfs attribute.
func readInt(path string) (int, bool) {
	s := readStr(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}
