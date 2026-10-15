// Package scsi issues SCSI commands to Linux SCSI generic (/dev/sgN) nodes via
// the SG_IO ioctl and builds/parses the CDBs and responses mhvtld needs to
// drive a medium changer (INQUIRY, MODE SENSE page 1Dh, READ ELEMENT STATUS,
// MOVE MEDIUM).
//
// The ioctl is issued with raw syscalls and a locally declared struct
// sg_io_hdr (include/scsi/sg.h) so the module stays stdlib-only.
package scsi

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Device executes one SCSI command. dataIn receives the data-in phase (nil or
// empty for a command with no data transfer). It returns the number of bytes
// actually transferred into dataIn and any sense data the target returned.
// err is a *SenseError for CHECK CONDITION, a *StatusError for any other
// non-GOOD SCSI status, and a *TransportError for a host/driver failure.
type Device interface {
	Do(cdb, dataIn []byte, timeout time.Duration) (n int, sense []byte, err error)
}

// SG_IO ioctl request and its interface id (include/scsi/sg.h).
const (
	sgIO          = 0x2285
	sgInterfaceID = 'S'
)

// dxfer_direction values (include/scsi/sg.h).
const (
	sgDxferNone      = -1 // SG_DXFER_NONE: no data transfer
	sgDxferToDev     = -2 // SG_DXFER_TO_DEV: data-out
	sgDxferFromDev   = -3 // SG_DXFER_FROM_DEV: data-in
	sgDxferToFromDev = -4 // SG_DXFER_TO_FROM_DEV: indirect-IO both ways
)

// driverSense is DRIVER_SENSE: the driver_status bit that only says sense
// data is present, which is not a transport failure.
const driverSense = 0x08

// senseBufLen is the sense buffer offered to the driver (SCSI_SENSE_BUFFERSIZE).
const senseBufLen = 96

// sgIOHdr mirrors struct sg_io_hdr for linux/amd64 (88 bytes). The explicit
// pad keeps usr_ptr on its 8-byte boundary exactly where the C compiler puts
// it; sgio_test.go pins every offset.
type sgIOHdr struct {
	InterfaceID    int32          // 0
	DxferDirection int32          // 4
	CmdLen         uint8          // 8
	MxSbLen        uint8          // 9
	IovecCount     uint16         // 10
	DxferLen       uint32         // 12
	Dxferp         unsafe.Pointer // 16
	Cmdp           unsafe.Pointer // 24
	Sbp            unsafe.Pointer // 32
	Timeout        uint32         // 40, milliseconds
	Flags          uint32         // 44
	PackID         int32          // 48
	_              [4]byte        // 52, alignment pad
	UsrPtr         unsafe.Pointer // 56
	Status         uint8          // 64
	MaskedStatus   uint8          // 65
	MsgStatus      uint8          // 66
	SbLenWr        uint8          // 67
	HostStatus     uint16         // 68
	DriverStatus   uint16         // 70
	Resid          int32          // 72
	Duration       uint32         // 76
	Info           uint32         // 80
}

// StatusError is a non-GOOD, non-CHECK CONDITION SCSI status (e.g. BUSY 08h,
// RESERVATION CONFLICT 18h).
type StatusError struct {
	Status uint8
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("scsi: status 0x%02x", e.Status)
}

// TransportError is a failure below the SCSI target: the HBA (host_status)
// or the sg driver (driver_status) could not complete the command.
type TransportError struct {
	HostStatus   uint16
	DriverStatus uint16
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("scsi: transport failure (host_status 0x%02x, driver_status 0x%02x)",
		e.HostStatus, e.DriverStatus)
}

// SGDevice is a Device backed by an open /dev/sgN node.
type SGDevice struct {
	f *os.File
}

var _ Device = (*SGDevice)(nil)

// OpenSG opens a SCSI generic node read-write (SG_IO needs write access for
// commands that change state, such as MOVE MEDIUM).
func OpenSG(path string) (*SGDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &SGDevice{f: f}, nil
}

// Close closes the node.
func (d *SGDevice) Close() error { return d.f.Close() }

// Do issues cdb with SG_IO. A non-empty dataIn selects SG_DXFER_FROM_DEV.
func (d *SGDevice) Do(cdb, dataIn []byte, timeout time.Duration) (int, []byte, error) {
	if len(cdb) == 0 || len(cdb) > 255 {
		return 0, nil, fmt.Errorf("scsi: bad CDB length %d", len(cdb))
	}
	sense := make([]byte, senseBufLen)
	hdr := sgIOHdr{
		InterfaceID:    sgInterfaceID,
		DxferDirection: sgDxferNone,
		CmdLen:         uint8(len(cdb)),
		MxSbLen:        senseBufLen,
		Cmdp:           unsafe.Pointer(&cdb[0]),
		Sbp:            unsafe.Pointer(&sense[0]),
		Timeout:        uint32(timeout / time.Millisecond),
	}
	if len(dataIn) > 0 {
		hdr.DxferDirection = sgDxferFromDev
		hdr.DxferLen = uint32(len(dataIn))
		hdr.Dxferp = unsafe.Pointer(&dataIn[0])
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), sgIO, uintptr(unsafe.Pointer(&hdr)))
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(dataIn)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return 0, nil, &os.SyscallError{Syscall: "ioctl(SG_IO)", Err: errno}
	}
	return checkHdr(&hdr, len(dataIn), sense)
}

// checkHdr turns a completed sg_io_hdr into the Device results.
func checkHdr(hdr *sgIOHdr, dataLen int, sense []byte) (int, []byte, error) {
	n := dataLen - int(hdr.Resid)
	if n < 0 || n > dataLen {
		n = 0
	}
	var sb []byte
	if hdr.SbLenWr > 0 {
		sb = sense[:min(int(hdr.SbLenWr), len(sense))]
	}
	if hdr.HostStatus != 0 || (hdr.DriverStatus&0x0f != 0 && hdr.DriverStatus&0x0f != driverSense) {
		return n, sb, &TransportError{HostStatus: hdr.HostStatus, DriverStatus: hdr.DriverStatus}
	}
	switch hdr.Status {
	case statusGood:
		return n, sb, nil
	case statusCheckCondition:
		return n, sb, DecodeSense(sb)
	default:
		return n, sb, &StatusError{Status: hdr.Status}
	}
}

// SCSI status bytes (SAM).
const (
	statusGood           = 0x00
	statusCheckCondition = 0x02
)
