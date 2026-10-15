package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	NewServer(Config{Version: "0.1.0"}).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var got System
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Hostname == "" {
		t.Error("hostname empty")
	}
	if got.Version != "0.1.0" {
		t.Errorf("version = %q, want 0.1.0", got.Version)
	}
	if got.CPUCount < 1 {
		t.Errorf("cpu_count = %d", got.CPUCount)
	}
}
