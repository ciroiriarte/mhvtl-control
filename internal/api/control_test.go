package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mhvtld/internal/ipc"
)

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func TestMountLoadsSlot(t *testing.T) {
	fc := &ipc.FakeController{}
	h := NewServer(Config{Controller: fc}).Handler()

	rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.Calls) != 1 {
		t.Fatalf("calls = %d, want 1: %+v", len(fc.Calls), fc.Calls)
	}
	if c := fc.Calls[0]; c.Method != "Load" || c.Dev != 11 || c.Arg != 3 {
		t.Errorf("call = %+v, want Load dev 11 arg 3", c)
	}
}

func TestMountLoadsMapWhenNoSlot(t *testing.T) {
	fc := &ipc.FakeController{}
	h := NewServer(Config{Controller: fc}).Handler()

	rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.Calls) != 1 {
		t.Fatalf("calls = %d, want 1: %+v", len(fc.Calls), fc.Calls)
	}
	if c := fc.Calls[0]; c.Method != "LoadMap" || c.Dev != 11 {
		t.Errorf("call = %+v, want LoadMap dev 11", c)
	}
}

func TestUnmountUnloads(t *testing.T) {
	fc := &ipc.FakeController{}
	h := NewServer(Config{Controller: fc}).Handler()

	rec := postJSON(t, h, "/api/v1/libraries/10/unmount", `{"drive":11}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(fc.Calls) != 1 {
		t.Fatalf("calls = %d, want 1: %+v", len(fc.Calls), fc.Calls)
	}
	if c := fc.Calls[0]; c.Method != "Unload" || c.Dev != 11 {
		t.Errorf("call = %+v, want Unload dev 11", c)
	}
}

func TestMountNilControllerIsUnavailable(t *testing.T) {
	h := NewServer(Config{}).Handler()

	rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":3}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}
