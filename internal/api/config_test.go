package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// genDir copies internal/validate/testdata/gen (device.conf and
// library_contents.10 and .30 as upstream's generators write them) into a
// temp dir and returns it.
func genDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"device.conf", "library_contents.10", "library_contents.30"} {
		b, err := os.ReadFile(filepath.Join("../validate/testdata/gen", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// noSys is a sysfs root with no SCSI devices: the host check reports that
// it could not read them (a warning), and nothing on the build host leaks
// into a test.
func noSys(t *testing.T) string { return filepath.Join(t.TempDir(), "sys") }

func do(t *testing.T, cfg Config, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	NewServer(cfg).Handler().ServeHTTP(rec, r)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// liveOf builds the READ ELEMENT STATUS view of lib as its file lists it,
// then lets edit change it.
func liveOf(t *testing.T, dir string, id int, edit func(*changer.Inventory)) changer.Inventory {
	t.Helper()
	inv, err := config.LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	lib, _ := findConfigLibrary(inv, id)
	var out changer.Inventory
	for _, s := range lib.Slots {
		out.Slots = append(out.Slots, changer.SlotState{Number: s.Number, Full: s.Barcode != "", Barcode: s.Barcode})
	}
	for _, m := range lib.MAPs {
		out.MAPs = append(out.MAPs, changer.SlotState{Number: m.Number, Full: m.Barcode != "", Barcode: m.Barcode})
	}
	for _, d := range lib.DriveLines {
		out.Drives = append(out.Drives, changer.DriveState{Number: d.Number})
	}
	if edit != nil {
		edit(&out)
	}
	return out
}

func TestConfigView(t *testing.T) {
	dir := genDir(t)
	live := liveOf(t, dir, 10, func(inv *changer.Inventory) {
		inv.Slots[0].Full, inv.Slots[0].Barcode = false, "" // E01001L8 is in drive 1
		inv.Drives[0].Full, inv.Drives[0].Barcode = true, "E01001L8"
		inv.MAPs[0].Full, inv.MAPs[0].Barcode = true, "ZZ0001L8" // imported at run time
	})
	fc := &fakeChangers{invByLib: map[int]changer.Inventory{10: live}, invErr: nil}
	fc30 := &errByLib{fakeChangers: fc, fail: map[int]error{30: fmt.Errorf("no medium changer")}}
	rec := do(t, Config{ConfigDir: dir, Changers: fc30}, http.MethodGet, "/api/v1/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decodeBody[struct {
		Files      []configFileDTO         `json:"files"`
		Libraries  []configLibraryDTO      `json:"libraries"`
		Problems   []configProblemDTO      `json:"problems"`
		LiveErrors map[string]string       `json:"live_errors"`
		Slots      map[string]slotsViewDTO `json:"slots"`
	}](t, rec)

	if sv := got.Slots["10"]; !sv.Live || sv.MAPs[0].LiveBarcode == nil || *sv.MAPs[0].LiveBarcode != "ZZ0001L8" {
		t.Errorf("slots of library 10 = %+v", sv)
	}
	if sv := got.Slots["30"]; sv.Live || sv.LiveError == "" || len(sv.Slots) == 0 || len(sv.Drives) != 0 {
		t.Errorf("slots of library 30 = %+v", sv)
	}
	if d := got.Slots["10"].Drives; !reflect.DeepEqual(d, []liveDriveDTO{{1, "E01001L8"}}) {
		t.Errorf("live drives of library 10 = %+v", d)
	}
	if w := got.Slots["10"].Warning; w != "" {
		t.Errorf("warning for library 10: %s", w)
	}

	if len(got.Files) != 3 {
		t.Fatalf("files = %+v", got.Files)
	}
	for _, f := range got.Files {
		b, _ := os.ReadFile(filepath.Join(dir, f.Name))
		if f.Content != string(b) || f.SHA256 != conffile.Hash(b) {
			t.Errorf("%s: content or hash differs from the file", f.Name)
		}
		switch f.Name {
		case "library_contents.10":
			if f.LiveContent == nil {
				t.Fatalf("library_contents.10 has no live_content")
			}
			lc := *f.LiveContent
			for _, want := range []string{"Slot 1:\n", "MAP 1: ZZ0001L8\n", "Slot 2: E01002L8\n"} {
				if !strings.Contains(lc, want) {
					t.Errorf("live_content lacks %q", want)
				}
			}
		default:
			if f.LiveContent != nil {
				t.Errorf("%s: live_content without a live library", f.Name)
			}
		}
	}
	if got.LiveErrors["30"] == "" {
		t.Errorf("live_errors = %v, want library 30's", got.LiveErrors)
	}
	if len(got.Problems) != 0 {
		t.Errorf("problems = %+v", got.Problems)
	}
	if len(got.Libraries) != 2 {
		t.Fatalf("libraries = %+v", got.Libraries)
	}
	l := got.Libraries[0]
	if l.ID != 10 || l.Personality.InitFn != "init_stklxx" || l.Personality.VPD83 ||
		l.Personality.Limits.Drives != 500 || l.Personality.RuleSource == "" {
		t.Errorf("library 10 = %+v", l.Personality)
	}
	if len(l.Drives) != 4 || len(l.DriveLines) != 4 || len(l.MAPs) != 4 || l.Slots[0].Barcode != "E01001L8" {
		t.Errorf("library 10: %d drives, %d drive lines, %d MAPs, slot 1 %q; want the file's", len(l.Drives), len(l.DriveLines), len(l.MAPs), l.Slots[0].Barcode)
	}
	if l.Home != "testdata/gen/home" {
		t.Errorf("home = %q", l.Home)
	}
}

// errByLib fails Inventory for the libraries it names.
type errByLib struct {
	*fakeChangers
	fail map[int]error
}

func (e *errByLib) Inventory(lib config.Library) (changer.Inventory, error) {
	if err := e.fail[lib.ID]; err != nil {
		return changer.Inventory{}, err
	}
	return e.fakeChangers.Inventory(lib)
}

// A line mhVTL would not read as written is reported with its file and line.
func TestConfigProblems(t *testing.T) {
	dir := genDir(t)
	p := filepath.Join(dir, "library_contents.10")
	b, _ := os.ReadFile(p)
	b = bytes.Replace(b, []byte("Slot 3: E01003L8"), []byte("Slot three: E01003L8"), 1)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(t, Config{ConfigDir: dir}, http.MethodGet, "/api/v1/config", "")
	got := decodeBody[struct {
		Problems []configProblemDTO `json:"problems"`
	}](t, rec)
	line := bytes.Count(b[:bytes.Index(b, []byte("Slot three"))], []byte("\n")) + 1
	if len(got.Problems) != 1 || got.Problems[0].File != "library_contents.10" || got.Problems[0].Line != line || got.Problems[0].Message == "" {
		t.Errorf("problems = %+v; want one at library_contents.10:%d", got.Problems, line)
	}
}

func TestConfigLibraryRoutes(t *testing.T) {
	dir := genDir(t)
	cfg := Config{ConfigDir: dir}

	rec := do(t, cfg, http.MethodGet, "/api/v1/config/libraries", "")
	libs := decodeBody[struct {
		Libraries []configLibraryDTO `json:"libraries"`
	}](t, rec)
	if len(libs.Libraries) != 2 || libs.Libraries[1].ID != 30 {
		t.Errorf("libraries = %+v", libs.Libraries)
	}

	rec = do(t, cfg, http.MethodGet, "/api/v1/config/libraries/30", "")
	if l := decodeBody[configLibraryDTO](t, rec); l.ID != 30 || l.Product == "" {
		t.Errorf("library 30 = %+v", l)
	}

	rec = do(t, cfg, http.MethodGet, "/api/v1/config/libraries/10/drives", "")
	drives := decodeBody[struct {
		Library    int            `json:"library"`
		Drives     []driveDTO     `json:"drives"`
		DriveLines []driveLineDTO `json:"drive_lines"`
	}](t, rec)
	if drives.Library != 10 || len(drives.Drives) != 4 || drives.Drives[0].ID != 11 || len(drives.DriveLines) != 4 {
		t.Errorf("drives = %+v", drives)
	}

	for path, code := range map[string]int{
		"/api/v1/config/libraries/99":        http.StatusNotFound,
		"/api/v1/config/libraries/x":         http.StatusBadRequest,
		"/api/v1/config/libraries/99/drives": http.StatusNotFound,
		"/api/v1/config/libraries/99/slots":  http.StatusNotFound,
	} {
		if rec := do(t, cfg, http.MethodGet, path, ""); rec.Code != code {
			t.Errorf("%s: status %d, want %d", path, rec.Code, code)
		}
	}
}

func TestConfigSlots(t *testing.T) {
	dir := genDir(t)
	p := filepath.Join(dir, "library_contents.10")
	b, _ := os.ReadFile(p)
	b = bytes.Replace(b, []byte("Slot 2: E01002L8\n"), nil, 1) // slot 2 becomes a gap
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	type slots struct {
		Live      bool            `json:"live"`
		LiveError string          `json:"live_error"`
		Slots     []configSlotDTO `json:"slots"`
		MAPs      []configSlotDTO `json:"maps"`
	}

	got := decodeBody[slots](t, do(t, Config{ConfigDir: dir}, http.MethodGet, "/api/v1/config/libraries/10/slots", ""))
	if got.Live || got.LiveError == "" || got.Slots[1].Listed || got.Slots[1].Number != 2 || got.Slots[1].LiveBarcode != nil {
		t.Errorf("without a changer: live %v (%q), slot 2 %+v", got.Live, got.LiveError, got.Slots[1])
	}

	live := liveOf(t, dir, 10, func(inv *changer.Inventory) {
		inv.MAPs[1].Full, inv.MAPs[1].Barcode = true, "ZZ0001L8"
	})
	got = decodeBody[slots](t, do(t, Config{ConfigDir: dir, Changers: &fakeChangers{inv: live}}, http.MethodGet, "/api/v1/config/libraries/10/slots", ""))
	if !got.Live || got.Slots[0].LiveBarcode == nil || *got.Slots[0].LiveBarcode != "E01001L8" {
		t.Fatalf("live slots = %+v", got.Slots[:2])
	}
	if got.Slots[1].LiveBarcode == nil || *got.Slots[1].LiveBarcode != "" {
		t.Errorf("gap slot 2 live barcode = %v, want empty", got.Slots[1].LiveBarcode)
	}
	if m := got.MAPs[1]; m.Barcode != "" || m.LiveBarcode == nil || *m.LiveBarcode != "ZZ0001L8" {
		t.Errorf("MAP 2 = %+v", m)
	}
}

func TestConfigPersonalities(t *testing.T) {
	rec := do(t, Config{}, http.MethodGet, "/api/v1/config/personalities", "")
	got := decodeBody[struct {
		Libraries []struct {
			personalityDTO
			Rules []ruleDTO `json:"rules"`
		} `json:"libraries"`
		Drives     []driveModelDTO `json:"drives"`
		TargetApps []targetAppDTO  `json:"target_apps"`
	}](t, rec)
	var ibm bool
	for _, l := range got.Libraries {
		if l.InitFn == "init_ibm3584" {
			ibm = true
			if l.Limits.Drives != 512 || l.Limits.MAPs != 255 || !l.VPD83 || len(l.Rules) != 1 || l.Rules[0].ProductPrefix != "03584" {
				t.Errorf("init_ibm3584 = %+v", l)
			}
		}
	}
	if !ibm {
		t.Error("init_ibm3584 missing")
	}
	var td8 bool
	for _, d := range got.Drives {
		if d.Product == "ULT3580-TD8" {
			td8 = true
			if !d.WORM || len(d.Media) == 0 || (d.Media[0].Load != "rw" && d.Media[0].Load != "ro") {
				t.Errorf("ULT3580-TD8 = %+v", d)
			}
		}
	}
	if !td8 {
		t.Error("ULT3580-TD8 missing")
	}
	if len(got.TargetApps) != 1 || got.TargetApps[0].ID != validate.PresetPBS {
		t.Errorf("target_apps = %+v", got.TargetApps)
	}
}

func validateReq(t *testing.T, cfg Config, body string) validateResponse {
	t.Helper()
	rec := do(t, cfg, http.MethodPost, "/api/v1/config/validate", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	return decodeBody[validateResponse](t, rec)
}

// The plan's verify step for 2.4: the shipped STK L700 checked against the
// PBS preset gives exactly one blocking finding.
func TestValidateShippedL700WithPBS(t *testing.T) {
	for name, dir := range map[string]string{"shipped": writeConfigDir(t), "generated": genDir(t)} {
		t.Run(name, func(t *testing.T) {
			got := validateReq(t, Config{ConfigDir: dir, SysRoot: noSys(t)}, `{"target_apps":{"10":"pbs"}}`)
			if got.OK || len(got.Errors) != 1 || got.Errors[0].Code != validate.CodePresetUnmet || got.Errors[0].Library != 10 {
				t.Errorf("errors = %+v; want one preset.unmet for library 10", got.Errors)
			}
			got = validateReq(t, Config{ConfigDir: dir, SysRoot: noSys(t)}, `{}`)
			if !got.OK {
				t.Errorf("without a target application: errors %+v", got.Errors)
			}
		})
	}
}

// A request that leaves library_contents.10 out proposes the live view of
// it, so a device.conf edit moves no cartridge; the same edit sent with the
// file as it is on disk reverts the run-time import, which would make the
// imported cartridge leave the library.
func TestValidateBuildsOnTheLiveView(t *testing.T) {
	dir := genDir(t)
	// The imported cartridge is not in the (missing) media home: being in the
	// live MAP, it needs no check there.
	dc, _ := os.ReadFile(filepath.Join(dir, "device.conf"))
	live := liveOf(t, dir, 10, func(inv *changer.Inventory) {
		inv.MAPs[0].Full, inv.MAPs[0].Barcode = true, "ZZ0001L8"
	})
	fc := &errByLib{&fakeChangers{invByLib: map[int]changer.Inventory{10: live}}, map[int]error{30: fmt.Errorf("no medium changer")}}
	cfg := Config{ConfigDir: dir, SysRoot: noSys(t), Changers: fc}
	edited := strings.Replace(string(dc), "Product revision level: HB81", "Product revision level: HB82", 1)
	lc, _ := os.ReadFile(filepath.Join(dir, "library_contents.10"))

	leaves := func(r validateResponse) bool {
		for _, f := range r.Errors {
			if f.Code == validate.CodeRemovalLeaves && strings.Contains(f.Message, "ZZ0001L8") {
				return true
			}
		}
		return false
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{"device.conf": edited}})
	if r := validateReq(t, cfg, string(body)); !r.OK || leaves(r) {
		t.Errorf("device.conf only: errors %+v", r.Errors)
	}
	body, _ = json.Marshal(map[string]any{"files": map[string]string{"device.conf": edited, "library_contents.10": string(lc)}})
	if r := validateReq(t, cfg, string(body)); !leaves(r) {
		t.Errorf("stale library_contents.10: errors %+v; want ZZ0001L8 to leave", r.Errors)
	}
}

// Validation reads the proposal and never writes a file.
func TestValidateNeverWrites(t *testing.T) {
	dir := genDir(t)
	before := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		before[e.Name()] = conffile.Hash(b)
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]any{
		"device.conf":         "VERSION: 5\n",
		"library_contents.10": nil,
		"library_contents.30": "VERSION: 2\nSlot 1: BAD\n",
	}})
	validateReq(t, Config{ConfigDir: dir, SysRoot: noSys(t)}, string(body))
	ents, _ = os.ReadDir(dir)
	if len(ents) != len(before) {
		t.Errorf("%d files after, %d before", len(ents), len(before))
	}
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if before[e.Name()] != conffile.Hash(b) {
			t.Errorf("%s changed", e.Name())
		}
	}
}

// Problems of the proposed files come back with their file and line, and
// block.
func TestValidateReportsProposedProblems(t *testing.T) {
	dir := genDir(t)
	body, _ := json.Marshal(map[string]any{"files": map[string]string{"library_contents.30": "VERSION: 2\nSlot x: A\n"}})
	r := validateReq(t, Config{ConfigDir: dir, SysRoot: noSys(t)}, string(body))
	if r.OK || len(r.Problems) != 1 || r.Problems[0].File != "library_contents.30" || r.Problems[0].Line != 2 {
		t.Errorf("ok %v, problems %+v", r.OK, r.Problems)
	}
}

func TestValidateRequestBounds(t *testing.T) {
	dir := genDir(t)
	cfg := Config{ConfigDir: dir, SysRoot: noSys(t)}
	many := map[string]string{}
	for i := 0; i <= maxValidateFiles; i++ {
		many[fmt.Sprintf("library_contents.%d", i)] = ""
	}
	tooMany, _ := json.Marshal(map[string]any{"files": many})
	big, _ := json.Marshal(map[string]any{"files": map[string]string{"device.conf": strings.Repeat("#", maxValidateFileSize+1)}})
	huge, _ := json.Marshal(map[string]any{"files": map[string]string{
		"library_contents.10": strings.Repeat("#", maxValidateFileSize),
		"library_contents.30": strings.Repeat("#", maxValidateFileSize),
		"device.conf":         strings.Repeat("#", maxValidateFileSize),
		"library_contents.40": strings.Repeat("#", maxValidateFileSize),
	}})
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"not JSON":                {`{`, http.StatusBadRequest},
		"unknown field":           {`{"file":{}}`, http.StatusBadRequest},
		"other file":              {`{"files":{"mhvtl.conf":"x"}}`, http.StatusBadRequest},
		"path":                    {`{"files":{"../device.conf":"x"}}`, http.StatusBadRequest},
		"device.conf deleted":     {`{"files":{"device.conf":null}}`, http.StatusBadRequest},
		"unknown library":         {`{"files":{"library_contents.99":"VERSION: 2\n"}}`, http.StatusBadRequest},
		"leading zero":            {`{"files":{"library_contents.010":"VERSION: 2\n"}}`, http.StatusBadRequest},
		"trailing data":           {`{} x`, http.StatusBadRequest},
		"a second object":         {`{}{}`, http.StatusBadRequest},
		"trailing blanks":         {"{}\n\t ", http.StatusOK},
		"bad target app key":      {`{"target_apps":{"ten":"pbs"}}`, http.StatusBadRequest},
		"too many files":          {string(tooMany), http.StatusBadRequest},
		"file over 256 KiB":       {string(big), http.StatusBadRequest},
		"body over 1 MiB":         {string(huge), http.StatusRequestEntityTooLarge},
		"a library_contents file": {`{"files":{"library_contents.30":"VERSION: 2\n"}}`, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(t, cfg, http.MethodPost, "/api/v1/config/validate", tc.body); rec.Code != tc.code {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.code, rec.Body)
			}
		})
	}
}

// Every config route sits behind the API token.
func TestConfigRoutesNeedToken(t *testing.T) {
	cfg := Config{ConfigDir: genDir(t), SysRoot: noSys(t), Token: "s3cret"}
	for _, r := range [][2]string{
		{http.MethodGet, "/api/v1/config"},
		{http.MethodGet, "/api/v1/config/capabilities"},
		{http.MethodGet, "/api/v1/config/personalities"},
		{http.MethodGet, "/api/v1/config/libraries"},
		{http.MethodGet, "/api/v1/config/libraries/10"},
		{http.MethodGet, "/api/v1/config/libraries/10/drives"},
		{http.MethodGet, "/api/v1/config/libraries/10/slots"},
		{http.MethodPost, "/api/v1/config/validate"},
	} {
		body := ""
		if r[0] == http.MethodPost {
			body = "{}"
		}
		if rec := do(t, cfg, r[0], r[1], body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d", r[0], r[1], rec.Code)
		}
		req := httptest.NewRequest(r[0], r[1], strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		NewServer(cfg).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s with token: %d %s", r[0], r[1], rec.Code, rec.Body)
		}
	}
}

// No write route exists yet (AC 17): every other method on the config paths
// is refused.
func TestConfigHasNoWriteRoutes(t *testing.T) {
	cfg := Config{ConfigDir: genDir(t)}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		for _, p := range []string{"/api/v1/config", "/api/v1/config/libraries", "/api/v1/config/libraries/10",
			"/api/v1/config/libraries/10/drives", "/api/v1/config/libraries/10/slots", "/api/v1/config/apply"} {
			if rec := do(t, cfg, m, p, "{}"); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", m, p, rec.Code)
			}
		}
	}
}

func TestConfigCapabilities(t *testing.T) {
	dir, err := os.MkdirTemp("", "caps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	paths := CapabilityPaths{
		HelperSocket: filepath.Join(dir, "apply.sock"),
		LegacyEnv:    filepath.Join(dir, "mhvtl-control.env"),
		Generators:   []string{filepath.Join(dir, "missing"), filepath.Join(dir, "gen")},
		MakeVTLMedia: filepath.Join(dir, "make_vtl_media"),
	}
	get := func(cfg Config) capabilitiesDTO {
		t.Helper()
		cfg.Capabilities = paths
		return decodeBody[capabilitiesDTO](t, do(t, cfg, http.MethodGet, "/api/v1/config/capabilities", ""))
	}
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := get(Config{}), (capabilitiesDTO{Helper: "absent", Writes: "helper_absent"}); got != want {
		t.Errorf("nothing installed: %+v, want %+v", got, want)
	}

	// A regular file where the socket goes is not the helper.
	write("apply.sock", "", 0o600)
	if got := get(Config{}); got.Helper != "absent" {
		t.Errorf("file at the socket path: helper %q", got.Helper)
	}
	os.Remove(paths.HelperSocket)
	ln, err := net.Listen("unix", paths.HelperSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := get(Config{}); got.Helper != "present" || got.Writes != "ok" {
		t.Errorf("socket listening: %+v", got)
	}
	if got := get(Config{InsecureListen: true}); got.Writes != "tls_required" {
		t.Errorf("non-loopback without TLS: writes %q", got.Writes)
	}
	write("mhvtl-control.env", "", 0o600)
	if got := get(Config{}); got.Helper != "legacy_env_blocks" {
		t.Errorf("legacy env file: helper %q", got.Helper)
	}

	write("gen", "#!/bin/sh\n", 0o644)
	if get(Config{}).Generator {
		t.Error("a generator that is not executable counts")
	}
	os.Chmod(filepath.Join(dir, "gen"), 0o755)
	if !get(Config{}).Generator {
		t.Error("installed generator not found")
	}

	write("make_vtl_media", "#!/bin/bash\necho \"  [-H|--home-dir MHVTL_HOME_PATH]\"\n", 0o755)
	if get(Config{}).PerLibraryHomeMedia {
		t.Error("make_vtl_media before #276 counts as per-library")
	}
	write("make_vtl_media", "#!/bin/bash\necho \"  [--per-library-home]   -- create each library's media\"\n", 0o755)
	if !get(Config{}).PerLibraryHomeMedia {
		t.Error("make_vtl_media with --per-library-home not detected")
	}
	// A bare name is looked up in PATH.
	t.Setenv("PATH", dir)
	paths.MakeVTLMedia = "make_vtl_media"
	if !get(Config{}).PerLibraryHomeMedia {
		t.Error("make_vtl_media not found in PATH")
	}
}

func TestLiveContents(t *testing.T) {
	data := []byte("VERSION: 2\n\nDrive 1:\n\nPicker 1: P00001L8\n\nMAP 1:\nMAP 2: M00001L8\n\nSlot 1: A00001L8\nSlot 3: A00003L8\n")
	inv, err := config.ReadInventory("", func(name string) ([]byte, error) {
		if name == "device.conf" {
			return []byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n"), nil
		}
		return data, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live := validate.Placement{
		Slots:  map[int]string{2: "A00001L8", 3: "A00003L8"}, // slot 1 moved into the gap
		MAPs:   map[int]string{1: "M00001L8"},                // moved from MAP 2 to MAP 1
		Drives: map[int]string{1: "P00001L8"},                // the picker's cartridge is in drive 1
	}
	got, err := liveContents(data, inv.Libraries[0], live)
	if err != nil {
		t.Fatal(err)
	}
	want := "VERSION: 2\n\nDrive 1:\n\nPicker 1:\n\nMAP 1: M00001L8\nMAP 2:\n\nSlot 1:\nSlot 2: A00001L8\nSlot 3: A00003L8\n"
	if string(got) != want {
		t.Errorf("liveContents =\n%s\nwant\n%s", got, want)
	}
}

// gatedChangers counts READ ELEMENT STATUS per library, holding each
// until release is closed, so concurrent callers overlap.
type gatedChangers struct {
	fakeChangers
	release chan struct{}
	mu      sync.Mutex
	reads   map[int]int
}

func (c *gatedChangers) Inventory(lib config.Library) (changer.Inventory, error) {
	c.mu.Lock()
	c.reads[lib.ID]++
	c.mu.Unlock()
	<-c.release
	return changer.Inventory{}, nil
}

// The library list, the media lists, GET /config, the slots route and
// validation share one READ ELEMENT STATUS per library: concurrent calls
// wait for the read in progress, and later ones within the TTL reuse it.
func TestConfigSharesLiveReads(t *testing.T) {
	dir := genDir(t)
	cc := &gatedChangers{release: make(chan struct{}), reads: map[int]int{}}
	h := NewServer(Config{ConfigDir: dir, SysRoot: noSys(t), Changers: cc}).Handler()
	call := func(method, path, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	// What a dashboard refresh calls.
	reqs := [][3]string{
		{http.MethodGet, "/api/v1/libraries", ""},
		{http.MethodGet, "/api/v1/libraries/10/media", ""},
		{http.MethodGet, "/api/v1/libraries/30/media", ""},
		{http.MethodGet, "/api/v1/config", ""},
		{http.MethodPost, "/api/v1/config/validate", "{}"},
		{http.MethodGet, "/api/v1/config/libraries/10/slots", ""},
		{http.MethodGet, "/api/v1/config", ""},
	}
	var wg sync.WaitGroup
	codes := make([]int, len(reqs))
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = call(r[0], r[1], r[2])
		}()
	}
	// Let every request reach the changer read before it returns.
	time.Sleep(100 * time.Millisecond)
	close(cc.release)
	wg.Wait()
	call(http.MethodPost, "/api/v1/config/validate", "{}") // within the TTL
	call(http.MethodGet, "/api/v1/libraries", "")
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("%s %s: %d", reqs[i][0], reqs[i][1], c)
		}
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.reads[10] != 1 || cc.reads[30] != 1 {
		t.Errorf("READ ELEMENT STATUS per library = %v, want one each", cc.reads)
	}
}

// A read older than the TTL is not reused.
func TestLiveCacheExpires(t *testing.T) {
	c := liveCache{ttl: time.Millisecond}
	n := 0
	read := func() (changer.Inventory, error) { n++; return changer.Inventory{}, nil }
	lib := config.Library{ID: 10, Serial: "S"}
	ctx := context.Background()
	c.get(ctx, lib, read)
	c.get(ctx, lib, read)
	time.Sleep(5 * time.Millisecond)
	c.get(ctx, lib, read)
	lib.Serial = "T" // another changer
	c.get(ctx, lib, read)
	c.forget(10)
	c.get(ctx, lib, read)
	if n != 4 {
		t.Errorf("%d reads, want 4", n)
	}
}

// A read that panics still releases its waiters, with an error, and is not
// kept: the next caller reads again.
func TestLiveCacheReadPanics(t *testing.T) {
	var c liveCache
	lib := config.Library{ID: 10}
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		c.get(ctx, lib, func() (changer.Inventory, error) {
			close(started)
			<-release
			panic("changer read")
		})
	}()
	<-started
	waited := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, lib, func() (changer.Inventory, error) {
			t.Error("a waiter read the changer itself")
			return changer.Inventory{}, nil
		})
		waited <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the waiter reach the read in progress
	close(release)
	if p := <-panicked; p != "changer read" {
		t.Errorf("panic = %v, want it passed on", p)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Error("the waiter got no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter hangs")
	}
	n := 0
	c.get(ctx, lib, func() (changer.Inventory, error) { n++; return changer.Inventory{}, nil })
	if n != 1 {
		t.Error("the failed read was kept")
	}
}

// A caller waiting for another's read gives up with its request.
func TestLiveCacheWaiterCancels(t *testing.T) {
	var c liveCache
	lib := config.Library{ID: 10}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	go c.get(context.Background(), lib, func() (changer.Inventory, error) {
		close(started)
		<-release
		return changer.Inventory{}, nil
	})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, lib, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A move through this daemon drops the library's shared read: GET /config
// right after a mount shows the cartridge in the drive.
func TestMoveDropsLiveRead(t *testing.T) {
	dir := genDir(t)
	fc := &fakeChangers{invByLib: map[int]changer.Inventory{10: liveOf(t, dir, 10, nil)}}
	h := NewServer(Config{ConfigDir: dir, SysRoot: noSys(t), Changers: fc}).Handler()
	drives := func() []liveDriveDTO {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
		return decodeBody[struct {
			Slots map[string]slotsViewDTO `json:"slots"`
		}](t, rec).Slots["10"].Drives
	}
	if d := drives(); len(d) != 0 {
		t.Fatalf("drives before the mount = %+v", d)
	}
	fc.mu.Lock()
	fc.invByLib[10] = liveOf(t, dir, 10, func(inv *changer.Inventory) {
		inv.Slots[0].Full, inv.Slots[0].Barcode = false, ""
		inv.Drives[0].Full, inv.Drives[0].Barcode = true, "E01001L8"
	})
	fc.mu.Unlock()
	if rec := postJSON(t, h, "/api/v1/libraries/10/mount", `{"drive":11,"slot":1}`); rec.Code != http.StatusOK {
		t.Fatalf("mount: %d %s", rec.Code, rec.Body)
	}
	if d := drives(); !reflect.DeepEqual(d, []liveDriveDTO{{1, "E01001L8"}}) {
		t.Errorf("drives after the mount = %+v", d)
	}
}

// A running library holding cartridges in slots library_contents does not
// list is reported: the file changed after the library started.
func TestConfigSlotsWarnsOfOtherSlots(t *testing.T) {
	dir := genDir(t)
	live := liveOf(t, dir, 10, func(inv *changer.Inventory) {
		n := inv.Slots[len(inv.Slots)-1].Number
		inv.Slots = append(inv.Slots, changer.SlotState{Number: n + 1, Full: true, Barcode: "ZZ0002L8"})
	})
	fc := &fakeChangers{invByLib: map[int]changer.Inventory{10: live}}
	rec := do(t, Config{ConfigDir: dir, Changers: fc}, http.MethodGet, "/api/v1/config/libraries/10/slots", "")
	sv := decodeBody[slotsViewDTO](t, rec)
	if !strings.Contains(sv.Warning, "(ZZ0002L8)") || !strings.Contains(sv.Warning, "library_contents.10") {
		t.Errorf("warning = %q", sv.Warning)
	}
}
