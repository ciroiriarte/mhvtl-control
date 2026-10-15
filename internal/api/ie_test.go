package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"mhvtld/internal/changer"
	"mhvtld/internal/ipc"
)

func TestMove(t *testing.T) {
	fc := &fakeChangers{move: changer.Move{From: changer.Location{Kind: "slot", Number: 1}, To: changer.Location{Kind: "map", Number: 2}}}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/move", `{"from":{"kind":"slot","number":1},"to":{"kind":"map","number":2}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fc.calls) != 1 || fc.calls[0] != "Transfer lib=10 slot1->map2" {
		t.Errorf("calls = %q", fc.calls)
	}
	for _, body := range []string{
		`{"from":{"kind":"drive","number":1},"to":{"kind":"slot","number":2}}`,
		`{"from":{"kind":"slot","number":0},"to":{"kind":"slot","number":2}}`,
		`{"from":{"kind":"slot","number":2},"to":{"kind":"slot","number":2}}`,
		`{"from":{"kind":"slot","number":1}}`,
	} {
		fc := &fakeChangers{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/10/move", body); rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if len(fc.calls) != 0 {
			t.Errorf("%s: changer called", body)
		}
	}
	fc = &fakeChangers{err: changer.ErrMAPOpen}
	h = NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/move", `{"from":{"kind":"slot","number":1},"to":{"kind":"map","number":2}}`); rec.Code != 409 {
		t.Errorf("open MAP: status = %d, want 409", rec.Code)
	}
}

func TestMapExport(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"slot":4}`, "Export lib=10 slot=4 drive=0 barcode="},
		{`{"drive":11}`, "Export lib=10 slot=0 drive=11 barcode="},
		{`{"barcode":"A00001L8"}`, "Export lib=10 slot=0 drive=0 barcode=A00001L8"},
	}
	for _, c := range cases {
		fc := &fakeChangers{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/10/map/export", c.body); rec.Code != 200 {
			t.Fatalf("%s: status = %d (%s)", c.body, rec.Code, rec.Body.String())
		}
		if len(fc.calls) != 1 || fc.calls[0] != c.want {
			t.Errorf("%s: calls = %q", c.body, fc.calls)
		}
	}
	for _, body := range []string{`{}`, `{"slot":1,"drive":11}`, `{"slot":-1}`, `{"drive":99}`, `{"barcode":"a b"}`} {
		fc := &fakeChangers{}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/10/map/export", body); rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
	for err, status := range map[error]int{
		changer.ErrMAPFull:  409,
		changer.ErrMAPOpen:  409,
		changer.ErrNotFound: 404,
	} {
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: &fakeChangers{err: err}}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/10/map/export", `{"slot":1}`); rec.Code != status {
			t.Errorf("%v: status = %d, want %d", err, rec.Code, status)
		}
	}
}

func TestMapActions(t *testing.T) {
	for action, method := range map[string]string{"open": "OpenMap", "close": "CloseMap", "eject": "EjectMap"} {
		ctl := &ipc.FakeController{}
		fc := &fakeChangers{inv: changer.Inventory{MAPs: []changer.SlotState{{Number: 1, Full: true, Barcode: "OUT001L8"}, {Number: 2}}}}
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Controller: ctl, Changers: fc}).Handler()
		rec := postJSON(t, h, "/api/v1/libraries/10/map/"+action, "")
		if rec.Code != 200 {
			t.Fatalf("%s: status = %d (%s)", action, rec.Code, rec.Body.String())
		}
		if len(ctl.Calls) != 1 || ctl.Calls[0] != (ipc.Call{Method: method, Dev: 10}) {
			t.Errorf("%s: calls = %+v", action, ctl.Calls)
		}
		if action == "eject" && !strings.Contains(rec.Body.String(), `"ejected":["OUT001L8"]`) {
			t.Errorf("eject body = %s", rec.Body.String())
		}
	}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Controller: &ipc.FakeController{}}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/map/frobnicate", ""); rec.Code != 404 {
		t.Errorf("unknown action: status = %d, want 404", rec.Code)
	}
	h = NewServer(Config{ConfigDir: writeConfigDir(t), Controller: &ipc.FakeController{
		Err: &ipc.ReplyError{Command: "empty map", Reply: "Can't empty map while MAP is closed"}}}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/map/eject", ""); rec.Code != 409 {
		t.Errorf("refused eject: status = %d, want 409", rec.Code)
	}
	h = NewServer(Config{ConfigDir: writeConfigDir(t)}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/map/open", ""); rec.Code != 503 {
		t.Errorf("no controller: status = %d, want 503", rec.Code)
	}
}

func TestMapImportStore(t *testing.T) {
	ctl := &ipc.FakeController{}
	fc := &fakeChangers{move: changer.Move{From: changer.Location{Kind: "map", Number: 1}, To: changer.Location{Kind: "slot", Number: 3}, Barcode: "E01009L8"}}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: ctl, Changers: fc}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"E01009L8","store":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"to":{"kind":"slot","number":3}`) {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(ctl.Calls) != 1 || strings.Join(fc.calls, "|") != "Inventory lib=10|Store lib=10 E01009L8" {
		t.Errorf("calls = %+v / %q", ctl.Calls, fc.calls)
	}

	// The import happened even when the store fails: 200, with the reason.
	fc = &fakeChangers{storeErr: changer.ErrNoEmptySlot}
	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: &ipc.FakeController{}, Changers: fc}).Handler()
	rec = postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"E01009L8","store":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"imported":true`) ||
		!strings.Contains(rec.Body.String(), "left in the MAP") {
		t.Errorf("failed store: %d %s", rec.Code, rec.Body.String())
	}

	// A barcode already in the library is refused before the queue is used.
	ctl = &ipc.FakeController{}
	fc = &fakeChangers{inv: changer.Inventory{Slots: []changer.SlotState{{Number: 7, Full: true, Barcode: "E01009L8"}}}}
	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: ctl, Changers: fc}).Handler()
	rec = postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"E01009L8"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "slot 7") || len(ctl.Calls) != 0 {
		t.Errorf("already in library: %d %s, calls %+v", rec.Code, rec.Body.String(), ctl.Calls)
	}

	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01009L8"), Controller: &ipc.FakeController{}}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/map/import", `{"barcode":"E01009L8","store":true}`); rec.Code != 503 {
		t.Errorf("store without changer: status = %d, want 503", rec.Code)
	}
}

func TestLibrariesReportMAPState(t *testing.T) {
	open := true
	fc := &fakeChangers{inv: changer.Inventory{MAPOpen: &open}}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Changers: fc}).Handler()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries", "")
	if !strings.Contains(rec.Body.String(), `"map_open":true`) {
		t.Errorf("body lacks map_open: %s", rec.Body.String())
	}
	h = NewServer(Config{ConfigDir: writeConfigDir(t), Changers: &fakeChangers{invErr: errors.New("x")}}).Handler()
	if rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries", ""); strings.Contains(rec.Body.String(), "map_open") {
		t.Errorf("map_open reported without a live view: %s", rec.Body.String())
	}
}
