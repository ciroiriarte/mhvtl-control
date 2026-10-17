package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/validate"
)

func write(t *testing.T, dir, name, data string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRequestTakesBasesFromConfigDir(t *testing.T) {
	conf, next := t.TempDir(), t.TempDir()
	write(t, conf, "device.conf", "VERSION: 5\n")
	write(t, conf, "library_contents.30", "VERSION: 2\n")
	dc := write(t, next, "device.conf", "VERSION: 5\n\nLibrary: 40\n")
	lc := write(t, next, "library_contents.40", "VERSION: 2\nSlot 1: F40001L8\n")
	disp := write(t, t.TempDir(), "disp.json", `[{"library":10,"barcode":"E01001L8","to":"shelf"}]`)

	var out bytes.Buffer
	err := run([]string{"request", "-config-dir", conf, "-delete", "library_contents.30",
		"-dispositions", disp, "-actor", "smoke", dc, lc}, &out)
	if err != nil {
		t.Fatal(err)
	}
	var req apply.Request
	if err := json.Unmarshal(out.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	want := apply.Request{
		Files: []apply.File{
			{Name: "device.conf", BaseSHA256: conffile.Hash([]byte("VERSION: 5\n")), Content: []byte("VERSION: 5\n\nLibrary: 40\n")},
			{Name: "library_contents.40", BaseSHA256: "", Content: []byte("VERSION: 2\nSlot 1: F40001L8\n")},
		},
		Delete:       []apply.Delete{{Name: "library_contents.30", BaseSHA256: conffile.Hash([]byte("VERSION: 2\n"))}},
		Dispositions: []validate.Disposition{{Library: 10, Barcode: "E01001L8", To: "shelf"}},
		Actor:        "smoke",
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request\n got %+v\nwant %+v", req, want)
	}
}

func TestRequestDeleteOfMissingFileFails(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"request", "-config-dir", t.TempDir(), "-delete", "library_contents.40"}, &out); err == nil {
		t.Fatal("deleting a file that is not there built a request")
	}
}

// serve answers one connection on a socket in a temp dir with resp, and
// sends the decoded request on the returned channel.
func serve(t *testing.T, resp apply.Response) (string, <-chan *apply.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "apply.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan *apply.Request, 1)
	go func() {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := apply.DecodeRequest(c)
		if err != nil {
			t.Error(err)
		}
		got <- req
		b, _ := json.Marshal(resp)
		c.Write(b)
	}()
	return sock, got
}

func TestSendSetsOpUnitsAndForce(t *testing.T) {
	dir := t.TempDir()
	reqFile := write(t, dir, "req.json", `{"files":[{"name":"device.conf","base_sha256":"","content":"VkVSU0lPTjogNQo="}],"actor":"smoke"}`)
	plan := write(t, dir, "plan.json", `{"v":1,"result":"refused","error":"drive_loaded","units":["vtllibrary@40.service","vtltape@41.service"],"force_token":"tok"}`)
	sock, got := serve(t, apply.Response{Result: apply.ResultApplied})

	var out bytes.Buffer
	err := run([]string{"send", "-socket", sock, "-op", "apply", "-units-from", plan, "-force-from", plan, reqFile}, &out)
	if err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req.Op != apply.OpApply || req.V != apply.Version || req.Actor != "smoke" {
		t.Fatalf("request %+v", req)
	}
	if want := []string{"vtllibrary@40.service", "vtltape@41.service"}; !reflect.DeepEqual(req.ExpectUnits, want) {
		t.Fatalf("expect_units %v, want %v", req.ExpectUnits, want)
	}
	if req.ForceToken != "tok" {
		t.Fatalf("force_token %q", req.ForceToken)
	}
	if string(req.Files[0].Content) != "VERSION: 5\n" {
		t.Fatalf("content %q", req.Files[0].Content)
	}
	var resp apply.Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.Result != apply.ResultApplied {
		t.Fatalf("printed %q (%v)", out.String(), err)
	}
}

func TestSendHelloNeedsNoRequestFile(t *testing.T) {
	sock, got := serve(t, apply.Response{Result: apply.ResultOK, State: apply.StateReady})
	var out bytes.Buffer
	if err := run([]string{"send", "-socket", sock, "-op", "hello"}, &out); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req.Op != apply.OpHello || len(req.Files) != 0 {
		t.Fatalf("request %+v", req)
	}
	if !strings.Contains(out.String(), `"state":"ready"`) {
		t.Fatalf("printed %q", out.String())
	}
}

func TestSendWithoutHelperFails(t *testing.T) {
	var out bytes.Buffer
	sock := filepath.Join(t.TempDir(), "none.sock")
	if err := run([]string{"send", "-socket", sock, "-op", "hello"}, &out); err == nil {
		t.Fatal("no error without a listener")
	}
}
