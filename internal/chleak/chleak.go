// Package chleak detects the conditions of the Linux ch driver's reference
// leak on this host (mhvtl-control#42).
//
// When ch probes a changer, ch_readconfig() takes a scsi_device_lookup()
// reference to every data transfer element (drive) the changer reports, in
// ch->dt[], and nothing releases it, not even ch_remove. So each time a
// changer appears with ch loaded (mhVTL adds a library's changer when
// vtllibrary starts), every drive it reports gains a reference. Once mhVTL
// removes such a drive (on the kernel module backend vtltape removes its LU
// at exit, usr/cmd/vtltape.c:2754 at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f/usr/cmd/vtltape.c#L2754,
// through mhvtl_remove_lu,
// https://github.com/markh794/mhvtl/blob/d65d25f/usr/transport_mhvtl.c#L293-L296),
// the scsi_device cannot be freed: it stays on the host's device list
// without a sysfs node, mhvtl.ko stays in use until a reboot, and
// target_core_pscsi refuses a new device at that address. Fixed by torvalds
// 27c0f718d3b3, "scsi: ch: Do not keep references to data transfer element
// devices", in v7.3-rc7 (https://git.kernel.org/torvalds/c/27c0f718d3b3).
//
// On the TCMU backend mhVTL never removes a LU (tcmu_remove_lu is a no-op,
// usr/transport_tcmu.c:307-310,
// https://github.com/markh794/mhvtl/blob/d65d25f/usr/transport_tcmu.c#L307-L310),
// so only changers on mhVTL's own HBA count.
//
// A distribution may backport the fix, so the kernel release only proves
// it present (7.3-rc7 or later), never absent.
package chleak

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/personality"
)

// Driver is the name the ch driver binds devices under.
const Driver = "ch"

// Changer is an mhVTL changer that ch is bound to. Library is 0 when the
// changer's unit serial cannot be read from sysfs (see Check).
type Changer struct {
	Library int    `json:"library,omitempty"`
	HCTL    string `json:"hctl"`
}

// Status is what this host shows of the leak.
type Status struct {
	// KernelFixed is whether the running kernel's release is 7.3-rc7 or
	// later, which has the fix. False means unknown: an older release may carry a
	// backport.
	KernelFixed bool `json:"kernel_fixed"`
	// Changers are the mhVTL changers ch is bound to.
	Changers []Changer `json:"changers"`
	// ChLoaded is whether the ch driver is registered (loaded, or built
	// in): it then binds to every changer mhVTL adds, so a library started
	// or restarted is exposed even when ch is not bound to it now.
	ChLoaded bool `json:"ch_loaded"`
	// ModuleRefcount is mhvtl.ko's reference count; nil when the module is
	// not loaded (the TCMU backend, or mhVTL stopped and unloaded).
	ModuleRefcount *int `json:"mhvtl_module_refcount,omitempty"`
	// MhvtlDevices counts the SCSI devices on mhVTL's pseudo HBA.
	MhvtlDevices int `json:"mhvtl_devices"`
	// Leaked is a module reference count with no mhVTL device left: what
	// leaked references look like once every device is gone. An
	// application holding a /dev/sg* of a removed device open shows the
	// same until it closes it.
	Leaked bool `json:"leaked"`
}

// Exposed reports whether starting or restarting a library may leak
// references to its drives: mhvtl.ko is loaded (the kernel module backend),
// the kernel is not known to have the fix, and ch is loaded or bound to the
// library's changer.
func (s Status) Exposed(library int) bool {
	if s.KernelFixed || s.ModuleRefcount == nil {
		return false
	}
	if s.ChLoaded {
		return true
	}
	for _, c := range s.Changers {
		if c.Library == library {
			return true
		}
	}
	return false
}

// Check reads the leak's conditions from sysRoot (sysfs), the running
// kernel's release, the libraries of device.conf and the host's devices
// (hostids.Read). A library's changer is the device on mhVTL's HBA
// reporting the unit serial number its personality makes of device.conf's
// (personality.ChangerUnitSerial), as the changer locator matches it. The
// serial is read from sysfs only (vpd_pg80), with no INQUIRY on each
// request; the kernel fills it only for devices of SPC-2 or later, and
// some personalities' changers report SPC (ADIC Scalar, usr/pm/scalar_pm.c:31
// at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f/usr/pm/scalar_pm.c#L31).
// Such a changer is listed by its address alone. It changes no warning: ch
// bound to it means ch is loaded, which exposes every library.
func Check(sysRoot, release string, libs []config.Library, host []hostids.Device) (Status, error) {
	st := Status{KernelFixed: Fixed(release), Changers: []Changer{}}
	if _, err := os.Stat(filepath.Join(sysRoot, "bus", "scsi", "drivers", Driver)); err == nil {
		st.ChLoaded = true
	}
	for _, d := range host {
		if d.Driver == hostids.MhvtlDriver {
			st.MhvtlDevices++
		}
		if d.Bound != Driver || d.Driver != hostids.MhvtlDriver {
			continue
		}
		c := Changer{HCTL: d.HCTL()}
		for _, lib := range libs {
			if d.Serial != "" && lib.Serial != "" && personality.ChangerUnitSerial(lib.Vendor, lib.Product, lib.Serial) == d.Serial {
				c.Library = lib.ID
				break
			}
		}
		st.Changers = append(st.Changers, c)
	}
	b, err := os.ReadFile(filepath.Join(sysRoot, "module", hostids.MhvtlDriver, "refcnt"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case err != nil:
		return st, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return st, err
	}
	st.ModuleRefcount = &n
	st.Leaked = n > 0 && st.MhvtlDevices == 0
	return st, nil
}

// Fixed reports whether a kernel release ("6.12.111+deb13-amd64",
// "7.3.0-rc7") has the fix: 7.3-rc7 or later. A release candidate is told
// by "rcN" anywhere after the version, as in "7.3.0-rc6",
// "7.3.0-0.rc6.20261001.fc44" (Fedora) or "7.3.0-070300rc6-generic"
// (Ubuntu mainline).
func Fixed(release string) bool {
	m := releaseRE.FindStringSubmatch(release)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	switch {
	case major != 7:
		return major > 7
	case minor != 3:
		return minor > 3
	}
	rc := rcRE.FindStringSubmatch(release[len(m[0]):])
	if rc == nil {
		return true
	}
	n, _ := strconv.Atoi(rc[1])
	return n >= 7
}

// releaseRE is a release's major and minor number; rcRE a release
// candidate's number after it.
var (
	releaseRE = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.\d+)?`)
	rcRE      = regexp.MustCompile(`rc(\d+)`)
)
