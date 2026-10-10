package rawdisk

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MountChecker reports whether a device is mounted (in use) or backs a critical
// system mount. It is the seam ToolFormatter and ListDisks consult so tests can
// inject mount state. MountSet satisfies it.
type MountChecker interface {
	// IsDeviceInUse reports whether devPath, or any logical/stacked device
	// layered on top of it, is a mount source.
	IsDeviceInUse(devPath string) bool
	// IsSystemDisk reports whether devPath, or any logical/stacked device
	// layered on top of it, backs "/" or another critical system mount point.
	IsSystemDisk(devPath string) bool
}

// mountEntry is one parsed mount: the backing device (source), where it is
// mounted (mountPoint), and the "major:minor" device number (devNo) of the
// mounted filesystem's backing device (mountinfo field 3; empty for the
// /proc/mounts fallback, which does not report it).
type mountEntry struct {
	source     string
	mountPoint string
	devNo      string
}

// blockGraph resolves the kernel block-device holder graph by kernel name
// (e.g. "sda", "sda2", "dm-0", "md0"). It is the seam that lets IsDeviceInUse /
// IsSystemDisk follow a physical disk up through its partitions, LVM, dm-crypt
// and mdraid layers without a real /sys. The default implementation reads
// sysfs; tests inject an in-memory fake.
type blockGraph interface {
	// devNo returns the "major:minor" device number of name, if known.
	devNo(name string) (string, bool)
	// holders returns the kernel names of devices layered directly on top of
	// name: its partitions and any dm/md devices consuming it.
	holders(name string) []string
}

// MountSet is a snapshot of the system mount table. The zero value reports
// nothing as mounted, which is the safe "unknown" answer when the mount files
// cannot be read. graph, when non-nil, lets matching follow stacked devices
// (LVM/dm-crypt/mdraid) from a physical disk to the mount's backing device
// number; when nil, matching falls back to device-path logic only.
type MountSet struct {
	entries []mountEntry
	graph   blockGraph
}

// criticalMountPoints are the mount points whose backing device is treated as a
// system disk that must never be formatted.
var criticalMountPoints = map[string]bool{
	"/":         true,
	"/boot":     true,
	"/boot/efi": true,
	"/usr":      true,
	"/var":      true,
	"/etc":      true,
}

// DefaultMounts reads the real mount table, preferring /proc/self/mountinfo and
// falling back to /proc/mounts, and wires the real sysfs holder graph so that
// stacked devices resolve to their backing physical disks. An error is returned
// only when neither file can be read or parsed; callers that want best-effort
// behavior may treat the zero MountSet as "nothing mounted".
func DefaultMounts() (MountSet, error) {
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer f.Close()
		m, err := ParseMountinfo(f)
		if err != nil {
			return MountSet{}, err
		}
		m.graph = defaultBlockGraph()
		return m, nil
	}
	mf, err := os.Open("/proc/mounts")
	if err != nil {
		return MountSet{}, err
	}
	defer mf.Close()
	m, err := parseProcMounts(mf)
	if err != nil {
		return MountSet{}, err
	}
	m.graph = defaultBlockGraph()
	return m, nil
}

// ParseMountinfo parses /proc/self/mountinfo format. Each line is split at the
// " - " separator: on the left half, field 3 (1-based) is the "major:minor"
// device number and field 5 is the mount point; on the right half, field 2 is
// the mount source. Malformed lines are skipped. The returned MountSet has no
// block graph wired (device-path matching only); DefaultMounts attaches the
// real one, and tests inject a fake via withGraph.
func ParseMountinfo(r io.Reader) (MountSet, error) {
	var m MountSet
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		m.entries = append(m.entries, mountEntry{
			source:     right[1],
			mountPoint: left[4],
			devNo:      left[2],
		})
	}
	if err := sc.Err(); err != nil {
		return MountSet{}, err
	}
	return m, nil
}

// parseProcMounts parses /proc/mounts (fstab-style) format: field 1 is the
// mount source and field 2 is the mount point. This format carries no device
// number, so entries resolve by device path only. Malformed lines are skipped.
func parseProcMounts(r io.Reader) (MountSet, error) {
	var m MountSet
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		m.entries = append(m.entries, mountEntry{source: f[0], mountPoint: f[1]})
	}
	if err := sc.Err(); err != nil {
		return MountSet{}, err
	}
	return m, nil
}

// withGraph returns a copy of m with the given block graph attached. It is the
// test seam for pairing a parsed mount table with an in-memory holder graph.
func (m MountSet) withGraph(g blockGraph) MountSet {
	m.graph = g
	return m
}

// IsDeviceInUse reports whether devPath, or any logical/stacked device layered
// on top of it, is a mount source. Matching is by device number via the holder
// graph (catching LVM/dm-crypt/mdraid), with a device-path fallback for direct
// partitions when no graph is wired.
func (m MountSet) IsDeviceInUse(devPath string) bool {
	covered := m.coveredDevNos(devPath)
	for _, e := range m.entries {
		if e.devNo != "" && covered[e.devNo] {
			return true
		}
		if deviceMatches(devPath, e.source) {
			return true
		}
	}
	return false
}

// IsSystemDisk reports whether devPath, or any logical/stacked device layered
// on top of it, backs a critical system mount point (see criticalMountPoints).
func (m MountSet) IsSystemDisk(devPath string) bool {
	covered := m.coveredDevNos(devPath)
	for _, e := range m.entries {
		if !criticalMountPoints[e.mountPoint] {
			continue
		}
		if e.devNo != "" && covered[e.devNo] {
			return true
		}
		if deviceMatches(devPath, e.source) {
			return true
		}
	}
	return false
}

// coveredDevNos returns the set of "major:minor" device numbers that devPath
// "covers": its own number plus, transitively, the numbers of every device
// layered on top of it (partitions, and the dm/md devices that consume it or
// its partitions). A disk holding a root LVM PV or a RAID member therefore
// covers the device number of the logical device that "/" is mounted from.
// Returns an empty set when no block graph is wired.
func (m MountSet) coveredDevNos(devPath string) map[string]bool {
	covered := map[string]bool{}
	if m.graph == nil || devPath == "" {
		return covered
	}
	m.walkHolders(devName(devPath), covered, map[string]bool{})
	return covered
}

// walkHolders adds name's device number to covered and recurses into its
// holders, guarding against cycles via seen.
func (m MountSet) walkHolders(name string, covered, seen map[string]bool) {
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	if dn, ok := m.graph.devNo(name); ok {
		covered[dn] = true
	}
	for _, h := range m.graph.holders(name) {
		m.walkHolders(h, covered, seen)
	}
}

// devName maps a device path to its kernel name by stripping a leading "/dev/".
// Non-/dev paths (e.g. image files) are returned unchanged and simply will not
// resolve in the block graph.
func devName(devPath string) string {
	return strings.TrimPrefix(devPath, "/dev/")
}

// deviceMatches reports whether source is devPath itself or a partition of it.
// A partition suffix is an optional "p" followed by digits, covering both
// /dev/sdb -> /dev/sdb1 and /dev/nvme0n1 -> /dev/nvme0n1p1. This is the
// device-path fallback used when no block graph is available.
func deviceMatches(devPath, source string) bool {
	if devPath == "" {
		return false
	}
	if source == devPath {
		return true
	}
	if !strings.HasPrefix(source, devPath) {
		return false
	}
	rest := source[len(devPath):]
	if strings.HasPrefix(rest, "p") {
		rest = rest[1:]
	}
	if rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// sysfsGraph is the default blockGraph, reading the kernel holder graph from
// /sys/class/block, which exposes whole disks, partitions and dm/md devices all
// by kernel name.
type sysfsGraph struct {
	root string
}

// defaultBlockGraph returns the sysfs-backed block graph.
func defaultBlockGraph() blockGraph {
	return sysfsGraph{root: "/sys/class/block"}
}

// devNo reads /sys/class/block/<name>/dev ("major:minor\n").
func (g sysfsGraph) devNo(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(g.root, name, "dev"))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", false
	}
	return s, true
}

// holders returns the devices layered directly on top of name: the dm/md
// devices listed under /sys/class/block/<name>/holders plus any partitions of
// name (child directories carrying a "partition" attribute). Following both
// edges lets a whole disk reach its partitions' holders (e.g. sda -> sda2 ->
// dm-0) as well as holders of the whole disk itself.
func (g sysfsGraph) holders(name string) []string {
	base := filepath.Join(g.root, name)
	var out []string
	if ents, err := os.ReadDir(filepath.Join(base, "holders")); err == nil {
		for _, e := range ents {
			out = append(out, e.Name())
		}
	}
	if ents, err := os.ReadDir(base); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(base, e.Name(), "partition")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	return out
}
