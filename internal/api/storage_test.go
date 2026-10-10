package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mhvtld/internal/rawdisk"
)

func TestDisksHandlerUnformatted(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	// 8 MiB of zeros: large enough for a superblock window read, no MVTL magic.
	if err := os.WriteFile(img, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	opts := rawdisk.Options{SysBlockDir: filepath.Join(t.TempDir(), "no-sys"), ScanPaths: []string{img}}
	h := NewServer(Config{DiskOptions: opts}).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/storage/disks", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Disks []diskDTO `json:"disks"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Disks) != 1 {
		t.Fatalf("disks = %d, want 1: %+v", len(got.Disks), got.Disks)
	}
	d := got.Disks[0]
	if d.MVTLFormatted {
		t.Error("mvtl_formatted = true, want false")
	}
	if d.MVTL != nil {
		t.Errorf("mvtl = %+v, want null", d.MVTL)
	}
	if d.Path != img {
		t.Errorf("path = %q, want %q", d.Path, img)
	}
	if d.Status != "no_mvtl" {
		t.Errorf("status = %q, want no_mvtl", d.Status)
	}
}

func TestFormatDeviceInUse(t *testing.T) {
	ff := fakeFormatter{err: rawdisk.ErrDeviceInUse}
	h := NewServer(Config{Formatter: ff}).Handler()

	rec := postJSON(t, h, "/api/v1/storage/disks/format", `{"device":"/dev/sdb","barcode":"VOL001L5"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "device_in_use" {
		t.Errorf("code = %q, want device_in_use", env.Error.Code)
	}
}

// fakeFormatter is a test Formatter returning a canned superblock or error.
type fakeFormatter struct {
	sb  *rawdisk.Superblock
	err error
}

func (f fakeFormatter) Format(rawdisk.FormatRequest) (*rawdisk.Superblock, error) {
	return f.sb, f.err
}

func TestFormatSuccess(t *testing.T) {
	ff := fakeFormatter{sb: &rawdisk.Superblock{Barcode: "VOL001L5", VendorID: "IBM", ProductID: "ULT3580-TD5", Version: 2}}
	h := NewServer(Config{Formatter: ff}).Handler()

	rec := postJSON(t, h, "/api/v1/storage/disks/format", `{"device":"/dev/loop0","barcode":"VOL001L5"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got mvtlDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Barcode != "VOL001L5" || got.VendorID != "IBM" {
		t.Errorf("summary = %+v, want barcode VOL001L5 vendor IBM", got)
	}
}

func TestFormatAlreadyFormatted(t *testing.T) {
	ff := fakeFormatter{err: rawdisk.ErrAlreadyFormatted}
	h := NewServer(Config{Formatter: ff}).Handler()

	rec := postJSON(t, h, "/api/v1/storage/disks/format", `{"device":"/dev/loop0","barcode":"VOL001L5"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "already_formatted" {
		t.Errorf("code = %q, want already_formatted", env.Error.Code)
	}
}

func TestFormatNilFormatterIsUnavailable(t *testing.T) {
	h := NewServer(Config{}).Handler()

	rec := postJSON(t, h, "/api/v1/storage/disks/format", `{"device":"/dev/loop0","barcode":"VOL001L5"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}
