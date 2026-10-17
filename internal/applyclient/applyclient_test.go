package applyclient

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
)

// listen serves a real helper over dir/apply.sock, one Serve per
// connection as the socket unit does, and returns the socket path.
func listen(t *testing.T, cfg apply.Config) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "apply.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h := apply.New(cfg)
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go h.Serve(context.Background(), c)
		}
	}()
	return sock
}

func testConfig(t *testing.T) apply.Config {
	root := t.TempDir()
	cfg := apply.Config{
		ConfigDir:   filepath.Join(root, "etc"),
		StateDir:    filepath.Join(root, "state"),
		RuntimeDir:  filepath.Join(root, "run"),
		LegacyEnv:   filepath.Join(root, "etc", "mhvtl-control.env"),
		Version:     "test",
		Getenv:      func(string) string { return "" },
		Now:         time.Now,
		CheckPeer:   func(*net.UnixConn) (apply.Peer, error) { return apply.Peer{}, nil },
		Journal:     func(map[string]string) {},
		Logf:        t.Logf,
		ReadTimeout: 5 * time.Second,
	}
	for _, d := range []string{cfg.ConfigDir, cfg.StateDir, cfg.RuntimeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestHello(t *testing.T) {
	cfg := testConfig(t)
	c := &Client{Socket: listen(t, cfg)}
	hello := func(want string) {
		t.Helper()
		got, err := c.Hello(context.Background())
		if err != nil || got != want {
			t.Errorf("Hello = %q, %v; want %q", got, err, want)
		}
	}
	hello(Present)

	if err := os.WriteFile(cfg.LegacyEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hello(LegacyEnv)
	os.Remove(cfg.LegacyEnv)

	if err := os.WriteFile(filepath.Join(cfg.StateDir, "intent.json"), []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hello(Incomplete)

	f, err := os.OpenFile(filepath.Join(cfg.RuntimeDir, "exclusive.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	hello(InProgress)
}

func TestAbsent(t *testing.T) {
	missing := &Client{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if got, err := missing.Hello(context.Background()); err != nil || got != Absent {
		t.Errorf("no socket: %q, %v", got, err)
	}
	if _, err := missing.Do(context.Background(), apply.Request{Op: apply.OpPlan}); !errors.Is(err, ErrAbsent) {
		t.Errorf("Do without a socket: %v", err)
	}

	// A socket file nobody listens on (the unit stopped without
	// RemoveOnStop) refuses the connection.
	sock := filepath.Join(t.TempDir(), "stale.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if got, err := (&Client{Socket: sock}).Hello(context.Background()); err != nil || got != Absent {
		t.Errorf("stale socket: %q, %v", got, err)
	}
}

func TestDoRefusal(t *testing.T) {
	c := &Client{Socket: listen(t, testConfig(t))}
	resp, err := c.Do(context.Background(), apply.Request{Op: apply.OpPlan, Files: []apply.File{{Name: "../etc/shadow"}}})
	if err != nil || resp.Result != apply.ResultRefused || resp.Error != apply.ErrName {
		t.Errorf("Do: %+v, %v", resp, err)
	}
}

func TestDoTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "slow.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second) // never answers in time
		}
	}()
	start := time.Now()
	_, err = (&Client{Socket: sock, Timeout: 100 * time.Millisecond}).Do(context.Background(), apply.Request{Op: apply.OpHello})
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("Do on a silent helper: %v after %v", err, time.Since(start))
	}
}
