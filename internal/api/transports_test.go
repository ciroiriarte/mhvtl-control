package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// targetTree writes a configfs target tree with library 10 and drive 11 as
// TCMU backstores, exported on iSCSI LUNs 0 and 1.
func targetTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	tpg := filepath.Join(root, "iscsi", "iqn.2003-01.org.linux-iscsi.vtl:mhvtl", "tpgt_1")
	files := map[string]string{
		filepath.Join(tpg, "enable"):                     "1\n",
		filepath.Join(tpg, "np", "0.0.0.0:3260", "iser"): "0\n",
	}
	for i, bs := range []string{"lib10", "tape11"} {
		files[filepath.Join(root, "core", "user_0", bs, "enable")] = "1\n"
		files[filepath.Join(root, "core", "user_0", bs, "attrib", "dev_config")] = "mhvtl/" + bs[len(bs)-2:] + "\n"
		files[filepath.Join(tpg, "lun", "lun_"+string(rune('0'+i)), "alua_tg_pt_gp")] = "default_tg_pt_gp\n"
	}
	for p, c := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, bs := range []string{"lib10", "tape11"} {
		if err := os.Symlink("../../../../../../target/core/user_0/"+bs, filepath.Join(tpg, "lun", "lun_"+string(rune('0'+i)), "l")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func getTransports(t *testing.T, cfg Config) transportsDTO {
	t.Helper()
	rec := httptest.NewRecorder()
	NewServer(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/transports", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got transportsDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestTransportsHandler(t *testing.T) {
	got := getTransports(t, Config{ConfigDir: writeConfigDir(t), TargetRoot: targetTree(t), SysRoot: t.TempDir()})
	if !got.ConfigFS || len(got.Warnings) != 0 {
		t.Fatalf("configfs = %v, warnings %q", got.ConfigFS, got.Warnings)
	}
	if len(got.Backstores) != 2 {
		t.Fatalf("backstores = %+v", got.Backstores)
	}
	if d := got.Backstores[0].Device; d == nil || d.Kind != "library" || d.ID != 10 {
		t.Errorf("lib10 device = %+v, want library 10", d)
	}
	if d := got.Backstores[1].Device; d == nil || d.Kind != "drive" || d.ID != 11 || d.LibraryID == nil || *d.LibraryID != 10 {
		t.Errorf("tape11 device = %+v, want drive 11 of library 10", d)
	}
	if luns := got.Fabrics[0].Targets[0].TPGs[0].LUNs; len(luns) != 2 || luns[1].Backstore != "tape11" {
		t.Errorf("luns = %+v", luns)
	}
	if !reflect.DeepEqual(got.ActiveTransports, []string{"iscsi"}) {
		t.Errorf("active_transports = %q, want iscsi (mhvtl.ko not loaded)", got.ActiveTransports)
	}
}

func TestTransportsNoConfigFS(t *testing.T) {
	rec := httptest.NewRecorder()
	cfg := Config{TargetRoot: filepath.Join(t.TempDir(), "absent"), SysRoot: t.TempDir()}
	NewServer(cfg).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/transports", nil))
	want := `{"configfs":false,"backstores":[],"fabrics":[],"warnings":[],"active_transports":[]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("got %d %s, want 200 %s", rec.Code, rec.Body.String(), want)
	}
}

func TestSystemActiveTransports(t *testing.T) {
	sys := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sys, "module", "mhvtl"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	NewServer(Config{ConfigDir: writeConfigDir(t), TargetRoot: targetTree(t), SysRoot: sys}).Handler().
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	var got System
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ActiveTransports, []string{"local", "iscsi"}) {
		t.Errorf("active_transports = %q, want local, iscsi", got.ActiveTransports)
	}
}
