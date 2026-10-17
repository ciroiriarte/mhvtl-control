package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/conffile"
)

const shippedMhvtlConf = "#\n# mhvtl.conf\n#\n\n# Home directory for tape emulation files\nMHVTL_HOME_PATH=/var/lib/mhvtl\n\n" +
	"# Default media capacity (500 M)\nCAPACITY=500\n\n# Set default verbosity [0|1|2|3]\nVERBOSE=1\n\n" +
	"# Set kernel module debugging [0|1]\nVTL_DEBUG=0\n"

// PATCH /config/system stages mhvtl.conf as the helper derives it: the
// current file with only the values changed (plan, amendment "mhvtl.conf
// is shell").
func TestConfigSystem(t *testing.T) {
	e := newWriteEnv(t, nil)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(shippedMhvtlConf), 0o644); err != nil {
		t.Fatal(err)
	}
	st := e.edit(http.MethodPatch, "/api/v1/config/system", map[string]any{"values": map[string]string{"CAPACITY": "600", "VERBOSE": "2"}})
	want := strings.Replace(strings.Replace(shippedMhvtlConf, "CAPACITY=500\n", "CAPACITY=600\n", 1), "VERBOSE=1\n", "VERBOSE=2\n", 1)
	if got := file(t, st, "mhvtl.conf"); got != want {
		t.Errorf("mhvtl.conf:\n%s\nwant:\n%s", got, want)
	}
	if err := apply.DeriveMhvtlConf([]byte(shippedMhvtlConf), []byte(want)); err != nil {
		t.Errorf("the helper would refuse it: %v", err)
	}
	if st.Base["mhvtl.conf"] != conffile.Hash([]byte(shippedMhvtlConf)) || len(st.Files) != 1 {
		t.Errorf("stage %+v", st)
	}
	st = e.edit(http.MethodPatch, "/api/v1/config/system", map[string]any{"stage": st.Stage, "values": map[string]string{"VTL_DEBUG": "1", "CAPACITY": "500"}})
	want = strings.Replace(strings.Replace(want, "VTL_DEBUG=0\n", "VTL_DEBUG=1\n", 1), "CAPACITY=600\n", "CAPACITY=500\n", 1)
	if got := file(t, st, "mhvtl.conf"); got != want {
		t.Errorf("second edit:\n%s", got)
	}

	// The apply sends it.
	e.answer(func(apply.Request) apply.Response { return apply.Response{Result: apply.ResultValidated} })
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true}); rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	reqs := e.requests()
	if len(reqs) != 1 || len(reqs[0].Files) != 1 || reqs[0].Files[0].Name != "mhvtl.conf" || string(reqs[0].Files[0].Content) != want {
		t.Errorf("plan request: %+v", reqs)
	}

	for _, tc := range []struct {
		values map[string]string
		code   string
	}{
		{map[string]string{"CAPACITY": "0600"}, apply.ErrMhvtlConfValue},
		{map[string]string{"VERBOSE": "4"}, apply.ErrMhvtlConfValue},
		{map[string]string{"VTL_DEBUG": "1; rm -rf /"}, apply.ErrMhvtlConfValue},
		{map[string]string{"MHVTL_HOME_PATH": "/srv/tapes"}, "bad_request"},
		{map[string]string{"DAEMON_DEBUG": "-d"}, "bad_request"},
		{map[string]string{}, "bad_request"},
	} {
		rec := e.call(http.MethodPatch, "/api/v1/config/system", map[string]any{"stage": st.Stage, "values": tc.values})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%v: %d %s, want 400 %s", tc.values, rec.Code, rec.Body, tc.code)
		}
	}
	if rec := e.call(http.MethodPatch, "/api/v1/config/system", map[string]any{"stage": st.Stage, "values": map[string]string{"MHVTL_HOME_PATH": "/srv/tapes"}}); !strings.Contains(rec.Body.String(), "mhvtl-move-home") {
		t.Errorf("MHVTL_HOME_PATH: %s", rec.Body)
	}
}

// GET /config reports the mhvtl.conf values PATCH /config/system sets, as
// the file has them; a key it lacks is left out.
func TestConfigSystemRead(t *testing.T) {
	e := newWriteEnv(t, nil)
	conf := strings.Replace(shippedMhvtlConf, "VTL_DEBUG=0\n", "", 1)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	type sys struct {
		System systemDTO `json:"system"`
	}
	rec := e.call(http.MethodGet, "/api/v1/config", nil)
	got := decodeBody[sys](t, rec).System
	if !reflect.DeepEqual(got.Values, map[string]string{"CAPACITY": "500", "VERBOSE": "1"}) {
		t.Errorf("system values = %v", got.Values)
	}
	// The file itself, which a client diffs a staged mhvtl.conf against.
	if got.File == nil || got.File.Name != "mhvtl.conf" || got.File.Content != conf || got.File.SHA256 != conffile.Hash([]byte(conf)) {
		t.Errorf("system file = %+v", got.File)
	}
	os.Remove(filepath.Join(e.dir, "mhvtl.conf"))
	rec = e.call(http.MethodGet, "/api/v1/config", nil)
	if got := decodeBody[sys](t, rec).System; len(got.Values) != 0 || got.Values == nil || got.File != nil || got.FileError != "" {
		t.Errorf("no mhvtl.conf: %#v", got)
	}
	// A mhvtl.conf that cannot be read is not "no file" (#62 review N1).
	if err := os.Mkdir(filepath.Join(e.dir, "mhvtl.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = e.call(http.MethodGet, "/api/v1/config", nil)
	if got := decodeBody[sys](t, rec).System; got.File != nil || !strings.Contains(got.FileError, "mhvtl.conf") {
		t.Errorf("unreadable mhvtl.conf: %#v", got)
	}
	// Nor is the home it would set known.
	var homes struct {
		Effective *string `json:"default_home_effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &homes); err != nil || homes.Effective == nil || *homes.Effective != "" {
		t.Errorf("default_home_effective with an unreadable mhvtl.conf: %v %v", homes.Effective, err)
	}
}

// GET /config gives the default home the helper judges with,
// default_home_effective: MHVTL_HOME_PATH, else /var/lib/mhvtl. A stage
// that has mhvtl.conf gives it as staged. default_home stays the
// dashboard's media path: MHVTL_HOME_PATH too, but --home-dir rather than
// the helper's default when mhvtl.conf sets none.
func TestConfigDefaultHomeEffective(t *testing.T) {
	e := newWriteEnv(t, func(c *Config) { c.DefaultHome = "/srv/served" })
	type homes struct {
		DefaultHome          string `json:"default_home"`
		DefaultHomeEffective string `json:"default_home_effective"`
	}
	get := func() homes { return decodeBody[homes](t, e.call(http.MethodGet, "/api/v1/config", nil)) }
	if got := get(); got != (homes{"/srv/served", apply.DefaultHome}) {
		t.Errorf("no mhvtl.conf: %+v", got)
	}
	conf := strings.Replace(shippedMhvtlConf, "MHVTL_HOME_PATH=/var/lib/mhvtl", "MHVTL_HOME_PATH=/srv/tapes", 1)
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != (homes{"/srv/tapes", "/srv/tapes"}) {
		t.Errorf("MHVTL_HOME_PATH set: %+v", got)
	}
	// The shell takes the last of two assignments; both fields read the
	// file as conffile does (#62 review N3).
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(conf+"MHVTL_HOME_PATH=/srv/later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != (homes{"/srv/later", "/srv/later"}) {
		t.Errorf("MHVTL_HOME_PATH twice: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/10", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
	if st.DefaultHomeEffective != "" {
		t.Errorf("stage without mhvtl.conf: %q", st.DefaultHomeEffective)
	}
	st = e.edit(http.MethodPatch, "/api/v1/config/system", map[string]any{"stage": st.Stage, "values": map[string]string{"VERBOSE": "2"}})
	if st.DefaultHomeEffective != "/srv/tapes" {
		t.Errorf("stage with mhvtl.conf: %q", st.DefaultHomeEffective)
	}
}

// Without a mhvtl.conf the edit creates one, which must not exist yet.
func TestConfigSystemNoFile(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.edit(http.MethodPatch, "/api/v1/config/system", map[string]any{"values": map[string]string{"VERBOSE": "2"}})
	if got := file(t, st, "mhvtl.conf"); got != "VERBOSE=2\n" {
		t.Errorf("mhvtl.conf %q", got)
	}
	if b, ok := st.Base["mhvtl.conf"]; !ok || b != "" {
		t.Errorf("base %q %v", b, ok)
	}
}

// Values staged over several requests make the file the helper derives
// from all of them at once, whatever order they came in (#62 review M1).
func TestConfigSystemTwoRequests(t *testing.T) {
	for _, disk := range []string{"", "MHVTL_HOME_PATH=/var/lib/mhvtl\n"} {
		for _, seq := range [][2]map[string]string{
			{{"VTL_DEBUG": "1"}, {"VERBOSE": "2"}},
			{{"VERBOSE": "2"}, {"CAPACITY": "600"}},
		} {
			e := newWriteEnv(t, nil)
			if disk != "" {
				if err := os.WriteFile(filepath.Join(e.dir, "mhvtl.conf"), []byte(disk), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			st := e.edit(http.MethodPatch, "/api/v1/config/system", map[string]any{"values": seq[0]})
			rec := e.call(http.MethodPatch, "/api/v1/config/system", map[string]any{"stage": st.Stage, "values": seq[1]})
			if rec.Code != http.StatusOK {
				t.Errorf("%q %v: second request %d %s", disk, seq, rec.Code, rec.Body)
				continue
			}
			got := file(t, decodeBody[stageDTO](t, rec), "mhvtl.conf")
			want := map[string]string{}
			for _, m := range seq {
				for k, v := range m {
					want[k] = v
				}
			}
			built, err := apply.BuildMhvtlConf([]byte(disk), want)
			if err != nil || got != string(built) {
				t.Errorf("%q %v: staged %q, want %q (%v)", disk, seq, got, built, err)
			}
		}
	}
}

// Staging judges a library's default home as the helper does, from its
// compiled-in /var/lib/mhvtl, not from --home-dir (#62 review L2).
func TestConfigHomesHelperDefault(t *testing.T) {
	root := t.TempDir()
	served := filepath.Join(root, "served")
	if err := os.MkdirAll(filepath.Join(served, "G03001TA"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newWriteEnv(t, func(c *Config) { c.DefaultHome = served })
	e.readable(30)
	p := filepath.Join(e.dir, "device.conf")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dc := conffile.ParseDeviceConf(b)
	if err := dc.DeleteAttr("Library", 30, "Home directory"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, dc.Render(), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := e.call(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"attrs": map[string]any{"Home directory": filepath.Join(root, "new")}})
	if strings.Contains(rec.Body.String(), served) {
		t.Errorf("judged from --home-dir: %d %s", rec.Code, rec.Body)
	}
	if rec.Code != http.StatusOK && !strings.Contains(rec.Body.String(), "/var/lib/mhvtl") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// setHomes gives libraries of the env's device.conf their own Home
// directory.
func (e *writeEnv) setHomes(homes map[int]string) {
	e.t.Helper()
	p := filepath.Join(e.dir, "device.conf")
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	dc := conffile.ParseDeviceConf(b)
	for id, h := range homes {
		if err := dc.SetAttr("Library", id, "Home directory", h); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := os.WriteFile(p, dc.Render(), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// A library's home changes only when the old one holds no cartridge and no
// other library uses it (plan 4.3); otherwise mhvtl-move-home moves it. A
// new library may get a new home. The helper applies the same rules
// (apply.CheckHomes).
func TestConfigHomeRules(t *testing.T) {
	// The test's homes are under t.TempDir(), in /tmp.
	defer func(s []string) { apply.SandboxedTmp = s }(apply.SandboxedTmp)
	apply.SandboxedTmp = nil
	root := t.TempDir()
	h1, h2, h3 := filepath.Join(root, "h1"), filepath.Join(root, "h2"), filepath.Join(root, "h3")
	if err := os.MkdirAll(filepath.Join(h1, "E01001L8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h2, 0o755); err != nil {
		t.Fatal(err)
	}
	homeOf := func(h string) map[string]any { return map[string]any{"attrs": map[string]any{"Home directory": h}} }

	e := newWriteEnv(t, nil)
	e.readable(30)
	e.setHomes(map[int]string{10: h1, 30: h2})
	rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10", homeOf(h3))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), apply.ErrHomeInUse) ||
		!strings.Contains(rec.Body.String(), "E01001L8") || !strings.Contains(rec.Body.String(), "mhvtl-move-home") {
		t.Errorf("home with a cartridge: %d %s", rec.Code, rec.Body)
	}
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/30", homeOf(h3))
	if got := file(t, st, "device.conf"); !strings.Contains(got, " Home directory: "+h3+"\n") {
		t.Errorf("empty home not changed:\n%s", got)
	}
	// A refused edit leaves the stage as it was.
	if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10", map[string]any{"stage": st.Stage, "attrs": map[string]any{"Home directory": nil}}); rec.Code != http.StatusConflict {
		t.Errorf("home with a cartridge to the default: %d %s", rec.Code, rec.Body)
	}
	if got := file(t, st, "device.conf"); !strings.Contains(got, " Home directory: "+h1+"\n") {
		t.Errorf("library 10's home lost:\n%s", got)
	}

	// A shared home stays, empty or not.
	e = newWriteEnv(t, nil)
	e.readable(30)
	e.setHomes(map[int]string{10: h2, 30: h2})
	if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/30", homeOf(h3)); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "shared with library 10") {
		t.Errorf("shared home: %d %s", rec.Code, rec.Body)
	}

	// A new library: a new home is fine, a forbidden one is not.
	e = newWriteEnv(t, nil)
	newLib := func(home string) map[string]any {
		return map[string]any{"id": 50, "target": 9, "slots": 1,
			"attrs": []map[string]string{{"key": "Home directory", "value": home}}}
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/libraries", newLib(filepath.Join(root, "new"))); rec.Code != http.StatusOK {
		t.Errorf("new library, new home: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/libraries", newLib("/etc/mhvtl/tapes")); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), apply.ErrHomeForbidden) {
		t.Errorf("new library, forbidden home: %d %s", rec.Code, rec.Body)
	}
}
