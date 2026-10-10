package ipc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// uniqueKey returns a high, process/time-derived key in the "Mark" range to
// avoid colliding with any real mhvtl queue.
func uniqueKey() int {
	return 0x4d000000 | ((os.Getpid() & 0xfff) << 12) | int(time.Now().UnixNano()&0xfff)
}

// deviceRecv polls (IPC_NOWAIT) for a message addressed to mtype=dev and
// returns its command text. It mirrors the daemon side of the protocol using
// raw syscalls, independent of Client, so a successful round-trip proves the
// kernel actually carried the frame.
func deviceRecv(t *testing.T, qid int, dev int64, timeout time.Duration) string {
	t.Helper()
	buf := make([]byte, frameSize)
	deadline := time.Now().Add(timeout)
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_MSGRCV, uintptr(qid),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(qMsgSize),
			uintptr(dev), uintptr(ipcNowait), 0)
		if errno == 0 {
			return cstr(buf[16 : 16+TextLen])
		}
		if errno == syscall.ENOMSG && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("deviceRecv msgrcv: %v", errno)
		return ""
	}
}

// deviceReply sends text back on mtype=VTLCMDQ, as a daemon replies to vtlcmd.
func deviceReply(t *testing.T, qid int, dev int64, text string) {
	t.Helper()
	buf := marshalFrame(VTLCMDQ, dev, text)
	_, _, errno := syscall.Syscall6(syscall.SYS_MSGSND, uintptr(qid),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(qMsgSize), 0, 0, 0)
	if errno != 0 {
		t.Fatalf("deviceReply msgsnd: %v", errno)
	}
}

// TestRoundTripRealQueue proves a real kernel round-trip: Client.Send delivers
// a command to a fake device goroutine, which replies, and Client.Receive
// returns that reply.
func TestRoundTripRealQueue(t *testing.T) {
	c := &Client{}
	if err := c.OpenQueue(uniqueKey(), true); err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	const dev int64 = 3
	const want = "Loaded OK"

	done := make(chan struct{})
	go func() {
		defer close(done)
		got := deviceRecv(t, c.qid, dev, 2*time.Second)
		if got != "online" {
			t.Errorf("device received %q, want %q", got, "online")
		}
		deviceReply(t, c.qid, dev, want)
	}()

	if err := c.Send(dev, "online"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	snd, text, err := c.Receive(2000)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if text != want {
		t.Errorf("reply text = %q, want %q", text, want)
	}
	if snd != dev {
		t.Errorf("reply snd_id = %d, want %d", snd, dev)
	}
	<-done
}

const crossLangC = `#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/ipc.h>
#include <sys/msg.h>
#include "utils/q.h"

int main(int argc, char **argv) {
	if (argc != 3) { fprintf(stderr, "usage: %s key dev\n", argv[0]); return 10; }
	key_t key = (key_t)strtoul(argv[1], NULL, 0);
	long dev  = atol(argv[2]);

	int qid = msgget(key, 0);
	if (qid < 0) { perror("msgget"); return 2; }

	struct q_entry e;
	if (msgrcv(qid, &e, MAXOBN, dev, 0) < 0) { perror("msgrcv"); return 3; }

	/* Report sizeof(struct q_msg) so the Go test can assert layout agreement. */
	printf("%zu\n", sizeof(struct q_msg));
	fflush(stdout);

	/* Echo the received text back on the VTLCMD_Q reply channel. */
	struct q_entry r;
	r.rcv_id = VTLCMD_Q;
	r.msg.snd_id = dev;
	r.msg.text[0] = '\0';
	strncat(r.msg.text, e.msg.text, MAXTEXTLEN);
	int len = strlen(r.msg.text) + 1 + sizeof(r.msg.snd_id);
	if (msgsnd(qid, &r, len, 0) < 0) { perror("msgsnd"); return 4; }
	return 0;
}
`

// TestCrossLanguageFrame compiles a C responder against mhvtl's real q.h,
// sends it a frame from the Go Client, and asserts (a) the C program read the
// identical text back (byte-compatible q_entry/q_msg layout) and (b) the C
// sizeof(struct q_msg) equals the Go msgsz (qMsgSize).
func TestCrossLanguageFrame(t *testing.T) {
	ccPath, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("cc not available")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "resp.c")
	if err := os.WriteFile(src, []byte(crossLangC), 0o644); err != nil {
		t.Fatalf("write C source: %v", err)
	}
	bin := filepath.Join(dir, "resp")
	// Resolve the submodule include dir relative to this test file so the test
	// is portable across checkouts; skip (not fail) if the submodule is absent.
	inc, err := filepath.Abs(filepath.Join("..", "..", "..", "mhvtl", "include"))
	if err != nil {
		t.Fatalf("resolve include dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inc, "utils", "q.h")); err != nil {
		t.Skipf("mhvtl submodule header not available (%v); skipping cross-language test", err)
	}
	if out, err := exec.Command(ccPath, "-I", inc, src, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("compile C responder: %v\n%s", err, out)
	}

	c := &Client{}
	key := uniqueKey()
	if err := c.OpenQueue(key, true); err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	const dev int64 = 7
	const want = "load map VOL001L5"

	if err := c.Send(dev, want); err != nil {
		t.Fatalf("Send: %v", err)
	}

	out, err := exec.Command(bin, strconv.Itoa(key), strconv.FormatInt(dev, 10)).Output()
	if err != nil {
		t.Fatalf("run C responder: %v", err)
	}
	cSize, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse C sizeof output %q: %v", out, err)
	}
	if cSize != qMsgSize {
		t.Fatalf("C sizeof(struct q_msg) = %d, Go msgsz qMsgSize = %d (mismatch)", cSize, qMsgSize)
	}
	t.Logf("C sizeof(struct q_msg) = %d matches Go msgsz qMsgSize = %d", cSize, qMsgSize)

	_, text, err := c.Receive(2000)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if text != want {
		t.Fatalf("round-tripped text = %q, want %q", text, want)
	}
	t.Logf("cross-language round-trip text = %q", text)
}
