package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mhvtl-control/internal/chleak"
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

func TestSystemKernelCh(t *testing.T) {
	sys := t.TempDir()
	// Library 10's changer (unit serial XYZZY_A in the fixture) on
	// mhVTL's HBA, with ch bound to it.
	dev := filepath.Join(sys, "class", "scsi_device", "3:0:0:0", "device")
	for _, d := range []string{dev, filepath.Join(sys, "class", "scsi_host", "host3"), filepath.Join(sys, "module", "mhvtl")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, b := range map[string][]byte{
		filepath.Join(dev, "vpd_pg80"):                                 append([]byte{0x01, 0x80, 0, 7}, "XYZZY_A"...),
		filepath.Join(sys, "class", "scsi_host", "host3", "proc_name"): []byte("mhvtl\n"),
		filepath.Join(sys, "module", "mhvtl", "refcnt"):                []byte("5\n"),
	} {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../bus/scsi/drivers/ch", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	NewServer(Config{ConfigDir: writeConfigDir(t), SysRoot: sys}).Handler().
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	var got System
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	ch := got.KernelCh
	if ch == nil || got.KernelChError != "" {
		t.Fatalf("kernel_ch = %+v, error %q", ch, got.KernelChError)
	}
	if len(ch.Changers) != 1 || ch.Changers[0] != (chleak.Changer{Library: 10, HCTL: "3:0:0:0"}) ||
		ch.ModuleRefcount == nil || *ch.ModuleRefcount != 5 || ch.MhvtlDevices != 1 || ch.Leaked {
		t.Errorf("kernel_ch = %+v", ch)
	}
}
