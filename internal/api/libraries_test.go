package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
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

// getLibraries fetches /api/v1/libraries and returns the raw JSON objects by
// library id, so tests can assert on field presence as well as values.
func getLibraries(t *testing.T, cfg Config) map[float64]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Libraries []map[string]any `json:"libraries"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[float64]map[string]any{}
	for _, l := range got.Libraries {
		out[l["id"].(float64)] = l
	}
	return out
}

func TestLibrariesLiveState(t *testing.T) {
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{
			{Number: 1, Full: true, Barcode: "E01005L8", SourceSlot: 5},
			{Number: 2},
			{Number: 3, Full: true, Barcode: "IMPORT01", SourceMAP: 2},
			{Number: 4},
		},
		Slots: []changer.SlotState{{Number: 1, Full: true, Barcode: "E01001L8"}, {Number: 2}},
		MAPs:  []changer.SlotState{{Number: 1}},
	}}
	lib := getLibraries(t, Config{ConfigDir: writeConfigDir(t), Changers: fc})[10]

	if lib["live"] != true || lib["live_error"] != nil {
		t.Fatalf("live = %v, live_error = %v", lib["live"], lib["live_error"])
	}
	drives := lib["drives"].([]any)
	d0 := drives[0].(map[string]any) // drive 11, Slot 1
	if d0["loaded"] != true || d0["barcode"] != "E01005L8" || d0["source_slot"] != 5.0 || d0["slot"] != 1.0 {
		t.Errorf("drive 11 = %v", d0)
	}
	d1 := drives[1].(map[string]any)
	if d1["loaded"] != false || d1["barcode"] != nil {
		t.Errorf("drive 12 = %v, want loaded:false and no barcode", d1)
	}
	if d2 := drives[2].(map[string]any); d2["source_map"] != 2.0 {
		t.Errorf("drive 13 = %v, want source_map 2", d2)
	}
	slots := lib["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("slots = %v, want the 2 live slots", slots)
	}
	if s0 := slots[0].(map[string]any); s0["full"] != true || s0["barcode"] != "E01001L8" {
		t.Errorf("slot 1 = %v", s0)
	}
	if s1 := slots[1].(map[string]any); s1["full"] != false || s1["barcode"] != "" {
		t.Errorf("slot 2 = %v", s1)
	}
	if maps := lib["maps"].([]any); len(maps) != 1 || maps[0].(map[string]any)["full"] != false {
		t.Errorf("maps = %v", maps)
	}
}

func TestLibrariesNotLiveKeepsConfigView(t *testing.T) {
	for name, cfg := range map[string]Config{
		"changer unreachable": {Changers: &fakeChangers{invErr: errors.New("changer unavailable: no medium changer")}},
		"no changer access":   {},
	} {
		cfg.ConfigDir = writeConfigDir(t)
		lib := getLibraries(t, cfg)[10]
		if lib["live"] != false || lib["live_error"] == "" || lib["live_error"] == nil {
			t.Errorf("%s: live = %v, live_error = %v", name, lib["live"], lib["live_error"])
		}
		d0 := lib["drives"].([]any)[0].(map[string]any)
		if _, ok := d0["loaded"]; ok {
			t.Errorf("%s: drive reports loaded without live state: %v", name, d0)
		}
		slots := lib["slots"].([]any)
		if len(slots) == 0 {
			t.Fatalf("%s: config slots dropped", name)
		}
		if _, ok := slots[0].(map[string]any)["full"]; ok {
			t.Errorf("%s: slot reports full without live state", name)
		}
	}
}

// Each drive reports its own NAA and SCSI address from device.conf, not the
// library's (the dashboard used to fall back to the changer's identity).
func TestLibrariesDriveIdentity(t *testing.T) {
	srv := NewServer(Config{ConfigDir: writeConfigDir(t)})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil))
	var body struct {
		Libraries []struct {
			ID     int    `json:"id"`
			NAA    string `json:"naa"`
			Drives []struct {
				ID     int    `json:"id"`
				NAA    string `json:"naa"`
				Target int    `json:"target"`
			} `json:"drives"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lib := body.Libraries[0]
	d := lib.Drives[0]
	if d.ID != 11 || d.NAA != "10:22:33:44:ab:00:01:00" || d.Target != 1 {
		t.Errorf("drive = %+v, want id 11, naa 10:22:33:44:ab:00:01:00, target 1", d)
	}
	if d.NAA == lib.NAA {
		t.Errorf("drive NAA equals the library's %q", lib.NAA)
	}
}

type fakeNodes struct{}

func (fakeNodes) LibraryNodes(lib config.Library) (changer.Nodes, error) {
	return changer.Nodes{SG: "/dev/sg10", SCH: "/dev/sch0", ByID: []string{"/dev/tape/by-id/scsi-SSTK_L700_XYZZY_A-changer"}}, nil
}

func (fakeNodes) DriveNodes(d config.Drive) (changer.Nodes, error) {
	if d.ID != 11 {
		return changer.Nodes{}, changer.ErrUnavailable
	}
	return changer.Nodes{SG: "/dev/sg8", ST: "/dev/st2", NST: "/dev/nst2"}, nil
}

// Changers and drives carry their device nodes when found; a drive that isn't
// found has no nodes field.
func TestLibrariesNodes(t *testing.T) {
	lib := getLibraries(t, Config{ConfigDir: writeConfigDir(t), Nodes: fakeNodes{}})[10]
	ln, ok := lib["nodes"].(map[string]any)
	if !ok || ln["sch"] != "/dev/sch0" || ln["sg"] != "/dev/sg10" || len(ln["by_id"].([]any)) != 1 {
		t.Errorf("library nodes = %v", lib["nodes"])
	}
	if _, ok := ln["st"]; ok {
		t.Errorf("library nodes report an st node: %v", ln)
	}
	drives := lib["drives"].([]any)
	dn, ok := drives[0].(map[string]any)["nodes"].(map[string]any)
	if !ok || dn["nst"] != "/dev/nst2" || dn["st"] != "/dev/st2" || dn["sg"] != "/dev/sg8" {
		t.Errorf("drive 11 nodes = %v", drives[0])
	}
	if _, ok := drives[1].(map[string]any)["nodes"]; ok {
		t.Errorf("drive 12 has nodes though it wasn't found: %v", drives[1])
	}

	lib = getLibraries(t, Config{ConfigDir: writeConfigDir(t)})[10]
	if _, ok := lib["nodes"]; ok {
		t.Errorf("nodes reported without a node service: %v", lib["nodes"])
	}
}
