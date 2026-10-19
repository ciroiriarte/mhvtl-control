package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/scsi"
)

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// fakeChangers is a ChangerService that records calls and returns canned
// results.
type fakeChangers struct {
	mu     sync.Mutex // Inventory runs for several libraries at once
	calls  []string
	inv    changer.Inventory
	invErr error
	move   changer.Move
	err    error
	// storeErr is Store's result, so a place flow can fail after imports.
	storeErr error
	// invByLib overrides inv for the libraries it names.
	invByLib map[int]changer.Inventory
}

func (f *fakeChangers) Inventory(lib config.Library) (changer.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("Inventory lib=%d", lib.ID))
	if inv, ok := f.invByLib[lib.ID]; ok {
		return inv, f.invErr
	}
	return f.inv, f.invErr
}

func (f *fakeChangers) TryInventory(lib config.Library) (changer.Inventory, error) {
	return f.Inventory(lib)
}

func (f *fakeChangers) Mount(lib config.Library, driveID int, from changer.Source) (changer.Move, error) {
	f.calls = append(f.calls, fmt.Sprintf("Mount lib=%d drive=%d slot=%d map=%d", lib.ID, driveID, from.Slot, from.MAP))
	return f.move, f.err
}

func (f *fakeChangers) Unmount(lib config.Library, driveID, toSlot int) (changer.Move, error) {
	f.calls = append(f.calls, fmt.Sprintf("Unmount lib=%d drive=%d slot=%d", lib.ID, driveID, toSlot))
	return f.move, f.err
}

func (f *fakeChangers) Transfer(lib config.Library, from, to changer.Location) (changer.Move, error) {
	f.calls = append(f.calls, fmt.Sprintf("Transfer lib=%d %s%d->%s%d", lib.ID, from.Kind, from.Number, to.Kind, to.Number))
	return f.move, f.err
}

func (f *fakeChangers) Export(lib config.Library, from changer.ExportSource) (changer.Move, error) {
	f.calls = append(f.calls, fmt.Sprintf("Export lib=%d slot=%d drive=%d barcode=%s", lib.ID, from.Slot, from.Drive, from.Barcode))
	return f.move, f.err
}

func (f *fakeChangers) Store(lib config.Library, barcode string) (changer.Move, error) {
	f.calls = append(f.calls, fmt.Sprintf("Store lib=%d %s", lib.ID, barcode))
	return f.move, f.storeErr
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error.Code
}

func TestMountUsesChanger(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"drive":11,"slot":3}`, "Mount lib=10 drive=11 slot=3 map=0"},
		{`{"drive":12,"map":2}`, "Mount lib=10 drive=12 slot=0 map=2"},
	}
	for _, c := range cases {
		fc := &fakeChangers{move: changer.Move{From: changer.Location{Kind: "slot", Number: 3}, To: changer.Location{Kind: "drive", Number: 1}}}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		rec := postJSON(t, h, "/api/v1/libraries/10/mount", c.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", c.body, rec.Code, rec.Body.String())
		}
		if len(fc.calls) != 1 || fc.calls[0] != c.want {
			t.Errorf("%s: calls = %q, want [%q]", c.body, fc.calls, c.want)
		}
		if !strings.Contains(rec.Body.String(), `"move":{"from":{"kind":"slot","number":3}`) {
			t.Errorf("%s: body %s lacks the move", c.body, rec.Body.String())
		}
	}
}

func TestMountValidation(t *testing.T) {
	cases := []struct {
		name, path, body string
		status           int
	}{
		{"neither slot nor map", "/api/v1/libraries/10/mount", `{"drive":11}`, http.StatusBadRequest},
		{"both slot and map", "/api/v1/libraries/10/mount", `{"drive":11,"slot":1,"map":1}`, http.StatusBadRequest},
		{"drive of another library", "/api/v1/libraries/10/mount", `{"drive":31,"slot":1}`, http.StatusBadRequest},
		{"bad json", "/api/v1/libraries/10/mount", `{`, http.StatusBadRequest},
		{"unknown library", "/api/v1/libraries/99/mount", `{"drive":11,"slot":1}`, http.StatusNotFound},
		{"non-numeric library", "/api/v1/libraries/x/mount", `{"drive":11,"slot":1}`, http.StatusBadRequest},
		{"unmount drive of another library", "/api/v1/libraries/10/unmount", `{"drive":31}`, http.StatusBadRequest},
		{"unmount negative slot", "/api/v1/libraries/10/unmount", `{"drive":11,"slot":-1}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		fc := &fakeChangers{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		rec := postJSON(t, h, c.path, c.body)
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d (body %s)", c.name, rec.Code, c.status, rec.Body.String())
		}
		if len(fc.calls) != 0 {
			t.Errorf("%s: changer called for an invalid request: %q", c.name, fc.calls)
		}
	}
}

func TestUnmountUsesChanger(t *testing.T) {
	for body, want := range map[string]string{
		`{"drive":11}`:          "Unmount lib=10 drive=11 slot=0",
		`{"drive":12,"slot":7}`: "Unmount lib=10 drive=12 slot=7",
	} {
		fc := &fakeChangers{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		rec := postJSON(t, h, "/api/v1/libraries/10/unmount", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", body, rec.Code, rec.Body.String())
		}
		if len(fc.calls) != 1 || fc.calls[0] != want {
			t.Errorf("%s: calls = %q, want [%q]", body, fc.calls, want)
		}
	}
}

func TestChangerErrorMapping(t *testing.T) {
	sense := func(key, asc, ascq uint8) error {
		return fmt.Errorf("move: %w", scsi.DecodeSense(scsi.FixedSense(key, asc, ascq)))
	}
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		hint   string // in the message, when set
	}{
		{"not found", fmt.Errorf("%w: no medium changer", changer.ErrUnavailable), 503, "changer_unavailable", ""},
		{"transport", &scsi.TransportError{HostStatus: 1}, 503, "changer_unavailable", ""},
		{"source empty", sense(scsi.KeyIllegalRequest, 0x3b, 0x0e), 409, "conflict", ""},
		{"destination full", sense(scsi.KeyIllegalRequest, 0x3b, 0x0d), 409, "conflict", ""},
		{"invalid element", sense(scsi.KeyIllegalRequest, 0x21, 0x01), 409, "conflict", ""},
		{"MAP open", sense(scsi.KeyNotReady, 0x3a, 0x02), 409, "conflict", ""},
		{"library offline", sense(scsi.KeyNotReady, 0x04, 0x12), 409, "conflict", ""},
		{"invalid field", sense(scsi.KeyIllegalRequest, 0x24, 0x00), 500, "scsi_error", ""},
		// mhVTL before markh794/mhvtl#285 reports any failed load into a
		// drive as 04/04/03; from #285 on, 05/30/00 for media the drive
		// cannot use and 04/53/00 for any other load failure.
		{"drive refused cartridge", sense(scsi.KeyHardwareError, 0x04, 0x03), 409, "conflict", "failed to load or unload the cartridge"},
		{"incompatible medium", sense(scsi.KeyIllegalRequest, 0x30, 0x00), 409, "conflict", "cannot use this cartridge's media type"},
		{"load failed", sense(scsi.KeyHardwareError, 0x53, 0x00), 409, "conflict", "the drive refused the load"},
		{"other hardware", sense(scsi.KeyHardwareError, 0x44, 0x00), 500, "scsi_error", ""},
		{"drive empty", changer.ErrDriveEmpty, 409, "conflict", ""},
		{"no empty slot", changer.ErrNoEmptySlot, 409, "conflict", ""},
		{"out of range", fmt.Errorf("%w: slot 99", changer.ErrOutOfRange), 400, "bad_request", ""},
		{"other", errors.New("boom"), 500, "scsi_error", ""},
	}
	for _, c := range cases {
		fc := &fakeChangers{err: c.err}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":1}`)
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d (body %s)", c.name, rec.Code, c.status, rec.Body.String())
			continue
		}
		body := rec.Body.String() // errorCode drains it
		if code := errorCode(t, rec); code != c.code {
			t.Errorf("%s: code = %q, want %q", c.name, code, c.code)
		}
		if c.hint != "" && !strings.Contains(body, c.hint) {
			t.Errorf("%s: body %s lacks %q", c.name, body, c.hint)
		}
	}
}

func TestMountWithoutChangerIsUnavailable(t *testing.T) {
	h := NewServer(Config{ConfigDir: writeConfigDir(t)}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":3}`)
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "changer_unavailable" {
		t.Fatalf("status = %d, want 503 changer_unavailable", rec.Code)
	}
}

func TestMountMissingConfigIsNotFound(t *testing.T) {
	h := NewServer(Config{ConfigDir: filepath.Join(t.TempDir(), "none"), Changers: &fakeChangers{}}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":3}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// writeConfigDirWithMedia is writeConfigDir with every library's home
// directory moved to a temporary one holding media for the given barcodes.
func writeConfigDirWithMedia(t *testing.T, barcodes ...string) string {
	t.Helper()
	dir := writeConfigDir(t)
	home := t.TempDir()
	for _, bc := range barcodes {
		if err := os.Mkdir(filepath.Join(home, bc), 0o755); err != nil {
			t.Fatalf("create media %s: %v", bc, err)
		}
		// The files vtltape opens on load, so the cartridge is not missing.
		for _, f := range []string{"data.0", "indx.0", "meta.0"} {
			if err := os.WriteFile(filepath.Join(home, bc, f), nil, 0o644); err != nil {
				t.Fatalf("create media %s: %v", bc, err)
			}
		}
	}
	conf := filepath.Join(dir, "device.conf")
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatalf("read device.conf: %v", err)
	}
	re := regexp.MustCompile(`(?m)^(\s*Home directory:).*$`)
	if err := os.WriteFile(conf, re.ReplaceAll(b, []byte("${1} "+home)), 0o644); err != nil {
		t.Fatalf("write device.conf: %v", err)
	}
	return dir
}

func TestMapImport(t *testing.T) {
	fc := &ipc.FakeController{}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: fc}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/30/map/import", `{"barcode":"E01009L8"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.Calls) != 1 || fc.Calls[0] != (ipc.Call{Method: "ImportMap", Dev: 30, Arg: "E01009L8"}) {
		t.Errorf("calls = %+v, want ImportMap lib 30 E01009L8", fc.Calls)
	}

	for _, body := range []string{`{"barcode":""}`, `{"barcode":"A B"}`, `{"barcode":"ABCDEFGHIJKLMNOPQ"}`, `{"barcode":"x;exit"}`, `{`} {
		fc := &ipc.FakeController{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Controller: fc}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/30/map/import", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if len(fc.Calls) != 0 {
			t.Errorf("%s: controller called: %+v", body, fc.Calls)
		}
	}
}

func TestMapImportErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: no such file or directory", ipc.ErrUnavailable), 503, "ipc_unavailable"},
		{ipc.ErrTimeout, 504, "ipc_timeout"},
		{&ipc.ReplyError{Command: "load map A", Reply: "MAP Full"}, 409, "conflict"},
		{errors.New("boom"), 500, "control_error"},
	}
	for _, c := range cases {
		h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "A00001L8"), Controller: &ipc.FakeController{Err: c.err}}).Handler()
		rec := postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"A00001L8"}`)
		if rec.Code != c.status {
			t.Errorf("%v: status = %d, want %d", c.err, rec.Code, c.status)
			continue
		}
		if code := errorCode(t, rec); code != c.code {
			t.Errorf("%v: code = %q, want %q", c.err, code, c.code)
		}
	}

	h := NewServer(Config{ConfigDir: writeConfigDir(t)}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"A00001L8"}`); rec.Code != 503 {
		t.Errorf("nil controller: status = %d, want 503", rec.Code)
	}
}

func TestMapImportWithoutMediaIsRefused(t *testing.T) {
	fc := &ipc.FakeController{}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: fc}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"E01033L8"}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "conflict" {
		t.Fatalf("status = %d (body %s), want 409 conflict", rec.Code, rec.Body.String())
	}
	if len(fc.Calls) != 0 {
		t.Errorf("controller called for a cartridge with no media: %+v", fc.Calls)
	}
}
