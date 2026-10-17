package apply

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// DaemonUnit is the only unit whose processes may call the helper.
const DaemonUnit = "mhvtl-control.service"

// Peer is the process at the other end of the socket, as the kernel
// reports it.
type Peer struct {
	PID    int32  `json:"pid"`
	UID    uint32 `json:"uid"`
	Cgroup string `json:"cgroup"`
}

// Linux constants the syscall package lacks. SO_PEERPIDFD is in
// include/uapi/asm-generic/socket.h since Linux 6.5; pidfd_send_signal is
// 424 on every architecture since Linux 5.1 (the unified syscall table).
const (
	soPeerPIDFD          = 77
	sysPidfdSendSignal   = 424
	procRoot             = "/proc"
	errPeerNotDaemonUnit = "peer is not in " + DaemonUnit
)

// CheckPeer authenticates the process at the other end of conn (plan 3.2,
// peer.go): it must be uid 0 (SO_PEERCRED) and run in the cgroup of
// mhvtl-control.service. The cgroup is read from /proc/<pid>/cgroup, so the
// pid must still name the process that connected when it is read: with
// SO_PEERPIDFD (Linux 6.5 and later) a pidfd taken at connect time is
// checked alive after the read, which pins it; otherwise the process start
// time is read before and after, which catches a pid reused between the two
// reads but not before the first. This is defence in depth: the boundary is
// the daemon's own sandbox and the socket's mode 0600.
func CheckPeer(conn *net.UnixConn) (Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *syscall.Ucred
	var credErr error
	pidfd := -1
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if credErr == nil {
			pidfd = peerPIDFD(int(fd))
		}
	}); err != nil {
		return Peer{}, err
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	if pidfd >= 0 {
		defer syscall.Close(pidfd)
	}
	return verifyPeer(procRoot, cred.Pid, cred.Uid, func() error {
		if pidfd < 0 {
			return errNoPIDFD
		}
		return pidfdAlive(pidfd)
	})
}

var errNoPIDFD = errors.New("no pidfd")

// peerPIDFD returns a pidfd of the socket's peer, or -1 when the kernel
// has no SO_PEERPIDFD.
func peerPIDFD(fd int) int {
	var pidfd int32
	l := uint32(unsafe.Sizeof(pidfd))
	_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), syscall.SOL_SOCKET, soPeerPIDFD,
		uintptr(unsafe.Pointer(&pidfd)), uintptr(unsafe.Pointer(&l)), 0)
	if e != 0 {
		return -1
	}
	return int(pidfd)
}

// pidfdAlive sends signal 0 through pidfd: ESRCH once the process has
// exited, whatever now holds its pid.
func pidfdAlive(pidfd int) error {
	_, _, e := syscall.Syscall6(sysPidfdSendSignal, uintptr(pidfd), 0, 0, 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("peer exited: %w", e)
	}
	return nil
}

// verifyPeer checks uid and the cgroup of pid under proc. alive reports
// whether the connecting process still holds pid after the cgroup is read;
// errNoPIDFD makes it fall back to the start-time recheck.
func verifyPeer(proc string, pid int32, uid uint32, alive func() error) (Peer, error) {
	p := Peer{PID: pid, UID: uid}
	if uid != 0 {
		return p, fmt.Errorf("peer uid %d, not 0", uid)
	}
	start, err := startTime(proc, pid)
	if err != nil {
		return p, err
	}
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(int(pid)), "cgroup"))
	if err != nil {
		return p, err
	}
	switch err := alive(); {
	case errors.Is(err, errNoPIDFD):
		again, err := startTime(proc, pid)
		if err != nil {
			return p, err
		}
		if again != start {
			return p, errors.New("peer exited while it was checked")
		}
	case err != nil:
		return p, err
	}
	p.Cgroup = unitCgroup(b)
	if !daemonCgroup(p.Cgroup) {
		return p, fmt.Errorf("%s (cgroup %q)", errPeerNotDaemonUnit, p.Cgroup)
	}
	return p, nil
}

// startTime returns field 22 of /proc/<pid>/stat, the process start time.
// The command name (field 2) may hold blanks and parentheses, so fields are
// counted after its last ')'.
func startTime(proc string, pid int32) (string, error) {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(int(pid)), "stat"))
	if err != nil {
		return "", err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", errors.New("malformed stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", errors.New("malformed stat")
	}
	return f[19], nil // field 22: fields 3.. start at f[0]
}

// unitCgroup returns the systemd cgroup path of /proc/<pid>/cgroup: the
// unified hierarchy ("0::"), else the named systemd one of a legacy or
// hybrid setup.
func unitCgroup(b []byte) string {
	var named string
	for _, l := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(l, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			return parts[2]
		}
		if parts[1] == "name=systemd" {
			named = parts[2]
		}
	}
	return named
}

// daemonCgroup reports whether path is the cgroup of mhvtl-control.service:
// its last component is the unit and every component above it is a slice,
// so a sub-cgroup named after the unit inside another unit does not pass.
func daemonCgroup(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if !strings.HasPrefix(path, "/") || parts[len(parts)-1] != DaemonUnit {
		return false
	}
	for _, p := range parts[:len(parts)-1] {
		if !strings.HasSuffix(p, ".slice") {
			return false
		}
	}
	return true
}
