package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/ipc"
	"mhvtl-control/internal/media"
)

// fakeMedia is a MediaService that records calls and creates the cartridge
// directory, as mktape would.
type fakeMedia struct {
	calls   []string
	err     error
	failOn  string
	created []string
}

func (f *fakeMedia) Create(_ context.Context, lib int, home string, s media.Spec) error {
	f.calls = append(f.calls, fmt.Sprintf("Create lib=%d %s %s %s %d", lib, s.Barcode, s.Type, s.Density, s.SizeMB))
	if s.Barcode == f.failOn {
		return &media.ToolError{Tool: "mktape", Err: errors.New("exit status 1"), Output: "error: boom"}
	}
	f.created = append(f.created, s.Barcode)
	return os.Mkdir(filepath.Join(home, s.Barcode), 0o755)
}

func (f *fakeMedia) Edit(_ context.Context, lib int, home, barcode string, c media.Change) error {
	desc := []string{fmt.Sprintf("Edit lib=%d %s", lib, barcode)}
	if c.WriteProtect != nil {
		desc = append(desc, fmt.Sprintf("wp=%v", *c.WriteProtect))
	}
	if c.Type != nil {
		desc = append(desc, "type="+*c.Type)
	}
	if c.Density != nil {
		desc = append(desc, "density="+*c.Density)
	}
	if c.SizeMB != nil {
		desc = append(desc, fmt.Sprintf("size=%d", *c.SizeMB))
	}
	f.calls = append(f.calls, strings.Join(desc, " "))
	return f.err
}

// homeOf returns the Home directory writeConfigDirWithMedia gave library 10.
func homeOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "device.conf"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*Home directory:\s*(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no Home directory in device.conf")
	}
	return string(m[1])
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestMediaListLocations(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "A00002L8", "SHELF1L8")
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: 1, Full: true, Barcode: "A00002L8"}},
		Slots:  []changer.SlotState{{Number: 1, Full: true, Barcode: "A00001L8"}, {Number: 2, Full: true, Barcode: "NOMEDIA1"}},
	}}
	h := NewServer(Config{ConfigDir: dir, Changers: fc}).Handler()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		HomeDir    string   `json:"home_dir"`
		Densities  []string `json:"densities"`
		DataOnly   []string `json:"data_only_densities"`
		Live       bool     `json:"live"`
		Cartridges []struct {
			Barcode    string       `json:"barcode"`
			Missing    string       `json:"missing"`
			MissingWhy string       `json:"missing_why"`
			Location   *locationDTO `json:"location"`
		} `json:"cartridges"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.HomeDir != homeOf(t, dir) || !resp.Live || len(resp.Densities) == 0 || strings.Join(resp.DataOnly, ",") != "LTO8M8" {
		t.Errorf("resp = %+v", resp)
	}
	want := map[string]locationDTO{
		"A00001L8": {Kind: "slot", Number: 1},
		"A00002L8": {Kind: "drive", Number: 1},
		"SHELF1L8": {Kind: "shelf"},
		"NOMEDIA1": {Kind: "slot", Number: 2},
	}
	if len(resp.Cartridges) != len(want) {
		t.Fatalf("cartridges = %+v", resp.Cartridges)
	}
	for _, c := range resp.Cartridges {
		if c.Location == nil || *c.Location != want[c.Barcode] {
			t.Errorf("%s: location = %+v, want %+v", c.Barcode, c.Location, want[c.Barcode])
		}
		wantMissing := map[string]string{"NOMEDIA1": media.MissingDirectory}[c.Barcode]
		if c.Missing != wantMissing || (wantMissing != "") != strings.Contains(c.MissingWhy, "no media in "+resp.HomeDir) {
			t.Errorf("%s: missing = %q (%q), want %q", c.Barcode, c.Missing, c.MissingWhy, wantMissing)
		}
	}

	// Without the changer, locations are unknown rather than guessed.
	h = NewServer(Config{ConfigDir: dir, Changers: &fakeChangers{invErr: errors.New("no changer")}}).Handler()
	rec = doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"location"`) ||
		!strings.Contains(rec.Body.String(), `"live_error":"no changer"`) {
		t.Errorf("not live: %d %s", rec.Code, rec.Body.String())
	}
	// It still lists what library_contents.10 places without media.
	if !strings.Contains(rec.Body.String(), `"missing":"directory_absent"`) {
		t.Errorf("not live: no missing cartridge from library_contents: %s", rec.Body.String())
	}
}

func TestMediaListPersistFallback(t *testing.T) {
	dir := writeConfigDirWithMedia(t)
	conf := filepath.Join(dir, "device.conf")
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// PERSIST on for library 10 only (its stanza comes first).
	if err := os.WriteFile(conf, bytes.Replace(b, []byte("PERSIST: False"), []byte("PERSIST: True"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	// The .persist vtllibrary saved at its last exit holds a cartridge
	// imported at run time, which library_contents.10 does not list.
	persist := filepath.Join(dir, "library_contents.10.persist")
	if err := os.WriteFile(persist, []byte("Slot 1: IMPORT1L8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(persist, later, later); err != nil {
		t.Fatal(err)
	}
	h := NewServer(Config{ConfigDir: dir, Changers: &fakeChangers{invErr: errors.New("no changer")}}).Handler()
	body := doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/media", "").Body.String()
	if !strings.Contains(body, `"barcode":"IMPORT1L8"`) || strings.Contains(body, `"barcode":"E01001L8"`) {
		t.Errorf("not the .persist placement: %s", body)
	}
}

func TestMediaHomeFallback(t *testing.T) {
	dir := writeConfigDir(t)
	conf := filepath.Join(dir, "device.conf")
	b, _ := os.ReadFile(conf)
	b = regexp.MustCompile(`(?m)^\s*Home directory:.*\n`).ReplaceAll(b, nil)
	if err := os.WriteFile(conf, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{ConfigDir: dir, DefaultHome: "/srv/default"})
	lib := mustLib(t, dir, 10)
	if got := s.homeFor(lib); got != "/srv/default" {
		t.Errorf("no mhvtl.conf: home = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "mhvtl.conf"), []byte("# x\nMHVTL_DEBUG=0\nMHVTL_HOME_PATH=/srv/tapes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.homeFor(lib); got != "/srv/tapes" {
		t.Errorf("mhvtl.conf: home = %q", got)
	}
}

func TestMediaCreateRange(t *testing.T) {
	dir := writeConfigDirWithMedia(t)
	fm := &fakeMedia{}
	h := NewServer(Config{ConfigDir: dir, Media: fm}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/media",
		`{"prefix":"E01","start":8,"count":3,"width":3,"suffix":"L8","type":"WORM","density":"LTO8","size_mb":100}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	want := []string{
		"Create lib=10 E01008L8 WORM LTO8 100",
		"Create lib=10 E01009L8 WORM LTO8 100",
		"Create lib=10 E01010L8 WORM LTO8 100",
	}
	if strings.Join(fm.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q", fm.calls)
	}
	if !strings.Contains(rec.Body.String(), `"created":["E01008L8","E01009L8","E01010L8"]`) {
		t.Errorf("body = %s", rec.Body.String())
	}

	// Default width is the digits of the last number; default type is data.
	fm = &fakeMedia{}
	h = NewServer(Config{ConfigDir: dir, Media: fm}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"prefix":"T","start":9,"count":2,"density":"LTO6"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.calls, "|") != "Create lib=10 T09 data LTO6 0|Create lib=10 T10 data LTO6 0" {
		t.Errorf("calls = %q", fm.calls)
	}
}

func TestMediaCreateValidation(t *testing.T) {
	cases := []string{
		`{"density":"LTO8"}`,
		`{"barcode":"A B","density":"LTO8"}`,
		`{"barcode":"A1","prefix":"X","count":1,"density":"LTO8"}`,
		`{"barcode":"A1","density":"LTO99"}`,
		`{"barcode":"A1","density":"LTO8","type":"worm"}`,
		`{"prefix":"A","count":501,"density":"LTO8"}`,
		`{"prefix":"A","count":1,"start":-1,"density":"LTO8"}`,
		`{"prefix":"ABCDEFGHIJKLMN","count":200,"suffix":"L8","density":"LTO8"}`,
		`{"prefix":"A-","count":1,"density":"LTO8"}`,
		`{"barcode":"A1M8","density":"LTO8M8","type":"WORM"}`,
		`{"barcode":"A1M8","density":"LTO8M8","type":"clean"}`,
		`{"barcode":"A1M8","density":"LTO8M8","type":"NULL"}`,
		`{`,
	}
	for _, body := range cases {
		fm := &fakeMedia{}
		h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: fm}).Handler()
		if rec := postJSON(t, h, "/api/v1/libraries/10/media", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
		if len(fm.calls) != 0 {
			t.Errorf("%s: tool ran: %q", body, fm.calls)
		}
	}
	fm := &fakeMedia{}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: fm}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"barcode":"A1M8","density":"LTO8M8"}`); rec.Code != http.StatusCreated {
		t.Errorf("data M8: status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Join(fm.calls, "|") != "Create lib=10 A1M8 data LTO8M8 0" {
		t.Errorf("data M8: calls = %q", fm.calls)
	}
	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t)}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"barcode":"A1","density":"LTO8"}`); rec.Code != 503 {
		t.Errorf("no media tools: status = %d, want 503", rec.Code)
	}
}

func TestMediaCreateRefusesExistingBeforeCreatingAny(t *testing.T) {
	fm := &fakeMedia{}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t, "E01002L8", "E01003L8"), Media: fm}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"prefix":"E01","start":1,"count":4,"width":3,"suffix":"L8","density":"LTO8"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "E01002L8, E01003L8") {
		t.Errorf("body does not list the existing barcodes: %s", rec.Body.String())
	}
	if len(fm.calls) != 0 {
		t.Errorf("created some media before refusing: %q", fm.calls)
	}
}

func TestMediaCreateToolFailure(t *testing.T) {
	fm := &fakeMedia{failOn: "A02"}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: fm}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"prefix":"A","start":1,"count":3,"width":2,"density":"LTO8"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "error: boom") || !strings.Contains(body, "created before the failure: A01") {
		t.Errorf("body = %s", body)
	}
	if len(fm.calls) != 2 {
		t.Errorf("kept going after a failure: %q", fm.calls)
	}
}

func TestMediaCreatePlace(t *testing.T) {
	fm := &fakeMedia{}
	ctl := &ipc.FakeController{}
	fc := &fakeChangers{move: changer.Move{From: changer.Location{Kind: "map", Number: 1}, To: changer.Location{Kind: "slot", Number: 5}}}
	h := NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: fm, Controller: ctl, Changers: fc}).Handler()
	rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"prefix":"P","start":1,"count":2,"density":"LTO8","place":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(ctl.Calls) != 2 || ctl.Calls[0].Arg != "P1" || ctl.Calls[1].Arg != "P2" {
		t.Errorf("imports = %+v", ctl.Calls)
	}
	if strings.Join(fc.calls, "|") != "Store lib=10 P1|Store lib=10 P2" {
		t.Errorf("changer calls = %q", fc.calls)
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("place reported an error: %s", rec.Body.String())
	}

	// A failed store stops placing; creation still succeeded.
	fc = &fakeChangers{storeErr: changer.ErrNoEmptySlot}
	ctl = &ipc.FakeController{}
	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: &fakeMedia{}, Controller: ctl, Changers: fc}).Handler()
	rec = postJSON(t, h, "/api/v1/libraries/10/media", `{"prefix":"P","start":1,"count":2,"density":"LTO8","place":true}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "no empty storage slot") || len(ctl.Calls) != 1 {
		t.Errorf("failed place: %d %s, imports %+v", rec.Code, rec.Body.String(), ctl.Calls)
	}

	// Placing needs both the queue and the changer.
	h = NewServer(Config{ConfigDir: writeConfigDirWithMedia(t), Media: &fakeMedia{}, Changers: fc}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"barcode":"P1","density":"LTO8","place":true}`); rec.Code != 503 {
		t.Errorf("no controller: status = %d, want 503", rec.Code)
	}
}

func TestMediaEdit(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "A00002L8")
	fm := &fakeMedia{}
	fc := &fakeChangers{inv: changer.Inventory{
		Drives: []changer.DriveState{{Number: 1, Full: true, Barcode: "A00002L8"}},
	}}
	h := NewServer(Config{ConfigDir: dir, Media: fm, Changers: fc}).Handler()

	rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":true,"size_mb":0,"density":"LTO7","type":"data"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fm.calls) != 1 || fm.calls[0] != "Edit lib=10 A00001L8 wp=true type=data density=LTO7 size=0" {
		t.Errorf("calls = %q", fm.calls)
	}
	if !strings.Contains(rec.Body.String(), `"location":{"kind":"shelf"}`) {
		t.Errorf("body = %s", rec.Body.String())
	}

	cases := []struct {
		path, body string
		status     int
	}{
		{"/api/v1/libraries/10/media/A00002L8", `{"write_protect":true}`, 409}, // in a drive
		{"/api/v1/libraries/10/media/NOPE0001", `{"write_protect":true}`, 404},
		{"/api/v1/libraries/10/media/A00001L8", `{}`, 400},
		{"/api/v1/libraries/10/media/A00001L8", `{"density":"LTO99"}`, 400},
		{"/api/v1/libraries/10/media/A00001L8", `{"type":"tape"}`, 400},
		{"/api/v1/libraries/10/media/A00001L8", `{"density":"LTO8M8","type":"WORM"}`, 400},
		{"/api/v1/libraries/10/media/A00001L8", `{"density":"LTO8M8","type":"clean"}`, 400},
	}
	for _, c := range cases {
		fm.calls = nil
		if rec := doJSON(t, h, http.MethodPatch, c.path, c.body); rec.Code != c.status {
			t.Errorf("%s %s: status = %d, want %d (%s)", c.path, c.body, rec.Code, c.status, rec.Body.String())
		}
		if len(fm.calls) != 0 {
			t.Errorf("%s %s: edit_tape ran", c.path, c.body)
		}
	}

	// Without a live inventory nobody can say the cartridge is not loaded.
	h = NewServer(Config{ConfigDir: dir, Media: fm, Changers: &fakeChangers{invErr: errors.New("gone")}}).Handler()
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":false}`); rec.Code != 503 {
		t.Errorf("no inventory: status = %d, want 503", rec.Code)
	}
	h = NewServer(Config{ConfigDir: dir, Media: fm}).Handler()
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":false}`); rec.Code != 503 {
		t.Errorf("no changer: status = %d, want 503", rec.Code)
	}
	fm.err = &media.ToolError{Tool: "edit_tape", Err: errors.New("exit status 1"), Output: "bad"}
	h = NewServer(Config{ConfigDir: dir, Media: fm, Changers: fc}).Handler()
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":false}`); rec.Code != 500 || !strings.Contains(rec.Body.String(), "bad") {
		t.Errorf("tool failure: %d %s", rec.Code, rec.Body.String())
	}
}

func mustLib(t *testing.T, dir string, id int) config.Library {
	t.Helper()
	inv, err := config.LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range inv.Libraries {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("library %d not in %s", id, dir)
	return config.Library{}
}

func TestMediaEditTypeOnlyOnShelf(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8", "SHELF1L8")
	fm := &fakeMedia{}
	fc := &fakeChangers{inv: changer.Inventory{Slots: []changer.SlotState{{Number: 3, Full: true, Barcode: "A00001L8"}}}}
	h := NewServer(Config{ConfigDir: dir, Media: fm, Changers: fc}).Handler()
	rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"type":"clean"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "export and eject") || len(fm.calls) != 0 {
		t.Errorf("type change in a slot: %d %s, calls %q", rec.Code, rec.Body.String(), fm.calls)
	}
	// Write protect is read from the MAM at load time, so it may change in a slot.
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":true}`); rec.Code != 200 {
		t.Errorf("WP in a slot: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/SHELF1L8", `{"type":"clean"}`); rec.Code != 200 {
		t.Errorf("type change on the shelf: %d %s", rec.Code, rec.Body.String())
	}
}

// Libraries sharing a home directory share media: a cartridge loaded in the
// other library's drive must not be edited either.
func TestMediaEditChecksLibrariesSharingHome(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8")
	fm := &fakeMedia{}
	fc := &fakeChangers{invByLib: map[int]changer.Inventory{
		10: {},
		30: {Drives: []changer.DriveState{{Number: 2, Full: true, Barcode: "A00001L8"}}},
	}}
	h := NewServer(Config{ConfigDir: dir, Media: fm, Changers: fc}).Handler()
	rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"write_protect":true}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "library 30") || len(fm.calls) != 0 {
		t.Errorf("loaded in library 30: %d %s, calls %q", rec.Code, rec.Body.String(), fm.calls)
	}
}

func TestMediaSizeCap(t *testing.T) {
	dir := writeConfigDirWithMedia(t, "A00001L8")
	fm := &fakeMedia{}
	h := NewServer(Config{ConfigDir: dir, Media: fm, Changers: &fakeChangers{}}).Handler()
	if rec := postJSON(t, h, "/api/v1/libraries/10/media", `{"barcode":"B1","density":"LTO8","size_mb":4294967297}`); rec.Code != 400 {
		t.Errorf("create: status = %d, want 400", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/v1/libraries/10/media/A00001L8", `{"size_mb":4294967297}`); rec.Code != 400 {
		t.Errorf("edit: status = %d, want 400", rec.Code)
	}
	if len(fm.calls) != 0 {
		t.Errorf("tool ran: %q", fm.calls)
	}
}
