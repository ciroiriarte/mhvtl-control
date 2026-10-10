package rawdisk

import (
	"strings"
	"testing"
)

// fakeMountinfo is a minimal /proc/self/mountinfo fixture: /dev/sda2 backs "/"
// and /dev/sdb1 is mounted at /mnt/backup.
const fakeMountinfo = `25 0 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
36 25 8:17 / /mnt/backup rw,relatime shared:2 - ext4 /dev/sdb1 rw
`

func TestParseMountinfo(t *testing.T) {
	m, err := ParseMountinfo(strings.NewReader(fakeMountinfo))
	if err != nil {
		t.Fatalf("ParseMountinfo: %v", err)
	}

	// /dev/sdb1 is mounted, so its whole-disk device /dev/sdb is in use.
	if !m.IsDeviceInUse("/dev/sdb") {
		t.Error("IsDeviceInUse(/dev/sdb) = false, want true (sdb1 mounted)")
	}
	// Nothing backed by /dev/sdc.
	if m.IsDeviceInUse("/dev/sdc") {
		t.Error("IsDeviceInUse(/dev/sdc) = true, want false")
	}
	// Exact partition path is also in use.
	if !m.IsDeviceInUse("/dev/sdb1") {
		t.Error("IsDeviceInUse(/dev/sdb1) = false, want true")
	}

	// /dev/sda2 backs "/", so /dev/sda is a system disk.
	if !m.IsSystemDisk("/dev/sda") {
		t.Error("IsSystemDisk(/dev/sda) = false, want true (sda2 backs /)")
	}
	// /dev/sdb backs only /mnt/backup, not a critical mount.
	if m.IsSystemDisk("/dev/sdb") {
		t.Error("IsSystemDisk(/dev/sdb) = true, want false")
	}
}

// fakeGraph is an in-memory blockGraph for tests: devNos maps a kernel name to
// its "major:minor" device number, and holder maps a name to the devices
// layered directly on top of it (partitions and/or dm/md consumers).
type fakeGraph struct {
	devNos map[string]string
	holder map[string][]string
}

func (g fakeGraph) devNo(name string) (string, bool) { v, ok := g.devNos[name]; return v, ok }
func (g fakeGraph) holders(name string) []string     { return g.holder[name] }

// TestLVMStackedDeviceResolvesToPhysicalDisk covers an LVM-default layout: "/"
// is mounted from /dev/mapper/rl-root (dm-0, devno 253:0), whose PV sits on the
// sda2 partition of the physical disk sda. deviceMatches never relates
// /dev/mapper/rl-root to /dev/sda, so only the devno holder graph closes the
// gap: sda -> sda2 -> dm-0.
func TestLVMStackedDeviceResolvesToPhysicalDisk(t *testing.T) {
	const lvmMountinfo = `25 0 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/rl-root rw
`
	m, err := ParseMountinfo(strings.NewReader(lvmMountinfo))
	if err != nil {
		t.Fatalf("ParseMountinfo: %v", err)
	}
	m = m.withGraph(fakeGraph{
		devNos: map[string]string{"sda": "8:0", "sda2": "8:2", "dm-0": "253:0"},
		holder: map[string][]string{"sda": {"sda2"}, "sda2": {"dm-0"}},
	})

	if !m.IsSystemDisk("/dev/sda") {
		t.Error("IsSystemDisk(/dev/sda) = false, want true (sda -> sda2 -> dm-0 backs /)")
	}
	if !m.IsDeviceInUse("/dev/sda") {
		t.Error("IsDeviceInUse(/dev/sda) = false, want true (sda backs the root LVM PV)")
	}
	// A sibling disk not in the holder graph is neither in use nor a system disk.
	if m.IsSystemDisk("/dev/sdb") {
		t.Error("IsSystemDisk(/dev/sdb) = true, want false")
	}
	if m.IsDeviceInUse("/dev/sdb") {
		t.Error("IsDeviceInUse(/dev/sdb) = true, want false")
	}
}

// TestMdraidMemberResolvesToPhysicalDisks covers an mdraid root: "/" is mounted
// from /dev/md0 (devno 9:0), whose members are the whole disks sdb and sdc.
// Both members must resolve as system disks via the holder graph.
func TestMdraidMemberResolvesToPhysicalDisks(t *testing.T) {
	const mdMountinfo = `25 0 9:0 / / rw,relatime shared:1 - ext4 /dev/md0 rw
`
	m, err := ParseMountinfo(strings.NewReader(mdMountinfo))
	if err != nil {
		t.Fatalf("ParseMountinfo: %v", err)
	}
	m = m.withGraph(fakeGraph{
		devNos: map[string]string{"sdb": "8:16", "sdc": "8:32", "md0": "9:0"},
		holder: map[string][]string{"sdb": {"md0"}, "sdc": {"md0"}},
	})

	if !m.IsSystemDisk("/dev/sdb") {
		t.Error("IsSystemDisk(/dev/sdb) = false, want true (sdb -> md0 backs /)")
	}
	if !m.IsSystemDisk("/dev/sdc") {
		t.Error("IsSystemDisk(/dev/sdc) = false, want true (sdc -> md0 backs /)")
	}
	if !m.IsDeviceInUse("/dev/sdb") {
		t.Error("IsDeviceInUse(/dev/sdb) = false, want true")
	}
	// A disk that is not a RAID member is unaffected.
	if m.IsSystemDisk("/dev/sdd") {
		t.Error("IsSystemDisk(/dev/sdd) = true, want false")
	}
	if m.IsDeviceInUse("/dev/sdd") {
		t.Error("IsDeviceInUse(/dev/sdd) = true, want false")
	}
}

func TestParseProcMountsFallback(t *testing.T) {
	const procMounts = "/dev/nvme0n1p1 / ext4 rw,relatime 0 0\n"
	m, err := parseProcMounts(strings.NewReader(procMounts))
	if err != nil {
		t.Fatalf("parseProcMounts: %v", err)
	}
	// nvme partition suffix (p1) resolves to the whole device.
	if !m.IsDeviceInUse("/dev/nvme0n1") {
		t.Error("IsDeviceInUse(/dev/nvme0n1) = false, want true")
	}
	if !m.IsSystemDisk("/dev/nvme0n1") {
		t.Error("IsSystemDisk(/dev/nvme0n1) = false, want true (backs /)")
	}
}
