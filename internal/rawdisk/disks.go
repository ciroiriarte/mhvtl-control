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
		disks = append(disks, probeSysBlock(sysDir, name))
	}

	for _, p := range opts.ScanPaths {
		disks = append(disks, probeScanPath(p))
	}

	return disks, nil
}

// probeSysBlock builds a Disk from /sys/block/<name> and a ReadSuperblock
// attempt against /dev/<name>.
func probeSysBlock(sysDir, name string) Disk {
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

	attachSuperblock(&d, d.Path)
	return d
}

// probeScanPath builds a Disk from an explicit image/device path.
func probeScanPath(path string) Disk {
	d := Disk{Path: path, Name: filepath.Base(path)}
	if fi, err := os.Stat(path); err == nil {
		if fi.Size() >= 0 {
			d.SizeBytes = uint64(fi.Size())
		}
	}
	attachSuperblock(&d, path)
	return d
}

// attachSuperblock runs ReadSuperblock and records the result. Absence and
// permission errors are swallowed; only a valid superblock sets MVTLFormatted.
func attachSuperblock(d *Disk, path string) {
	sb, err := ReadSuperblock(path)
	if err != nil {
		return
	}
	d.MVTL = sb
	d.MVTLFormatted = true
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
