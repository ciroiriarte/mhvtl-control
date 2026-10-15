// Package ipc implements mhvtl's System V message-queue control protocol.
//
// The on-the-wire frame mirrors mhvtl's C types from include/utils/q.h:
//
//	struct q_msg   { long snd_id; char text[MAXTEXTLEN + 1]; };
//	struct q_entry { long rcv_id; struct q_msg msg; };
//
// On linux/amd64 a C `long` is 8 bytes, so struct q_msg is 8 (snd_id) +
// 1025 (text) = 1033 bytes, padded up to the 8-byte struct alignment to
// 1040 bytes. That padded value (MAXOBN = sizeof(struct q_msg)) is the
// msgsz passed to msgsnd(2)/msgrcv(2); the kernel treats q_entry.rcv_id as
// the SysV message type (mtype) which precedes the msgsz-counted payload.
package ipc

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// Protocol constants from include/utils/q.h.
const (
	// DefaultQKey is MHVTL_DEFAULT_QKEY: ASCII "Mark".
	DefaultQKey = 0x4d61726b
	// VTLCMDQ is VTLCMD_Q, the snd_id vtlcmd sends with and so the mtype
	// its replies come back on. Device ids are below it.
	VTLCMDQ = 32768
	// MaxText is MAXTEXTLEN, the usable command/reply length.
	MaxText = 1024
	// TextLen is sizeof(q_msg.text) (MAXTEXTLEN + 1, room for the NUL).
	TextLen = 1025
)

// SysV IPC flags/commands (asm-generic/ipc.h). Declared locally to avoid a
// dependency on golang.org/x/sys.
const (
	ipcCreat  = 0x200 // IPC_CREAT
	ipcNowait = 0x800 // IPC_NOWAIT
	ipcRmid   = 0     // IPC_RMID
	qperm     = 0660  // QPERM: queue permission bits
)

// Frame geometry, computed to match C struct layout exactly.
//
//	msgTextRegion = sizeof(long snd_id) + sizeof(text)       = 8 + 1025 = 1033
//	qMsgSize      = align8(msgTextRegion) = sizeof(q_msg)     = 1040  (== MAXOBN, the msgsz)
//	frameSize     = sizeof(long rcv_id) + qMsgSize = sizeof(q_entry) = 1048
const (
	mtypeLen      = 8                        // sizeof(long) on linux/amd64
	msgTextRegion = mtypeLen + TextLen       // snd_id + text, unpadded
	qMsgSize      = (msgTextRegion + 7) &^ 7 // sizeof(struct q_msg), 8-byte aligned
	frameSize     = mtypeLen + qMsgSize      // sizeof(struct q_entry)
)

// ErrTimeout is returned by Receive when no reply arrives before the deadline.
var ErrTimeout = errors.New("ipc: receive timed out")

// QKey returns the SysV queue key: MHVTL_QKEY (parsed with base 0, matching
// the C strtoul(env, NULL, 0)) when set, else DefaultQKey.
func QKey() int {
	if v := os.Getenv("MHVTL_QKEY"); v != "" {
		if n, err := strconv.ParseInt(v, 0, 64); err == nil {
			return int(n)
		}
	}
	return DefaultQKey
}

// ReplyType returns the private reply mtype of process pid: VTLCMDQ + pid.
// vtllibrary and vtltape answer a command on the mtype named by its snd_id,
// so a client that sends with its own snd_id reads only its own replies,
// never those meant for vtlcmd or another client. Device ids are below
// VTLCMDQ, so the value can't name a device either.
func ReplyType(pid int) int64 {
	return VTLCMDQ + int64(pid)
}

// Client is the low-level msgsnd/msgrcv client for one queue.
type Client struct {
	qid int
	key int
	// Reply is the snd_id commands are sent with, and so the mtype their
	// replies are read from. Zero means VTLCMDQ, the channel vtlcmd shares.
	Reply int64
}

func (c *Client) reply() int64 {
	if c.Reply == 0 {
		return VTLCMDQ
	}
	return c.Reply
}

// OpenQueue opens (or, when create, creates) the SysV message queue for key.
func (c *Client) OpenQueue(key int, create bool) error {
	flags := uintptr(qperm)
	if create {
		flags |= ipcCreat
	}
	qid, _, errno := syscall.Syscall(syscall.SYS_MSGGET, uintptr(key), flags, 0)
	if errno != 0 {
		return errno
	}
	c.qid = int(qid)
	c.key = key
	return nil
}

// marshalFrame builds a frameSize buffer laid out exactly like a C q_entry:
// [mtype/rcv_id 8 LE][snd_id 8 LE][text 1025][pad 7]. text is truncated to
// MaxText and NUL-terminated.
func marshalFrame(rcvID, sndID int64, text string) []byte {
	buf := make([]byte, frameSize)
	putU64LE(buf[0:8], uint64(rcvID))
	putU64LE(buf[8:16], uint64(sndID))
	n := len(text)
	if n > MaxText {
		n = MaxText
	}
	copy(buf[16:16+n], text[:n])
	buf[16+n] = 0 // explicit NUL terminator (buffer is already zeroed)
	return buf
}

func putU64LE(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func getU64LE(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// cstr returns the NUL-terminated prefix of b as a Go string.
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Send delivers text to device deviceNo: mtype=deviceNo, snd_id=c.Reply.
func (c *Client) Send(deviceNo int64, text string) error {
	buf := marshalFrame(deviceNo, c.reply(), text)
	_, _, errno := syscall.Syscall6(syscall.SYS_MSGSND, uintptr(c.qid),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(qMsgSize), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Receive reads the next reply (mtype == c.Reply) and returns its snd_id and
// text. It polls with IPC_NOWAIT until timeoutMs elapses so the wait is
// cancellable; a non-positive timeoutMs makes a single attempt.
func (c *Client) Receive(timeoutMs int) (snd int64, text string, err error) {
	buf := make([]byte, frameSize)
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_MSGRCV, uintptr(c.qid),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(qMsgSize),
			uintptr(c.reply()), uintptr(ipcNowait), 0)
		if errno == 0 {
			snd = int64(getU64LE(buf[8:16]))
			text = cstr(buf[16 : 16+TextLen])
			return snd, text, nil
		}
		if errno == syscall.ENOMSG {
			if !time.Now().Before(deadline) {
				return 0, "", ErrTimeout
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return 0, "", errno
	}
}

// Remove destroys the queue (msgctl IPC_RMID); used for test cleanup.
func (c *Client) Remove() error {
	_, _, errno := syscall.Syscall(syscall.SYS_MSGCTL, uintptr(c.qid), uintptr(ipcRmid), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
