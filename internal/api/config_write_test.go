package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/validate"
)

// writeEnv is a server over genDir with a fake helper and changers: library
// 10's live view has E01001L8 in drive 1 (slot 1 empty) and ZZ0001L8
// imported into MAP 1; library 30's changer cannot be read.
type writeEnv struct {
	t       *testing.T
	dir     string
	socket  string
	h       *fakeHelper
	srv     *Server
	handler http.Handler
	clock   time.Time
}

func newWriteEnv(t *testing.T, edit func(*Config)) *writeEnv {
	t.Helper()
	dir := genDir(t)
	sockDir, err := os.MkdirTemp("", "write")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	e := &writeEnv{t: t, dir: dir, socket: filepath.Join(sockDir, "apply.sock")}
	e.h = newFakeHelper(t, e.socket)
	live := liveOf(t, dir, 10, func(inv *changer.Inventory) {
		inv.Slots[0].Full, inv.Slots[0].Barcode = false, ""
		inv.Drives[0].Full, inv.Drives[0].Barcode = true, "E01001L8"
		inv.MAPs[0].Full, inv.MAPs[0].Barcode = true, "ZZ0001L8"
	})
	fc := &fakeChangers{invByLib: map[int]changer.Inventory{10: live}}
	cfg := Config{
		ConfigDir: dir,
		Changers:  &errByLib{fakeChangers: fc, fail: map[int]error{30: fmt.Errorf("no medium changer")}},
		Helper:    &applyclient.Client{Socket: e.socket},
	}
	if edit != nil {
		edit(&cfg)
	}
	e.srv = NewServer(cfg)
	e.clock = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	e.srv.now = func() time.Time { return e.clock }
	e.handler = e.srv.Handler()
	return e
}

func (e *writeEnv) call(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, strings.NewReader(string(b)))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

// edit makes a staging request that must succeed and returns the stage.
func (e *writeEnv) edit(method, path string, body any) stageDTO {
	e.t.Helper()
	rec := e.call(method, path, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
	return decodeBody[stageDTO](e.t, rec)
}

func (e *writeEnv) disk(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *writeEnv) answer(f func(apply.Request) apply.Response) {
	e.h.mu.Lock()
	e.h.answer = f
	e.h.mu.Unlock()
}

func (e *writeEnv) requests() []apply.Request {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	return append([]apply.Request(nil), e.h.reqs...)
}

func file(t *testing.T, st stageDTO, name string) string {
	t.Helper()
	c, ok := st.Files[name]
	if !ok || c == nil {
		t.Fatalf("stage has no %s: %v", name, st.Files)
	}
	return *c
}

// live10 is library_contents.10 with library 10's live view written in.
func live10(disk string) string {
	s := strings.Replace(disk, "Slot 1: E01001L8\n", "Slot 1:\n", 1)
	return strings.Replace(s, "MAP 1:\n", "MAP 1: ZZ0001L8\n", 1)
}

// The slot routes edit library_contents as the live view has it (plan,
// amendment "Phase 4 client contract for next"), in one stage.
func TestConfigWriteSlots(t *testing.T) {
	e := newWriteEnv(t, nil)
	disk := e.disk("library_contents.10")

	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/25", map[string]any{"barcode": "NEW025L8"})
	if st.Stage == "" || !st.Expires.Equal(e.clock.Add(stageTTL)) {
		t.Fatalf("stage %q expires %v", st.Stage, st.Expires)
	}
	want := strings.Replace(live10(disk), "Slot 25:\n", "Slot 25: NEW025L8\n", 1)
	if got := file(t, st, "library_contents.10"); got != want {
		t.Errorf("pre-fill:\n%s\nwant:\n%s", got, want)
	}
	if _, ok := st.Files["device.conf"]; ok {
		t.Error("device.conf staged, unchanged")
	}
	if got := st.Base["library_contents.10"]; got != conffile.Hash([]byte(disk)) {
		t.Errorf("base %s", got)
	}

	st = e.edit(http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"stage": st.Stage, "kind": "slot", "count": 2})
	want = strings.Replace(want, "Slot 39: F01039L6\n", "Slot 39: F01039L6\nSlot 40:\nSlot 41:\n", 1)
	if got := file(t, st, "library_contents.10"); got != want {
		t.Errorf("add slots:\n%s\nwant:\n%s", got, want)
	}
	st = e.edit(http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"stage": st.Stage, "kind": "map", "count": 1})
	want = strings.Replace(want, "MAP 4:\n", "MAP 4:\nMAP 5:\n", 1)
	if got := file(t, st, "library_contents.10"); got != want {
		t.Errorf("add MAP:\n%s\nwant:\n%s", got, want)
	}

	// Deleting a slot that holds a cartridge records where it goes.
	st = e.edit(http.MethodDelete, "/api/v1/config/libraries/10/slots/39", map[string]any{
		"stage": st.Stage, "disposition": map[string]any{"to": "slot", "slot": 40}})
	want = strings.Replace(want, "Slot 39: F01039L6\n", "", 1)
	if got := file(t, st, "library_contents.10"); got != want {
		t.Errorf("delete slot:\n%s\nwant:\n%s", got, want)
	}
	wantDisp := []validate.Disposition{{Library: 10, Barcode: "F01039L6", To: "slot", Slot: 40}}
	if !reflect.DeepEqual(st.Dispositions, wantDisp) {
		t.Errorf("dispositions %+v", st.Dispositions)
	}

	// An edit that would leave a file mhVTL misreads changes nothing: a
	// barcode twice.
	if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10/slots/2", map[string]any{"stage": st.Stage, "barcode": "NEW025L8"}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "not_writable") {
		t.Errorf("duplicate barcode: %d %s", rec.Code, rec.Body)
	}
	st = e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/3", map[string]any{"stage": st.Stage, "barcode": "E01003L8"})
	if got := file(t, st, "library_contents.10"); got != want {
		t.Errorf("after a refused edit:\n%s\nwant:\n%s", got, want)
	}

	// The live view, not the file, says what a slot holds: slot 1 is empty.
	rec := e.call(http.MethodDelete, "/api/v1/config/libraries/10/slots/1", map[string]any{
		"stage": st.Stage, "disposition": map[string]any{"to": "shelf"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("disposition for an empty slot: %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		method, path string
		body         map[string]any
		code         int
	}{
		{http.MethodPatch, "/api/v1/config/libraries/10/slots/99", map[string]any{"barcode": "X"}, http.StatusNotFound},
		{http.MethodPatch, "/api/v1/config/libraries/10/slots/2", map[string]any{"barcode": "TWO WORDS"}, http.StatusBadRequest},
		{http.MethodPatch, "/api/v1/config/libraries/10/slots/2", map[string]any{"kind": "drive", "barcode": "X"}, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"kind": "slot", "count": 0}, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/config/libraries/99/slots", map[string]any{"kind": "slot", "count": 1}, http.StatusNotFound},
		{http.MethodPatch, "/api/v1/config/libraries/10/slots/2", map[string]any{"stage": "nope", "barcode": "X"}, http.StatusNotFound},
	} {
		if rec := e.call(tc.method, tc.path, tc.body); rec.Code != tc.code {
			t.Errorf("%s %s %v: %d %s, want %d", tc.method, tc.path, tc.body, rec.Code, rec.Body, tc.code)
		}
	}
}

func TestConfigWriteLibrariesAndDrives(t *testing.T) {
	e := newWriteEnv(t, nil)
	dev := e.disk("device.conf")

	// A device.conf edit writes library 10's live view into its
	// library_contents too; library 30's live view is unknown, so its file
	// stays as on disk and is not staged.
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/10", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
	wantDev := strings.Replace(dev, " PERSIST: False\n Backoff: 400\n", " PERSIST: False\n Backoff: 500\n", 1)
	if got := file(t, st, "device.conf"); got != wantDev {
		t.Errorf("patch library:\n%s", got)
	}
	if got := file(t, st, "library_contents.10"); got != live10(e.disk("library_contents.10")) {
		t.Errorf("library_contents.10 not the live view:\n%s", got)
	}
	st = e.edit(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"stage": st.Stage, "attrs": map[string]any{"Backoff": nil}})
	if _, ok := st.Files["library_contents.30"]; ok {
		t.Error("library_contents.30 staged, unchanged")
	}
	if got := file(t, st, "device.conf"); strings.Contains(got, "L80\n Unit serial number: XYZZY_B\n NAA: 30:22:33:44:ab:00:08:00\n Home directory: testdata/gen/home\n PERSIST: False\n Backoff") {
		t.Errorf("Backoff of library 30 not deleted:\n%s", got)
	}

	// A drive: its stanza names the library, and library_contents gets
	// its Drive line.
	st = e.edit(http.MethodPost, "/api/v1/config/libraries/10/drives", map[string]any{
		"stage": st.Stage, "id": 15, "channel": 0, "target": 5, "lun": 0, "slot": 5,
		"attrs": []map[string]string{{"key": "Vendor identification", "value": "IBM"}, {"key": "Product identification", "value": "ULT3580-TD8"}}})
	if got := file(t, st, "device.conf"); !strings.Contains(got, "\nDrive: 15 CHANNEL: 00 TARGET: 05 LUN: 00\n Library ID: 10 Slot: 05\n Vendor identification: IBM\n Product identification: ULT3580-TD8\n") {
		t.Errorf("drive 15 not added:\n%s", got)
	}
	if got := file(t, st, "library_contents.10"); !strings.Contains(got, "Drive 4:\nDrive 5:\n") {
		t.Errorf("Drive 5 line not added:\n%s", got)
	}
	st = e.edit(http.MethodPatch, "/api/v1/config/libraries/10/drives/15", map[string]any{"stage": st.Stage, "attrs": map[string]any{"Product identification": "ULT3580-TD9"}})
	if got := file(t, st, "device.conf"); !strings.Contains(got, " Product identification: ULT3580-TD9\n") {
		t.Errorf("drive 15 not patched:\n%s", got)
	}
	st = e.edit(http.MethodDelete, "/api/v1/config/libraries/10/drives/14", map[string]any{"stage": st.Stage})
	if got := file(t, st, "device.conf"); strings.Contains(got, "Drive: 14 ") {
		t.Errorf("drive 14 not removed:\n%s", got)
	}
	if got := file(t, st, "library_contents.10"); strings.Contains(got, "Drive 4:") {
		t.Errorf("Drive 4 line not removed:\n%s", got)
	}
	for _, tc := range []struct {
		method, path string
		body         map[string]any
		code         int
	}{
		{http.MethodDelete, "/api/v1/config/libraries/10/drives/31", map[string]any{"stage": st.Stage}, http.StatusNotFound},
		{http.MethodPost, "/api/v1/config/libraries/10/drives", map[string]any{"stage": st.Stage, "id": 11, "slot": 6}, http.StatusConflict},
		{http.MethodPost, "/api/v1/config/libraries/10/drives", map[string]any{"stage": st.Stage, "id": 16, "slot": 6,
			"attrs": []map[string]string{{"key": "Library ID", "value": "30 Slot: 06"}}}, http.StatusBadRequest},
		{http.MethodPatch, "/api/v1/config/libraries/10", map[string]any{"stage": st.Stage, "attrs": map[string]any{"Backoff": "1 # x"}}, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/config/libraries", map[string]any{"stage": st.Stage, "id": 30}, http.StatusConflict},
	} {
		if rec := e.call(tc.method, tc.path, tc.body); rec.Code != tc.code {
			t.Errorf("%s %s %v: %d %s, want %d", tc.method, tc.path, tc.body, rec.Code, rec.Body, tc.code)
		}
	}

	// A new library and its library_contents, which must not exist yet.
	st = e.edit(http.MethodPost, "/api/v1/config/libraries", map[string]any{
		"stage": st.Stage, "id": 50, "channel": 0, "target": 9, "lun": 0, "slots": 3, "maps": 1,
		"attrs": []map[string]string{{"key": "Vendor identification", "value": "STK"}}})
	if got := file(t, st, "device.conf"); !strings.Contains(got, "\nLibrary: 50 CHANNEL: 00 TARGET: 09 LUN: 00\n Vendor identification: STK\n") {
		t.Errorf("library 50 not added:\n%s", got)
	}
	wantLC := "VERSION: 2\n\nPicker 1:\n\nMAP 1:\n\nSlot 1:\nSlot 2:\nSlot 3:\n"
	if got := file(t, st, "library_contents.50"); got != wantLC {
		t.Errorf("library_contents.50:\n%q\nwant %q", got, wantLC)
	}
	if b, ok := st.Base["library_contents.50"]; !ok || b != "" {
		t.Errorf("library_contents.50 base %q %v, want empty", b, ok)
	}

	// Deleting a library removes its drives and its library_contents.
	st = e.edit(http.MethodDelete, "/api/v1/config/libraries/30", map[string]any{
		"stage": st.Stage, "dispositions": []map[string]any{{"barcode": "G03001TA", "to": "shelf"}}})
	got := file(t, st, "device.conf")
	for _, h := range []string{"Library: 30 ", "Drive: 31 ", "Drive: 32 ", "Drive: 33 ", "Drive: 34 "} {
		if strings.Contains(got, h) {
			t.Errorf("%q left after deleting library 30", h)
		}
	}
	if c, ok := st.Files["library_contents.30"]; !ok || c != nil {
		t.Errorf("library_contents.30 not deleted: %v", c)
	}
	if got := st.Base["library_contents.30"]; got != conffile.Hash([]byte(e.disk("library_contents.30"))) {
		t.Errorf("library_contents.30 base %s", got)
	}
	if !reflect.DeepEqual(st.Dispositions, []validate.Disposition{{Library: 30, Barcode: "G03001TA", To: "shelf"}}) {
		t.Errorf("dispositions %+v", st.Dispositions)
	}
	// A library new in the stage leaves nothing behind.
	st = e.edit(http.MethodDelete, "/api/v1/config/libraries/50", map[string]any{"stage": st.Stage})
	if _, ok := st.Files["library_contents.50"]; ok {
		t.Errorf("library_contents.50 still staged: %v", st.Files)
	}
}

// staged makes a one-edit stage on library 10.
func (e *writeEnv) staged() stageDTO {
	e.t.Helper()
	return e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/25", map[string]any{"barcode": "NEW025L8"})
}

func TestConfigApply(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()

	rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "plan_required") {
		t.Fatalf("apply without a dry run: %d %s", rec.Code, rec.Body)
	}

	units := []string{"vtllibrary@10.service"}
	e.answer(func(req apply.Request) apply.Response {
		if req.Op == apply.OpApply {
			return apply.Response{Result: apply.ResultApplied, Units: units, Warnings: []string{"slot 25: NEW025L8 created blank"}}
		}
		return apply.Response{Result: apply.ResultValidated, Units: units, Notes: []string{"n"},
			Findings: &validate.Result{Warnings: []validate.Finding{{Code: "w.x", Message: "m"}}},
			Warnings: []string{"slot 25: NEW025L8 has no cartridge in its home; mhvtl-media.service creates it blank"}}
	})
	rec = e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	plan := decodeBody[applyResultDTO](t, rec)
	if plan.Result != apply.ResultValidated || !reflect.DeepEqual(plan.Plan.Units, units) ||
		!reflect.DeepEqual(plan.Plan.OfflineLibraries, []int{10}) || len(plan.Warnings) != 1 ||
		plan.Findings == nil || len(plan.Findings.Warnings) != 1 || !reflect.DeepEqual(plan.Plan.Notes, []string{"n"}) {
		t.Errorf("dry run: %+v", plan)
	}
	reqs := e.requests()
	if len(reqs) != 1 || reqs[0].Op != apply.OpPlan || len(reqs[0].ExpectUnits) != 0 || reqs[0].ForceToken != "" {
		t.Fatalf("plan request: %+v", reqs)
	}
	wantFiles := []apply.File{{Name: "library_contents.10", BaseSHA256: st.Base["library_contents.10"], Content: []byte(file(t, st, "library_contents.10"))}}
	if !reflect.DeepEqual(reqs[0].Files, wantFiles) || reqs[0].Actor == "" {
		t.Errorf("plan request files %+v actor %q", reqs[0].Files, reqs[0].Actor)
	}

	// An edit after the plan needs another.
	st = e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/26", map[string]any{"stage": st.Stage, "barcode": "NEW026L8"})
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "plan_required") {
		t.Fatalf("apply after an edit: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true}); rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	rec = e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[applyResultDTO](t, rec); got.Result != apply.ResultApplied || len(got.Warnings) != 1 {
		t.Errorf("apply: %+v", got)
	}
	reqs = e.requests()
	last := reqs[len(reqs)-1]
	if last.Op != apply.OpApply || !reflect.DeepEqual(last.ExpectUnits, units) || len(last.Files) != 1 {
		t.Errorf("apply request: %+v", last)
	}
	// An applied stage is gone.
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true}); rec.Code != http.StatusNotFound {
		t.Errorf("stage after apply: %d %s", rec.Code, rec.Body)
	}
}

// A plan refused for drive_loaded gives the forced unit set and a token; the
// forced apply expects that set (mhvtl-control#58).
func TestConfigApplyForce(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	forced := []string{"vtllibrary@10.service", "vtltape@11.service"}
	exp := e.clock.Add(5 * time.Minute)
	e.answer(func(req apply.Request) apply.Response {
		switch {
		case req.Op == apply.OpPlan:
			return apply.Response{Result: apply.ResultRefused, Error: apply.ErrDriveLoaded, Message: "drive 1 holds E01001L8",
				Units: forced, ForceToken: "tok", ForceExpires: &exp}
		case req.ForceToken == "tok" && reflect.DeepEqual(req.ExpectUnits, forced):
			return apply.Response{Result: apply.ResultApplied, Units: forced,
				ForcedUnloads: []apply.ForcedUnload{{Library: 10, Drive: 1, Barcode: "E01001L8", SourceSlot: 1}}}
		default:
			return apply.Response{Result: apply.ResultRefused, Error: apply.ErrUnit, Message: "expect_units differ"}
		}
	})
	rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[applyResultDTO](t, rec)
	if got.Error == nil || got.Error.Code != apply.ErrDriveLoaded || got.ForceToken != "tok" || got.ForceExpires == nil ||
		!reflect.DeepEqual(got.Plan.Units, forced) {
		t.Fatalf("drive_loaded plan: %+v", got)
	}

	// Without the token the helper refuses again; the stage must plan again.
	rec = e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), apply.ErrUnit) {
		t.Fatalf("apply without the token: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "force_token": "tok"}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "plan_required") {
		t.Fatalf("apply after a refusal: %d %s", rec.Code, rec.Body)
	}

	e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
	rec = e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "force_token": "tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("forced apply: %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[applyResultDTO](t, rec); got.Result != apply.ResultApplied || len(got.ForcedUnloads) != 1 {
		t.Errorf("forced apply: %+v", got)
	}
}

// Each helper result maps to a status and an error code.
func TestConfigApplyResults(t *testing.T) {
	cur := "abc"
	for _, tc := range []struct {
		resp apply.Response
		code int
		err  string
	}{
		{apply.Response{Result: apply.ResultConflict, File: "library_contents.10", CurrentSHA256: &cur}, http.StatusConflict, "conflict"},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrIntentUnreadable, Message: "recover by hand"}, http.StatusConflict, apply.ErrIntentUnreadable},
		{apply.Response{Result: apply.ResultInProgress}, http.StatusConflict, apply.ResultInProgress},
		{apply.Response{Result: apply.ResultRecovered}, http.StatusConflict, apply.ResultRecovered},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrInvalid, Findings: &validate.Result{Errors: []validate.Finding{{Code: "e.x"}}}}, http.StatusUnprocessableEntity, apply.ErrInvalid},
		{apply.Response{Result: apply.ResultRolledBack, Error: apply.ErrStart, FailedUnit: "vtllibrary@10.service"}, http.StatusInternalServerError, apply.ResultRolledBack},
		{apply.Response{Result: apply.ResultError, Error: apply.ErrInternal}, http.StatusInternalServerError, apply.ErrInternal},
		{apply.Response{Result: "what"}, http.StatusBadGateway, "helper_error"},
	} {
		e := newWriteEnv(t, nil)
		st := e.staged()
		e.answer(func(apply.Request) apply.Response { return tc.resp })
		rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
		got := decodeBody[applyResultDTO](t, rec)
		if rec.Code != tc.code || got.Error == nil || got.Error.Code != tc.err {
			t.Errorf("%+v: %d %s, want %d %s", tc.resp, rec.Code, rec.Body, tc.code, tc.err)
		}
		if tc.resp.Result == apply.ResultConflict && (got.File != "library_contents.10" || got.CurrentSHA256 == nil || *got.CurrentSHA256 != cur) {
			t.Errorf("conflict: %+v", got)
		}
		if tc.resp.Result == apply.ResultRolledBack && got.FailedUnit != "vtllibrary@10.service" {
			t.Errorf("rolled back: %+v", got)
		}
	}
}

func TestConfigWriteGate(t *testing.T) {
	slot := map[string]any{"barcode": "NEW025L8"}
	const path = "/api/v1/config/libraries/10/slots/25"

	e := newWriteEnv(t, func(c *Config) { c.InsecureListen = true })
	if rec := e.call(http.MethodPatch, path, slot); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tls_required") {
		t.Errorf("insecure listen: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": "x", "dry_run": true}); rec.Code != http.StatusForbidden {
		t.Errorf("insecure apply: %d %s", rec.Code, rec.Body)
	}

	e = newWriteEnv(t, func(c *Config) { c.Token = "s3cret" })
	if rec := e.call(http.MethodPatch, path, slot); rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: %d %s", rec.Code, rec.Body)
	}

	e = newWriteEnv(t, func(c *Config) { c.Helper = nil })
	if rec := e.call(http.MethodPatch, path, slot); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "helper_absent") {
		t.Errorf("no helper client: %d %s", rec.Code, rec.Body)
	}

	// The helper goes away mid-session: the apply says so, with the
	// command that enables it, before and after the hello cache expires.
	e = newWriteEnv(t, nil)
	e.answer(func(apply.Request) apply.Response { return apply.Response{Result: apply.ResultValidated} })
	st := e.staged()
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true}); rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	e.h.ln.Close()
	os.Remove(e.socket)
	// Within the hello's 2 s the cache still says present, so the apply
	// finds the helper gone itself, and the next request knows it.
	rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "helper_absent") ||
		!strings.Contains(rec.Body.String(), "systemctl enable --now mhvtl-control-apply.socket") {
		t.Errorf("helper gone: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPatch, path, slot); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "helper_absent") {
		t.Errorf("edit with the helper gone: %d %s", rec.Code, rec.Body)
	}
}

// Stages live in memory for stageTTL after their last use, at most
// maxStages at a time.
func TestConfigStages(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	e.clock = e.clock.Add(stageTTL - time.Second)
	st = e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/26", map[string]any{"stage": st.Stage, "barcode": "NEW026L8"})
	if !st.Expires.Equal(e.clock.Add(stageTTL)) {
		t.Errorf("expiry not extended: %v", st.Expires)
	}
	e.clock = e.clock.Add(stageTTL)
	if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10/slots/27", map[string]any{"stage": st.Stage, "barcode": "X"}); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "stage_not_found") {
		t.Errorf("expired stage: %d %s", rec.Code, rec.Body)
	}

	for i := 0; i < maxStages; i++ {
		e.staged()
	}
	rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10/slots/25", map[string]any{"barcode": "NEW025L8"})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("stage %d: %d %s", maxStages+1, rec.Code, rec.Body)
	}
	e.clock = e.clock.Add(stageTTL)
	e.staged()
}
