package ipc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// deviceReply sends text from dev back on mtype to, as a daemon replies to
// the snd_id of the command it answers.
func deviceReply(t *testing.T, qid int, dev, to int64, text string) {
	t.Helper()
	buf := marshalFrame(to, dev, text)
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
		deviceReply(t, c.qid, dev, VTLCMDQ, want)
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

	/* Echo the received text back to its sender, as vtllibrary does. */
	struct q_entry r;
	r.rcv_id = e.msg.snd_id;
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
	// The mhVTL source is $MHVTL_SRC, else the superproject's sibling
	// submodule; skip (not fail) if neither is there.
	mhvtlSrc := os.Getenv("MHVTL_SRC")
	if mhvtlSrc == "" {
		mhvtlSrc = filepath.Join("..", "..", "..", "mhvtl")
	}
	inc, err := filepath.Abs(filepath.Join(mhvtlSrc, "include"))
	if err != nil {
		t.Fatalf("resolve include dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inc, "utils", "q.h")); err != nil {
		t.Skipf("mhvtl submodule header not available (%v); skipping cross-language test", err)
	}
	if out, err := exec.Command(ccPath, "-I", inc, src, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("compile C responder: %v\n%s", err, out)
	}

	c := &Client{Reply: ReplyType(os.Getpid())}
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

// testReply is the reply mtype test controllers use.
var testReply = ReplyType(os.Getpid())

// newTestController returns a controller for key with a short reply timeout.
func newTestController(key int) *IPCController {
	return &IPCController{key: key, reply: testReply, timeoutMs: 1000}
}

// createQueue creates the queue for key, as the mhvtl daemons do, and
// removes it at test end.
func createQueue(t *testing.T, key int) *Client {
	t.Helper()
	c := &Client{}
	if err := c.OpenQueue(key, true); err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })
	return c
}

// library answers every replying command for dev the way vtllibrary does,
// on the command's snd_id, recording the command texts, until stop is closed.
func library(t *testing.T, qid int, dev int64, answer func(cmd string) string, stop <-chan struct{}) <-chan []string {
	t.Helper()
	got := make(chan []string, 1)
	go func() {
		var cmds []string
		buf := make([]byte, frameSize)
		for {
			select {
			case <-stop:
				got <- cmds
				return
			default:
			}
			_, _, errno := syscall.Syscall6(syscall.SYS_MSGRCV, uintptr(qid),
				uintptr(unsafe.Pointer(&buf[0])), uintptr(qMsgSize),
				uintptr(dev), uintptr(ipcNowait), 0)
			if errno != 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			snd := int64(getU64LE(buf[8:16]))
			cmd := cstr(buf[16 : 16+TextLen])
			cmds = append(cmds, cmd)
			if reply := answer(cmd); reply != "" {
				r := marshalFrame(snd, dev, reply)
				syscall.Syscall6(syscall.SYS_MSGSND, uintptr(qid),
					uintptr(unsafe.Pointer(&r[0])), uintptr(qMsgSize), 0, 0, 0)
			}
		}
	}()
	return got
}

func TestControllerQueueAbsent(t *testing.T) {
	c := newTestController(uniqueKey())
	if err := c.Online(10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Online with no queue: err = %v, want ErrUnavailable", err)
	}
	if err := c.ImportMap(10, "A00001L8"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ImportMap with no queue: err = %v, want ErrUnavailable", err)
	}
}

func TestControllerOpensLazilyAndSurvivesQueueRecreation(t *testing.T) {
	key := uniqueKey()
	c := newTestController(key)
	// mhVTL starts after mhvtld: the first command finds no queue...
	if err := c.Offline(10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before mhVTL: err = %v, want ErrUnavailable", err)
	}
	// ...and a later one finds it without a daemon restart.
	q1 := createQueue(t, key)
	if err := c.Offline(10); err != nil {
		t.Fatalf("after mhVTL start: %v", err)
	}
	if got := deviceRecv(t, q1.qid, 10, time.Second); got != "offline" {
		t.Fatalf("library received %q, want offline", got)
	}

	// mhVTL restarts: the queue is removed and recreated under a new id.
	if err := q1.Remove(); err != nil {
		t.Fatal(err)
	}
	q2 := createQueue(t, key)
	if q2.qid == q1.qid {
		t.Skip("kernel reused the queue id; cannot exercise a stale id")
	}
	if err := c.Online(10); err != nil {
		t.Fatalf("after mhVTL restart: %v", err)
	}
	if got := deviceRecv(t, q2.qid, 10, time.Second); got != "online" {
		t.Fatalf("library received %q on the new queue, want online", got)
	}
}

func TestControllerImportMap(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	stop := make(chan struct{})
	cmds := library(t, q.qid, 10, func(cmd string) string {
		if cmd == "load map DUP001L8" {
			return "barcode already in library"
		}
		return "OK"
	}, stop)

	c := newTestController(key)
	if err := c.ImportMap(10, "A00001L8"); err != nil {
		t.Fatalf("ImportMap: %v", err)
	}
	err := c.ImportMap(10, "DUP001L8")
	var re *ReplyError
	if !errors.As(err, &re) || re.Reply != "barcode already in library" {
		t.Fatalf("refused import: err = %v, want ReplyError with the library's reply", err)
	}
	close(stop)
	want := []string{"open map", "load map A00001L8", "close map", "open map", "load map DUP001L8", "close map"}
	if got := <-cmds; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("library received %q, want %q (MAP closed even after a refused load)", got, want)
	}
}

func TestControllerMapVerbs(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	stop := make(chan struct{})
	var emptyRefused atomic.Bool
	cmds := library(t, q.qid, 10, func(cmd string) string {
		if cmd == "empty map" && emptyRefused.Load() {
			return "Can't empty map while MAP is closed"
		}
		return "OK"
	}, stop)

	c := newTestController(key)
	if err := c.OpenMap(10); err != nil {
		t.Fatalf("OpenMap: %v", err)
	}
	if err := c.CloseMap(10); err != nil {
		t.Fatalf("CloseMap: %v", err)
	}
	if err := c.EjectMap(10); err != nil {
		t.Fatalf("EjectMap: %v", err)
	}
	emptyRefused.Store(true)
	err := c.EjectMap(10)
	var re *ReplyError
	if !errors.As(err, &re) || re.Command != "empty map" {
		t.Fatalf("refused eject: err = %v, want ReplyError for empty map", err)
	}
	close(stop)
	want := []string{"open map", "close map", "open map", "empty map", "close map", "open map", "empty map", "close map"}
	if got := <-cmds; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("library received %q, want %q (MAP closed even after a refused empty)", got, want)
	}
}

func TestControllerDiscardsStaleReply(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	// A late reply from library 30 to an earlier, timed-out request.
	deviceReply(t, q.qid, 30, testReply, "MAP Full")

	stop := make(chan struct{})
	defer close(stop)
	library(t, q.qid, 10, func(string) string { return "OK" }, stop)

	c := newTestController(key)
	if err := c.ImportMap(10, "A00001L8"); err != nil {
		t.Fatalf("ImportMap took another device's reply: %v", err)
	}
}

func TestControllerConcurrentCommandsGetOwnReplies(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	stop := make(chan struct{})
	defer close(stop)
	for _, dev := range []int64{10, 30} {
		dev := dev
		library(t, q.qid, dev, func(cmd string) string {
			return cmd + " from " + strconv.FormatInt(dev, 10)
		}, stop)
	}

	c := newTestController(key)
	listMap := func(dev int64) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.command(dev, "list map", true)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, dev := range []int64{10, 30} {
			wg.Add(1)
			go func(dev int64) {
				defer wg.Done()
				reply, err := listMap(dev)
				if want := "list map from " + strconv.FormatInt(dev, 10); err != nil || reply != want {
					errs <- fmt.Errorf("library %d: reply %q, %v; want %q", dev, reply, err, want)
				}
			}(dev)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A reply that arrives after its command timed out must not answer the next
// command to the same library.
func TestControllerDiscardsLateReplyFromSameDevice(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	c := &IPCController{key: key, reply: testReply, timeoutMs: 50}
	if err := c.OpenMap(10); !errors.Is(err, ErrTimeout) {
		t.Fatalf("silent library: err = %v, want ErrTimeout", err)
	}
	deviceRecv(t, q.qid, 10, time.Second) // the timed-out "open map"
	deviceReply(t, q.qid, 10, testReply, "late reply")

	stop := make(chan struct{})
	cmds := library(t, q.qid, 10, func(string) string { return "OK" }, stop)
	c.timeoutMs = 2000
	if err := c.CloseMap(10); err != nil {
		t.Fatalf("CloseMap took the late reply: %v", err)
	}
	close(stop)
	if got := <-cmds; strings.Join(got, "|") != "close map" {
		t.Errorf("library received %q", got)
	}
}

func TestControllerNoReplyTimesOut(t *testing.T) {
	key := uniqueKey()
	createQueue(t, key)
	c := &IPCController{key: key, reply: testReply, timeoutMs: 50}
	if err := c.ImportMap(10, "A00001L8"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("silent library: err = %v, want ErrTimeout", err)
	}
}

// Replies to vtlcmd (and the bay monitor), which share VTLCMD_Q, are neither
// taken as mhvtld's nor consumed: they stay queued for their reader. That
// includes a reply from the very library mhvtld is commanding.
func TestControllerLeavesVtlcmdRepliesAlone(t *testing.T) {
	key := uniqueKey()
	q := createQueue(t, key)
	deviceReply(t, q.qid, 10, VTLCMDQ, "MAP Full")

	stop := make(chan struct{})
	defer close(stop)
	library(t, q.qid, 10, func(string) string { return "OK" }, stop)

	c := newTestController(key)
	if err := c.ImportMap(10, "A00001L8"); err != nil {
		t.Fatalf("ImportMap took vtlcmd's reply: %v", err)
	}
	vtlcmd := &Client{qid: q.qid}
	snd, text, err := vtlcmd.Receive(0)
	if err != nil || snd != 10 || text != "MAP Full" {
		t.Errorf("vtlcmd's reply = %d %q, %v; want it still queued", snd, text, err)
	}
}

func TestReplyType(t *testing.T) {
	if got := ReplyType(1); got != VTLCMDQ+1 {
		t.Errorf("ReplyType(1) = %d", got)
	}
	if NewIPCController().reply != ReplyType(os.Getpid()) {
		t.Error("controller does not reply on its own mtype")
	}
}
