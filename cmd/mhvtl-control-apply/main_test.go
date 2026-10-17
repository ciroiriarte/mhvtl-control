package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
)

// TestMain runs main itself when re-executed as the helper by a test, with
// the configuration pointed at the directories the test passes.
func TestMain(m *testing.M) {
	if root := os.Getenv("MHVTL_APPLY_TEST_ROOT"); root != "" {
		configure = func(c *apply.Config) {
			c.ConfigDir = filepath.Join(root, "etc")
			c.StateDir = filepath.Join(root, "state")
			c.RuntimeDir = filepath.Join(root, "run")
			c.LegacyEnv = filepath.Join(root, "legacy.env")
			c.CheckPeer = func(*net.UnixConn) (apply.Peer, error) { return apply.Peer{}, nil }
			c.Journal = func(map[string]string) {}
		}
		os.Args = append([]string{os.Args[0]}, strings.Fields(os.Getenv("MHVTL_APPLY_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

// helperCmd returns the test binary set up to run as the helper.
func helperCmd(t *testing.T, args string) *exec.Cmd {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc", "state", "run"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MHVTL_APPLY_TEST_ROOT="+root, "MHVTL_APPLY_TEST_ARGS="+args)
	return cmd
}

// TestSIGPIPE: a client that hangs up before the reply, with the journal
// stream (stderr) broken too, does not kill the helper with SIGPIPE: it
// exits 1 having failed to send the reply. Without signal.Ignore(SIGPIPE)
// the write to the broken stderr would kill it.
func TestSIGPIPE(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	child, parent := os.NewFile(uintptr(fds[0]), "child"), os.NewFile(uintptr(fds[1]), "parent")
	defer child.Close()
	if _, err := parent.Write([]byte(`{"v":1,"op":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	parent.Close() // the client is gone before the helper answers
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR.Close() // and so is the journal stream

	cmd := helperCmd(t, "")
	cmd.Stdin, cmd.Stderr = child, errW
	err = cmd.Run()
	errW.Close()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("helper: %v, want exit 1", err)
	}
	ws := ee.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		t.Fatalf("helper killed by %v", ws.Signal())
	}
	if ws.ExitStatus() != 1 {
		t.Errorf("exit status %d, want 1", ws.ExitStatus())
	}
}

// TestServeOverStdin: the helper answers on the socket it got as stdin.
func TestServeOverStdin(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	child, parent := os.NewFile(uintptr(fds[0]), "child"), os.NewFile(uintptr(fds[1]), "parent")
	defer parent.Close()
	cmd := helperCmd(t, "")
	cmd.Stdin = child
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child.Close()
	parent.Write([]byte(`{"v":1,"op":"hello"}`))
	syscall.Shutdown(int(parent.Fd()), syscall.SHUT_WR)
	parent.SetReadDeadline(time.Now().Add(10 * time.Second))
	var out bytes.Buffer
	out.ReadFrom(parent)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v (%s)", err, stderr.String())
	}
	if !strings.Contains(out.String(), `"state":"ready"`) {
		t.Errorf("reply %q", out.String())
	}
}

func TestStdinNotSocket(t *testing.T) {
	cmd := helperCmd(t, "")
	cmd.Stdin = strings.NewReader("")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Errorf("stdin not a socket: %v, want exit 2", err)
	}
}

func TestRecoverBootFlag(t *testing.T) {
	cmd := helperCmd(t, "--recover-boot")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "no interrupted apply") {
		t.Errorf("--recover-boot: %v %s", err, out)
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, nil, &out, nil); code != 0 || !strings.HasPrefix(out.String(), "mhvtl-control-apply ") {
		t.Errorf("--version: %d %q", code, out.String())
	}
}
