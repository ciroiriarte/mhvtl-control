package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const etcDir = "/home/ciro/code/vtl/mhvtl/etc"

// writeConfigDir copies the real device.conf and library_contents.10 fixtures
// into a temp dir and returns it.
func writeConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for src, dst := range map[string]string{
		filepath.Join(etcDir, "device.conf"):         "device.conf",
		filepath.Join(etcDir, "library_contents.10"): "library_contents.10",
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read fixture %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", dst, err)
		}
	}
	return dir
}

func TestLibrariesHandler(t *testing.T) {
	dir := writeConfigDir(t)
	srv := NewServer(Config{ConfigDir: dir})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Libraries []libraryDTO `json:"libraries"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var lib *libraryDTO
	for i := range got.Libraries {
		if got.Libraries[i].ID == 10 {
			lib = &got.Libraries[i]
			break
		}
	}
	if lib == nil {
		t.Fatalf("library id 10 not found in %+v", got.Libraries)
	}
	if lib.Vendor != "STK" || lib.Product != "L700" {
		t.Errorf("library 10 vendor/product = %q/%q, want STK/L700", lib.Vendor, lib.Product)
	}
	if len(lib.Drives) < 1 {
		t.Fatalf("library 10 has %d drives, want >= 1", len(lib.Drives))
	}
	if lib.Drives[0].ID == 0 {
		t.Errorf("drive 0 has zero id: %+v", lib.Drives[0])
	}
}

func TestLibrariesHandlerMissingConfig(t *testing.T) {
	srv := NewServer(Config{ConfigDir: filepath.Join(t.TempDir(), "does-not-exist")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Libraries []libraryDTO `json:"libraries"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Libraries) != 0 {
		t.Errorf("libraries = %d, want 0 (empty list)", len(got.Libraries))
	}
	if got.Libraries == nil {
		t.Error("libraries is null, want empty array")
	}
}
