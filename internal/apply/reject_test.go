package apply

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/validate"
)

// The refusals of AC 26 and plan 3.2: every request below is refused, and
// no file changes.

func TestDecodeRejects(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxFileSize+1))
	var many []string
	for i := 0; i <= MaxFiles; i++ {
		many = append(many, fmt.Sprintf(`{"name":"library_contents.%d","base_sha256":"","content":""}`, i))
	}
	cases := []struct {
		name, body, code string
	}{
		{"unknown field", `{"v":1,"op":"plan","extra":1}`, ErrBadRequest},
		{"unknown nested field", `{"v":1,"op":"plan","files":[{"name":"device.conf","base_sha256":"","content":"","mode":420}]}`, ErrBadRequest},
		{"unknown disposition field", `{"v":1,"op":"plan","dispositions":[{"library":1,"barcode":"A","to":"shelf","why":1}]}`, ErrBadRequest},
		{"version", `{"v":2,"op":"plan"}`, ErrBadRequest},
		{"op", `{"v":1,"op":"exec"}`, ErrBadRequest},
		{"trailing data", `{"v":1,"op":"plan"} {"v":1,"op":"apply"}`, ErrBadRequest},
		{"not json", `v=1`, ErrBadRequest},
		{"oversize request", `{"v":1,"op":"plan","actor":"` + strings.Repeat("a", MaxRequest) + `"}`, ErrOversize},
		{"oversize file", `{"v":1,"op":"plan","files":[{"name":"device.conf","base_sha256":"","content":"` + big + `"}]}`, ErrOversize},
		{"too many files", `{"v":1,"op":"plan","files":[` + strings.Join(many, ",") + `]}`, ErrOversize},
		{"actor too long", `{"v":1,"op":"plan","actor":"` + strings.Repeat("a", maxActor+1) + `"}`, ErrBadRequest},
		{"actor control character", `{"v":1,"op":"plan","actor":"a\nb"}`, ErrBadRequest},
	}
	for _, c := range cases {
		_, err := DecodeRequest(strings.NewReader(c.body))
		var r *refusal
		if !errors.As(err, &r) || r.code != c.code {
			t.Errorf("%s: err %v, want refusal %s", c.name, err, c.code)
		}
	}
	if _, err := DecodeRequest(strings.NewReader(`{"v":1,"op":"hello"}` + "\n\t ")); err != nil {
		t.Errorf("trailing blanks: %v", err)
	}
}

// refused runs req and checks it is refused with code and leaves the
// configuration as it was.
func (e *env) refused(name string, req Request, code string) Response {
	e.t.Helper()
	snapshot := func() map[string]string {
		m := map[string]string{}
		ents, _ := os.ReadDir(e.dir)
		for _, n := range ents {
			if !n.Type().IsRegular() {
				m[n.Name()] = n.Type().String()
				continue
			}
			if b, err := os.ReadFile(filepath.Join(e.dir, n.Name())); err == nil {
				m[n.Name()] = conffile.Hash(b)
			}
		}
		return m
	}
	before := snapshot()
	resp := e.do(req)
	if resp.Result != ResultRefused || resp.Error != code {
		e.t.Errorf("%s: %+v, want refused %s", name, resp, code)
	}
	if after := snapshot(); fmt.Sprint(after) != fmt.Sprint(before) {
		e.t.Errorf("%s: the configuration changed: %v, was %v", name, after, before)
	}
	return resp
}

func TestRejectNames(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{
		"../device.conf", "/etc/mhvtl/device.conf", "sub/device.conf", "..", ".",
		"mhvtl-control.env", "library_contents.x", "library_contents.10.persist",
		"library_contents.010", "library_contents.123456", "library_contents.", "Device.conf",
	} {
		e.refused(name, Request{Op: OpPlan, Files: []File{{Name: name, Content: []byte("x")}}}, ErrName)
	}
	dc := e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	e.refused("named twice", Request{Op: OpPlan, Files: []File{dc, dc}}, ErrName)
	e.refused("written and deleted", Request{Op: OpPlan, Files: []File{dc}, Delete: []Delete{{Name: "device.conf", BaseSHA256: dc.BaseSHA256}}}, ErrName)
	e.refused("delete device.conf", Request{Op: OpPlan, Delete: []Delete{{Name: "device.conf", BaseSHA256: e.sha("device.conf")}}}, ErrName)
	e.refused("delete mhvtl.conf", Request{Op: OpPlan, Delete: []Delete{{Name: "mhvtl.conf", BaseSHA256: e.sha("mhvtl.conf")}}}, ErrName)
	e.refused("delete without base", Request{Op: OpPlan, Delete: []Delete{{Name: "library_contents.10"}}}, ErrBadRequest)
	e.refused("malformed base", Request{Op: OpPlan, Files: []File{{Name: "device.conf", BaseSHA256: strings.ToUpper(dc.BaseSHA256), Content: dc.Content}}}, ErrBadRequest)
	e.refused("contents of no library", Request{Op: OpPlan, Files: []File{{Name: "library_contents.99", Content: []byte("VERSION: 2\n")}}}, ErrName)
	e.refused("delete a kept library's contents", Request{Op: OpPlan, Delete: []Delete{{Name: "library_contents.10", BaseSHA256: e.sha("library_contents.10")}}}, ErrName)
}

func TestRejectNonRegularTargets(t *testing.T) {
	e := newEnv(t)
	dc := e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	elsewhere := filepath.Join(t.TempDir(), "device.conf")
	if err := os.WriteFile(elsewhere, e.read("device.conf"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.dir, "device.conf")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	e.refused("symlinked device.conf", Request{Op: OpPlan, Files: []File{dc}}, ErrName)
	if b, _ := os.ReadFile(elsewhere); conffile.Hash(b) != dc.BaseSHA256 {
		t.Error("the symlink's target changed")
	}

	// A symlinked library_contents is refused even when only read.
	e = newEnv(t)
	dc = e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	lc := filepath.Join(e.dir, "library_contents.30")
	os.Remove(lc)
	if err := os.Symlink("/etc/shadow", lc); err != nil {
		t.Fatal(err)
	}
	e.refused("symlinked library_contents read", Request{Op: OpPlan, Files: []File{dc}}, ErrName)

	e = newEnv(t)
	dc = e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	lc = filepath.Join(e.dir, "library_contents.30")
	os.Remove(lc)
	if err := syscall.Mkfifo(lc, 0o644); err != nil {
		t.Fatal(err)
	}
	e.refused("FIFO", Request{Op: OpPlan, Files: []File{dc}}, ErrName)
}

func TestRejectUnits(t *testing.T) {
	e := newEnv(t)
	for _, u := range []string{
		"foo@1.service", "vtllibrary@1.service; x", "vtllibrary@.service", "vtllibrary@x.service",
		"vtllibrary@123456.service", "vtltape@1.socket", "../vtltape@1.service", "vtllibrary@1.service\n",
		"mhvtl.target", "vtllibrary@1",
	} {
		e.refused(u, Request{Op: OpPlan, ExpectUnits: []string{u}}, ErrUnit)
	}
	e.refused("listed twice", Request{Op: OpPlan, ExpectUnits: []string{"vtltape@11.service", "vtltape@11.service"}}, ErrUnit)
	// A drive stanza edit restarts the drive and its library (amendment 1);
	// expect_units must name exactly those (AC 26).
	// A plan needs no expect_units: it reports the units, which the daemon
	// echoes on the apply.
	dc := e.edit("device.conf", "Compression type: lzo", "Compression type: zlib")
	want := []string{"vtllibrary@10.service", "vtltape@11.service"}
	resp := e.do(Request{Op: OpPlan, Files: []File{dc}})
	if resp.Result != ResultValidated || fmt.Sprint(resp.Units) != fmt.Sprint(want) {
		t.Errorf("template instances: %+v", resp)
	}
	for name, units := range map[string][]string{
		"none":                {},
		"library only":        {"vtllibrary@10.service"},
		"a unit not affected": {"vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service"},
		"another library":     {"vtllibrary@30.service", "vtltape@11.service"},
	} {
		e.refused(name, Request{Op: OpApply, Files: []File{dc}, ExpectUnits: units}, ErrUnit)
	}
	e.refused("units for a comment edit", Request{Op: OpApply, Files: []File{e.edit("device.conf", "# Each 'record'", "# Every 'record'")},
		ExpectUnits: []string{"vtllibrary@10.service"}}, ErrUnit)
}

func TestRejectHashAtRequest(t *testing.T) {
	e := newEnv(t)
	dc := e.edit("device.conf", "# Each 'record'", "# Every 'record'")
	dc.BaseSHA256 = conffile.Hash([]byte("something else"))
	resp := e.do(Request{Op: OpPlan, Files: []File{dc}})
	if resp.Result != ResultConflict || resp.CurrentSHA256 == nil || *resp.CurrentSHA256 != e.sha("device.conf") ||
		resp.File != filepath.Join(e.dir, "device.conf") {
		t.Errorf("stale base: %+v", resp)
	}
	resp = e.do(Request{Op: OpPlan, Files: []File{{Name: "library_contents.10", Content: []byte("VERSION: 2\n")}}})
	if resp.Result != ResultConflict {
		t.Errorf("create over an existing file: %+v", resp)
	}
	resp = e.do(Request{Op: OpApply, Delete: []Delete{{Name: "library_contents.30", BaseSHA256: dc.BaseSHA256}}})
	if resp.Result != ResultConflict {
		t.Errorf("stale delete: %+v", resp)
	}
}

// TestRejectHashAtRename: a file changed after the request-time check is
// caught by the re-hash right before its rename, or before a delete; the
// files already written are restored and the intent is gone.
func TestRejectHashAtRename(t *testing.T) {
	for _, del := range []bool{false, true} {
		e := newEnv(t)
		lc := e.edit("library_contents.10", "Slot 1:", "Slot 1: ")
		var req Request
		var target string
		if del {
			req = Request{Op: OpApply, Files: []File{lc}, Delete: []Delete{{Name: "library_contents.30", BaseSHA256: e.sha("library_contents.30")}}}
			target = "library_contents.30"
		} else {
			req = Request{Op: OpApply, Files: []File{lc, e.edit("device.conf", "# Each 'record'", "# Every 'record'")}}
			target = "device.conf"
		}
		files := newFileSet(e.dir)
		if err := checkBases(&req, files); err != nil {
			t.Fatal(err)
		}
		hand := []byte("# hand edit\n")
		e.h.beforeWrite = func(name string) {
			if name == target {
				os.WriteFile(filepath.Join(e.dir, target), hand, 0o644)
			}
		}
		err := e.h.writeFiles(context.Background(), &Intent{V: Version}, changesOf(&req), files)
		if c, ok := conflictOf(err); !ok || c.Current != conffile.Hash(hand) {
			t.Errorf("delete %v: err %v, want a conflict with the hand edit", del, err)
		}
		if got := e.read(target); !bytes.Equal(got, hand) {
			t.Errorf("delete %v: hand edit overwritten: %q", del, got)
		}
		if e.sha("library_contents.10") != lc.BaseSHA256 {
			t.Errorf("delete %v: library_contents.10 not restored", del)
		}
		if _, err := os.Stat(e.h.intentPath()); !os.IsNotExist(err) {
			t.Errorf("delete %v: intent left: %v", del, err)
		}
	}
}

func TestRejectPeer(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, cgroup string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "cgroup"), []byte(cgroup), 0o644)
		os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(pid+" (mhvtl control) S 1 1 1 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 98765 1 1"), 0o644)
	}
	write("10", "0::/system.slice/mhvtl-control.service\n")
	write("11", "0::/system.slice/sshd.service\n")
	write("12", "0::/system.slice/evil.service/mhvtl-control.service\n")
	write("13", "0::/system.slice/evil-mhvtl-control.service\n")
	write("14", "12:pids:/system.slice/x.service\n1:name=systemd:/system.slice/mhvtl-control.service\n")
	write("15", "0::/user.slice/user-1000.slice/session-2.scope/mhvtl-control.service\n")
	alive := func() error { return nil }
	cases := []struct {
		pid int32
		uid uint32
		ok  bool
	}{
		{10, 0, true},
		{10, 1000, false}, // not root
		{11, 0, false},    // another unit
		{12, 0, false},    // a sub-cgroup named after the unit
		{13, 0, false},
		{14, 0, true}, // legacy hierarchy
		{15, 0, false},
		{16, 0, false}, // gone
	}
	for _, c := range cases {
		_, err := verifyPeer(proc, c.pid, c.uid, alive)
		if (err == nil) != c.ok {
			t.Errorf("pid %d uid %d: err %v, want ok %v", c.pid, c.uid, err, c.ok)
		}
	}
	if _, err := verifyPeer(proc, 10, 0, func() error { return errors.New("peer exited") }); err == nil {
		t.Error("a peer that exited during the check passed")
	}
	// Without a pidfd the start time is read twice.
	if _, err := verifyPeer(proc, 10, 0, func() error {
		write("10", "0::/system.slice/mhvtl-control.service\n")
		os.WriteFile(filepath.Join(proc, "10", "stat"), []byte("10 (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 11111 1 1"), 0o644)
		return errNoPIDFD
	}); err == nil {
		t.Error("a reused pid passed the start-time recheck")
	}

	// The real check on a socket pair: this test is not root, or not in
	// mhvtl-control.service, so it is refused either way.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "a")
	defer f.Close()
	defer syscall.Close(fds[1])
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := CheckPeer(c.(*net.UnixConn))
	if err == nil {
		t.Errorf("CheckPeer accepted the test process: %+v", p)
	}
	if p.PID != int32(os.Getpid()) || p.UID != uint32(os.Getuid()) {
		t.Errorf("SO_PEERCRED gave %+v, want pid %d uid %d", p, os.Getpid(), os.Getuid())
	}

	// Through Serve: a refused peer gets a refusal and an audit record.
	e := newEnv(t)
	e.peerErr = errors.New("peer uid 1000, not 0")
	e.rebuild()
	resp := serveOnce(t, e.h, `{"v":1,"op":"hello"}`)
	if resp.Result != ResultRefused || resp.Error != ErrPeer {
		t.Errorf("Serve: %+v", resp)
	}
	if l := e.auditLines(); len(l) != 1 || !strings.Contains(l[0], `"error":"peer"`) {
		t.Errorf("audit: %v", l)
	}
}

func TestRejectLegacyEnv(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(e.legacy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.refused("legacy env", Request{Op: OpPlan, Files: []File{e.edit("device.conf", "# Each 'record'", "# Every 'record'")}}, ErrLegacyEnv)
	// A dangling symlink counts: Lstat, not Stat.
	os.Remove(e.legacy)
	os.Symlink("/nonexistent", e.legacy)
	e.refused("legacy env symlink", Request{Op: OpPlan}, ErrLegacyEnv)
}

func TestRejectConfigPath(t *testing.T) {
	e := newEnv(t)
	e.cfg.Getenv = func(k string) string {
		if k == "MHVTL_CONFIG_PATH" {
			return "/srv/other"
		}
		return ""
	}
	e.rebuild()
	e.refused("environment", Request{Op: OpPlan}, ErrConfigPath)
	e.cfg.Getenv = func(k string) string {
		if k == "MHVTL_CONFIG_PATH" {
			return e.dir + "/"
		}
		return ""
	}
	e.rebuild()
	if resp := e.do(Request{Op: OpPlan}); resp.Result != ResultValidated {
		t.Errorf("environment naming --config-dir: %+v", resp)
	}

	e = newEnv(t)
	e.refused("proposed mhvtl.conf", Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", "CAPACITY=500", "CAPACITY=500\nMHVTL_CONFIG_PATH=/srv/other")}}, ErrConfigPath)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte("MHVTL_CONFIG_PATH=/srv/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.refused("current mhvtl.conf", Request{Op: OpPlan}, ErrConfigPath)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte("MHVTL_CONFIG_PATH = /srv/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.refused("mhvtl.conf mhVTL reads unlike the shell", Request{Op: OpPlan}, ErrNotWritable)
}

// TestMhvtlConfDerived: make_vtl_media sources mhvtl.conf as bash, as
// root, so the new file must be the current one with only editable values
// changed. Each probe would run code, or set a key, if let through.
func TestMhvtlConfDerived(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ old, new, code string }{
		{"CAPACITY=500", "CAPACITY=500\ntouch /tmp/pwned", ErrMhvtlConfDerived},
		{"CAPACITY=500", "touch /tmp/pwned\nCAPACITY=500", ErrMhvtlConfDerived},
		{"CAPACITY=500", "CAPACITY=$(touch /tmp/pwned)", ErrMhvtlConfValue},
		{"VERBOSE=1", "VERBOSE=1;touch /tmp/pwned", ErrMhvtlConfValue},
		{"VTL_DEBUG=0", "VTL_DEBUG=`touch /tmp/pwned`", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=500\nrm = -rf", ErrMhvtlConfDerived},
		{"MHVTL_HOME_PATH=" + e.home, "MHVTL_HOME_PATH=$(id)", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=500\nDAEMON_DEBUG=-d", ErrMhvtlConfDerived},
		{"CAPACITY=500", "CAPACITY=500\nMHVTL_CONFIG_PATH=" + e.dir, ErrMhvtlConfDerived},
		{"# Default media capacity", "# Default capacity", ErrMhvtlConfDerived},
		{"CAPACITY=500", "CAPACITY='500'", ErrMhvtlConfDerived},
		{"CAPACITY=500", "CAPACITY=500 ", ErrMhvtlConfDerived},
		{"CAPACITY=500", "CAPACITY=0", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=0600", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=00", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=4294967297", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=99999999999999999999", ErrMhvtlConfValue},
		{"CAPACITY=500", "CAPACITY=5e2", ErrMhvtlConfValue},
		{"VERBOSE=1", "VERBOSE=4", ErrMhvtlConfValue},
		{"VTL_DEBUG=0", "VTL_DEBUG=2", ErrMhvtlConfValue},
		{"CAPACITY=500\n", "", ErrMhvtlConfKey},
		{"MHVTL_HOME_PATH=" + e.home, "MHVTL_HOME_PATH=/etc/mhvtl", ErrMhvtlConfValue},
		{"MHVTL_HOME_PATH=" + e.home, "MHVTL_HOME_PATH=tapes", ErrMhvtlConfValue},
	} {
		e.refused(tc.new, Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", tc.old, tc.new)}}, tc.code)
	}
	for _, tc := range []struct {
		old, new string
		units    []string
	}{
		{"CAPACITY=500", "CAPACITY=1000", nil},
		{"CAPACITY=500", "CAPACITY=4294967296", nil},
		{"VERBOSE=1", "VERBOSE=3", shippedUnits},
		{"VTL_DEBUG=0", "VTL_DEBUG=1", nil},
	} {
		if resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", tc.old, tc.new)}, ExpectUnits: tc.units}); resp.Result != ResultValidated {
			t.Errorf("%q: %+v", tc.new, resp)
		}
	}
	// A key the file lacks is appended, as conffile's Set does.
	b := e.read("mhvtl.conf")
	cur := strings.Replace(string(b), "VTL_DEBUG=0\n", "", 1)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(cur), 0o644); err != nil {
		t.Fatal(err)
	}
	add := File{Name: "mhvtl.conf", BaseSHA256: conffile.Hash([]byte(cur)), Content: []byte(cur + "VTL_DEBUG=1\n")}
	if resp := e.do(Request{Op: OpPlan, Files: []File{add}}); resp.Result != ResultValidated {
		t.Errorf("appended VTL_DEBUG: %+v", resp)
	}
}

func TestRejectHomeChange(t *testing.T) {
	e := newEnv(t)
	// Both shipped libraries share the home.
	e.refused("shared home", Request{Op: OpPlan, Files: []File{e.edit("device.conf", " Home directory: "+e.home+"\n PERSIST: False\n Backoff: 400", " Home directory: /srv/new10\n PERSIST: False\n Backoff: 400")}}, ErrHomeInUse)

	// Library 30 alone in its home, which holds a cartridge.
	own := filepath.Join(t.TempDir(), "home30")
	b := string(e.read("device.conf"))
	i := strings.Index(b, "Library: 30")
	b = b[:i] + strings.Replace(b[i:], " Home directory: "+e.home, " Home directory: "+own, 1)
	if err := os.WriteFile(filepath.Join(e.dir, "device.conf"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(own, "G03001TA"), 0o755); err != nil {
		t.Fatal(err)
	}
	move := e.edit("device.conf", " Home directory: "+own, " Home directory: /srv/new30")
	e.refused("home with a cartridge", Request{Op: OpPlan, Files: []File{move}}, ErrHomeInUse)
	os.Remove(filepath.Join(own, "G03001TA"))
	if err := os.Symlink("/dev/sdz", filepath.Join(own, "R00001L8")); err != nil {
		t.Fatal(err)
	}
	e.refused("home with a raw cartridge", Request{Op: OpPlan, Files: []File{move}}, ErrHomeInUse)
	os.Remove(filepath.Join(own, "R00001L8"))
	if resp := e.do(Request{Op: OpPlan, Files: []File{move}}); resp.Error == ErrHomeInUse {
		t.Errorf("empty home: %+v", resp)
	}
}

// TestRejectHomeForbidden: a media home the change sets, in device.conf
// for an old or a new library, or as MHVTL_HOME_PATH, must be an absolute
// clean path outside system trees and the helper's state (S1).
func TestRejectHomeForbidden(t *testing.T) {
	e := newEnv(t)
	links := t.TempDir()
	for name, target := range map[string]string{"usr": "/usr", "root": "/", "dangling": filepath.Join(links, "missing")} {
		if err := os.Symlink(target, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	bad := []string{"relative/dir", "/proc/sys/kernel", "/var/lib/mhvtl-control-apply", "/var/lib/mhvtl-control-apply/rollback", "/etc/mhvtl", "/x/../etc/cron.d", "/",
		"/lib/systemd/system", "/home/tapes",
		filepath.Join(links, "usr"), filepath.Join(links, "usr/tapes/new"), filepath.Join(links, "root/etc"), filepath.Join(links, "dangling/tapes")}
	for _, h := range bad {
		e.refused("library 10 at "+h, Request{Op: OpPlan, Files: []File{e.edit("device.conf", " Home directory: "+e.home, " Home directory: "+h)}}, ErrHomeForbidden)
		e.refused("MHVTL_HOME_PATH="+h, Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", "MHVTL_HOME_PATH="+e.home, "MHVTL_HOME_PATH="+h)}}, ErrMhvtlConfValue)
	}
	for _, h := range []string{"usr", "usr/tapes/new", "root/etc"} {
		if p := homeProblem(filepath.Join(links, h)); !strings.HasPrefix(p, "resolves to /") {
			t.Errorf("%s: %q", h, p)
		}
	}
	// An ancestor that cannot be looked at is refused, not climbed past.
	if os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o755) })
		if p := homeProblem(filepath.Join(locked, "inner/tapes")); !strings.HasPrefix(p, "cannot resolve "+filepath.Join(locked, "inner/tapes")) {
			t.Errorf("home under a 000 directory: %q", p)
		}
	}
	// Library 30 renumbered 31: a new library.
	b := string(e.read("device.conf"))
	i := strings.Index(b, "Library: 30")
	renum := b[:i] + strings.ReplaceAll(strings.Replace(b[i:], "Library: 30", "Library: 31", 1), "Library ID: 30", "Library ID: 31")
	for _, h := range bad {
		next := renum[:i] + strings.Replace(renum[i:], " Home directory: "+e.home, " Home directory: "+h, 1)
		e.refused("new library 31 at "+h, Request{Op: OpPlan, Files: []File{{Name: "device.conf", BaseSHA256: conffile.Hash([]byte(b)), Content: []byte(next)}}}, ErrHomeForbidden)
	}
	for _, h := range []string{"/opt/mhvtl", "/srv/tapes", filepath.Join(links, "root/srv/tapes")} {
		if resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", "MHVTL_HOME_PATH="+e.home, "MHVTL_HOME_PATH="+h)}}); resp.Result != ResultValidated {
			t.Errorf("MHVTL_HOME_PATH=%s: %+v", h, resp)
		}
		next := renum[:i] + strings.Replace(renum[i:], " Home directory: "+e.home, " Home directory: "+h, 1)
		if resp := e.do(Request{Op: OpPlan, Files: []File{{Name: "device.conf", BaseSHA256: conffile.Hash([]byte(b)), Content: []byte(next)}}}); resp.Error == ErrHomeForbidden {
			t.Errorf("new library 31 at %s: %+v", h, resp)
		}
	}
}

// A forbidden MHVTL_HOME_PATH already in force does not block an
// unrelated change: validate reports it as a pre-existing warning, as the
// daemon's validation does.
func TestPreExistingDefaultHome(t *testing.T) {
	e := newEnv(t)
	b := strings.Replace(string(e.read("mhvtl.conf")), "MHVTL_HOME_PATH="+e.home, "MHVTL_HOME_PATH=/home/tapes", 1)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("device.conf", "# Each 'record'", "# Every 'record'")}})
	if resp.Result != ResultValidated || resp.Findings == nil {
		t.Fatalf("plan: %+v", resp)
	}
	found := false
	for _, f := range resp.Findings.Warnings {
		found = found || f.Code == validate.CodeMediaHomeForbidden && f.Tag == validate.TagPreExisting
	}
	if !found {
		t.Errorf("no pre-existing media.home_forbidden warning: %+v", resp.Findings.Warnings)
	}
}

func TestRejectNotWritable(t *testing.T) {
	e := newEnv(t)
	e.refused("duplicate stanza", Request{Op: OpPlan, Files: []File{e.edit("device.conf", "Library: 30 CHANNEL: 00 TARGET: 08 LUN: 00", "Library: 10 CHANNEL: 00 TARGET: 08 LUN: 00")}}, ErrNotWritable)
}

func TestRejectInvalid(t *testing.T) {
	e := newEnv(t)
	resp := e.refused("duplicate serial", Request{Op: OpApply, Files: []File{e.edit("device.conf", "Unit serial number: XYZZY_A2", "Unit serial number: XYZZY_A1")}}, ErrInvalid)
	if resp.Findings == nil || len(resp.Findings.Errors) == 0 {
		t.Errorf("no findings: %+v", resp)
	}
}

func TestRejectForceToken(t *testing.T) {
	e := newEnv(t)
	e.refused("force", Request{Op: OpPlan, ForceToken: "x"}, ErrNotImplemented)
}

// TestPeerPIDFDErrors: CheckPeer falls back to the start-time recheck only
// when the kernel lacks SO_PEERPIDFD (ENOPROTOOPT); any other error from it
// is refused (S5).
func TestPeerPIDFDErrors(t *testing.T) {
	defer func(f func(int) (int, syscall.Errno)) { getsockoptPIDFD = f }(getsockoptPIDFD)
	for _, e := range []syscall.Errno{syscall.EINVAL, syscall.ENODATA, syscall.EPERM, syscall.EBADF} {
		getsockoptPIDFD = func(int) (int, syscall.Errno) { return -1, e }
		a, _ := socketPair(t)
		if _, err := CheckPeer(a); err == nil || !strings.Contains(err.Error(), "SO_PEERPIDFD") || !errors.Is(err, e) {
			t.Errorf("%v: %v", e, err)
		}
	}
	getsockoptPIDFD = func(int) (int, syscall.Errno) { return -1, syscall.ENOPROTOOPT }
	a, _ := socketPair(t)
	if _, err := CheckPeer(a); err == nil || strings.Contains(err.Error(), "SO_PEERPIDFD") {
		// The fallback goes on to the uid and cgroup checks, which this
		// test process fails.
		t.Errorf("ENOPROTOOPT: %v", err)
	}
}
