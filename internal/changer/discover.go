package changer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mhvtld/internal/config"
	"mhvtld/internal/scsi"
)

// inquiryTimeout bounds the INQUIRY fallback used during discovery.
const inquiryTimeout = 10 * time.Second

// Locator finds the /dev/sgN node of a library's medium changer.
type Locator struct {
	// SysRoot is the sysfs mount point ("/sys").
	SysRoot string
	// DevRoot is the device directory holding the sg nodes ("/dev").
	DevRoot string
	// Overrides pins library IDs to explicit sg paths (--changer), skipping
	// discovery for those libraries.
	Overrides map[int]string
	// Open opens a node for the INQUIRY fallback when sysfs lacks the data.
	Open Opener
}

// Find returns the sg path of lib's changer. It scans
// <SysRoot>/class/scsi_generic/sg*/device for a medium changer (type 8) whose
// unit serial number (sysfs vpd_pg80, else INQUIRY VPD 80h) equals the
// library's "Unit serial number" from device.conf.
func (l *Locator) Find(lib config.Library) (string, error) {
	if p, ok := l.Overrides[lib.ID]; ok {
		return p, nil
	}
	want := strings.TrimSpace(lib.Serial)
	if want == "" {
		return "", fmt.Errorf("%w: library %d has no Unit serial number in device.conf", ErrUnavailable, lib.ID)
	}
	classDir := filepath.Join(l.SysRoot, "class", "scsi_generic")
	ents, err := os.ReadDir(classDir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "sg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		devDir := filepath.Join(classDir, name, "device")
		node := filepath.Join(l.DevRoot, name)
		if !l.isChanger(devDir, node) {
			continue
		}
		if serial, ok := l.serial(devDir, node); ok && serial == want {
			return node, nil
		}
	}
	return "", fmt.Errorf("%w: no medium changer with serial %q (library %d) under %s", ErrUnavailable, want, lib.ID, classDir)
}

// isChanger reports whether the device is a medium changer: sysfs "type",
// falling back to the standard INQUIRY peripheral device type.
func (l *Locator) isChanger(devDir, node string) bool {
	if b, err := os.ReadFile(filepath.Join(devDir, "type")); err == nil {
		return strings.TrimSpace(string(b)) == "8"
	}
	buf := make([]byte, 96)
	n, err := l.inquiry(node, scsi.InquiryCDB(false, 0, uint16(len(buf))), buf)
	if err != nil {
		return false
	}
	inq, err := scsi.ParseStdInquiry(buf[:n])
	return err == nil && inq.PeripheralType == scsi.PeripheralMediumChanger
}

// serial returns the unit serial number: sysfs vpd_pg80, falling back to
// INQUIRY VPD page 80h.
func (l *Locator) serial(devDir, node string) (string, bool) {
	if b, err := os.ReadFile(filepath.Join(devDir, "vpd_pg80")); err == nil {
		if s, err := scsi.ParseVPDSerial(b); err == nil {
			return s, true
		}
	}
	buf := make([]byte, 255)
	n, err := l.inquiry(node, scsi.InquiryCDB(true, scsi.VPDUnitSerial, uint16(len(buf))), buf)
	if err != nil {
		return "", false
	}
	s, err := scsi.ParseVPDSerial(buf[:n])
	return s, err == nil
}

func (l *Locator) inquiry(node string, cdb, buf []byte) (int, error) {
	if l.Open == nil {
		return 0, errors.New("changer: no opener for INQUIRY fallback")
	}
	dev, err := l.Open(node)
	if err != nil {
		return 0, err
	}
	defer dev.Close()
	return run(dev, cdb, buf, inquiryTimeout)
}
