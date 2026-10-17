package api

import (
	"bytes"
	"context"
	"log"
	"mhvtl-control/internal/conffile"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/changer"
)

// A stage is bounded in size (maxStageBytes) and in each library's
// highest Slot and MAP number (the personality's windows), so one request
// cannot hold the stage's lock for long (#61 review M1). Library 10 is an
// STK L700: 490 MAP slots.
func TestConfigStageBounds(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	rec := e.call(http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"stage": st.Stage, "kind": "map", "count": 487})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "slots_out_of_range") {
		t.Errorf("MAP 491: %d %s", rec.Code, rec.Body)
	}
	st = e.edit(http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"stage": st.Stage, "kind": "map", "count": 486})
	if !strings.Contains(file(t, st, "library_contents.10"), "\nMAP 490:\n") {
		t.Error("MAP 490 not added")
	}
	// A new library with no vendor is mhVTL's default personality: 256
	// MAP slots.
	rec = e.call(http.MethodPost, "/api/v1/config/libraries", map[string]any{"stage": st.Stage, "id": 50, "target": 9, "maps": 257})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "slots_out_of_range") {
		t.Errorf("new library with 257 MAP slots: %d %s", rec.Code, rec.Body)
	}

	if maxStageBytes != 2<<20 {
		t.Errorf("maxStageBytes = %d", maxStageBytes)
	}
	defer func(n int) { maxStageBytes = n }(maxStageBytes)
	maxStageBytes = 256 << 10
	e = newWriteEnv(t, nil)
	st = e.staged()
	before := st
	refused := 0
	for i := 0; i < 64 && refused < 2; i++ {
		start := time.Now()
		rec := e.call(http.MethodPost, "/api/v1/config/libraries/10/slots", map[string]any{"stage": st.Stage, "count": 4096})
		took := time.Since(start)
		switch {
		case rec.Code == http.StatusOK:
			if refused > 0 {
				t.Fatalf("add %d accepted after a refusal", i)
			}
			before = decodeBody[stageDTO](t, rec)
		case rec.Code == http.StatusRequestEntityTooLarge && strings.Contains(rec.Body.String(), "stage_too_large"):
			refused++
			if took > 5*time.Second {
				t.Errorf("refusal %d took %v", refused, took)
			}
		default:
			t.Fatalf("add %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if refused < 2 {
		t.Fatal("the stage grew past maxStageBytes")
	}
	got := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil)
	if got.Code != http.StatusOK || decodeBody[stageDTO](t, got).Files["library_contents.10"] == nil ||
		*decodeBody[stageDTO](t, got).Files["library_contents.10"] != *before.Files["library_contents.10"] {
		t.Errorf("stage changed by a refused edit: %d", got.Code)
	}
}

// DELETE /config/stages/{id} discards a stage; GET shows one (#61 review
// L1, L4).
func TestConfigStageDiscard(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	rec := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil)
	if got := decodeBody[stageDTO](t, rec); rec.Code != http.StatusOK || got.State != "open" || got.Files["library_contents.10"] == nil {
		t.Errorf("GET stage: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodDelete, "/api/v1/config/stages/"+st.Stage, nil); rec.Code != http.StatusNoContent {
		t.Errorf("discard: %d %s", rec.Code, rec.Body)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		e.call(http.MethodDelete, "/api/v1/config/stages/"+st.Stage, nil),
		e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil),
		e.call(http.MethodPatch, "/api/v1/config/libraries/10/slots/26", map[string]any{"stage": st.Stage, "barcode": "X"}),
	} {
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "stage_not_found") {
			t.Errorf("discarded stage: %d %s", rec.Code, rec.Body)
		}
	}
}

// busy is how many requests use stage id.
func (e *writeEnv) busy(id string) int {
	c := &e.srv.stages
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.m[id]; st != nil {
		return st.busy
	}
	return 0
}

// lockedClock is a clock requests read while the test moves it.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// A second apply of a stage waits for the first and then learns its
// outcome: plan_required when the first did not apply, the tombstone when
// it did. A running apply keeps its stage from the sweep and from a
// discard, and its stage lives stageTTL after the apply ends (#61 review
// L4, L5, L7).
func TestConfigApplyConcurrent(t *testing.T) {
	for _, final := range []string{apply.ResultApplied, apply.ResultRolledBack} {
		e := newWriteEnv(t, nil)
		clock := &lockedClock{t: e.clock}
		e.srv.now = clock.now
		st := e.staged()
		release := make(chan struct{})
		e.answer(func(req apply.Request) apply.Response {
			if req.Op == apply.OpPlan {
				return apply.Response{Result: apply.ResultValidated, Units: []string{"vtllibrary@10.service"}}
			}
			<-release
			if final == apply.ResultApplied {
				return apply.Response{Result: final, Units: []string{"vtllibrary@10.service"}}
			}
			return apply.Response{Result: final, Error: apply.ErrStart, FailedUnit: "vtllibrary@10.service"}
		})
		if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true}); rec.Code != http.StatusOK {
			t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
		}
		first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage}) }()
		for deadline := time.Now().Add(5 * time.Second); ; {
			if reqs := e.requests(); reqs[len(reqs)-1].Op == apply.OpApply {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the apply never reached the helper")
			}
			time.Sleep(time.Millisecond)
		}
		go func() { second <- e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage}) }()
		// The second apply waits for the stage.
		for deadline := time.Now().Add(5 * time.Second); e.busy(st.Stage) < 2; {
			if time.Now().After(deadline) {
				t.Fatal("the second apply never opened the stage")
			}
			time.Sleep(time.Millisecond)
		}
		if rec := e.call(http.MethodDelete, "/api/v1/config/stages/"+st.Stage, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stage_busy") {
			t.Errorf("%s: discard during the apply: %d %s", final, rec.Code, rec.Body)
		}
		// Past the stage's TTL a new stage sweeps expired ones, but not
		// this.
		clock.add(stageTTL + time.Minute)
		e.staged()
		if rec := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil); rec.Code != http.StatusOK || decodeBody[stageDTO](t, rec).State != "busy" {
			t.Errorf("%s: GET during the apply: %d %s", final, rec.Code, rec.Body)
		}
		select {
		case rec := <-second:
			t.Fatalf("%s: second apply did not wait: %d %s", final, rec.Code, rec.Body)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		rec := <-first
		if final == apply.ResultApplied && rec.Code != http.StatusOK || final != apply.ResultApplied && rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: first apply: %d %s", final, rec.Code, rec.Body)
		}
		rec = <-second
		got := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil)
		dto := decodeBody[stageDTO](t, got)
		switch final {
		case apply.ResultApplied:
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stage_applied") {
				t.Errorf("second apply after an applied one: %d %s", rec.Code, rec.Body)
			}
			if got.Code != http.StatusOK || dto.State != "applied" || dto.LastApply == nil || dto.LastApply.Result != apply.ResultApplied {
				t.Errorf("tombstone: %d %s", got.Code, got.Body)
			}
			if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/10/slots/26", map[string]any{"stage": st.Stage, "barcode": "X"}); rec.Code != http.StatusConflict ||
				!strings.Contains(rec.Body.String(), "stage_applied") {
				t.Errorf("edit of an applied stage: %d %s", rec.Code, rec.Body)
			}
		default:
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "plan_required") {
				t.Errorf("second apply after a rollback: %d %s", rec.Code, rec.Body)
			}
			if got.Code != http.StatusOK || dto.State != "open" || dto.LastApply == nil || dto.LastApply.Result != apply.ResultRolledBack ||
				dto.LastApply.FailedUnit != "vtllibrary@10.service" {
				t.Errorf("stage after a rollback: %d %s", got.Code, got.Body)
			}
		}
		// The stage, or its tombstone, lives stageTTL from the apply's
		// end.
		clock.add(stageTTL - time.Second)
		if rec := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil); rec.Code != http.StatusOK {
			t.Errorf("%s: stage before its TTL: %d %s", final, rec.Code, rec.Body)
		}
		clock.add(stageTTL + time.Second)
		if rec := e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: stage after its TTL: %d %s", final, rec.Code, rec.Body)
		}
	}
}

// force_token is for an apply; on a dry run it is refused. An apply
// refused for drive_loaded gives no token: a token comes from a dry run
// only (#61 review L6, L8).
func TestConfigApplyForceTokenUse(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true, "force_token": "tok"}); rec.Code != http.StatusBadRequest {
		t.Errorf("dry run with a token: %d %s", rec.Code, rec.Body)
	}
	exp := e.clock.Add(5 * time.Minute)
	e.answer(func(req apply.Request) apply.Response {
		if req.Op == apply.OpPlan {
			return apply.Response{Result: apply.ResultValidated, Units: []string{"vtllibrary@10.service"}}
		}
		return apply.Response{Result: apply.ResultRefused, Error: apply.ErrDriveLoaded, Message: "drive 1 holds E01001L8",
			Units: []string{"vtllibrary@10.service", "vtltape@11.service"}, ForceToken: "tok", ForceExpires: &exp}
	})
	e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
	rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "force_") {
		t.Errorf("apply refused for drive_loaded: %d %s", rec.Code, rec.Body)
	}
}

// Every configuration write is gated alike: 401 without the bearer, 403
// token_required without a configured token, 403 tls_required on a
// non-loopback listen without TLS (#61 review L7).
func TestConfigWriteRoutesGated(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/config/libraries"},
		{http.MethodPatch, "/api/v1/config/libraries/10"},
		{http.MethodDelete, "/api/v1/config/libraries/10"},
		{http.MethodPost, "/api/v1/config/libraries/10/drives"},
		{http.MethodPatch, "/api/v1/config/libraries/10/drives/11"},
		{http.MethodDelete, "/api/v1/config/libraries/10/drives/11"},
		{http.MethodPost, "/api/v1/config/libraries/10/slots"},
		{http.MethodPatch, "/api/v1/config/libraries/10/slots/1"},
		{http.MethodDelete, "/api/v1/config/libraries/10/slots/1"},
		{http.MethodPost, "/api/v1/config/apply"},
		{http.MethodPatch, "/api/v1/config/system"},
		{http.MethodDelete, "/api/v1/config/stages/0123"},
	}
	anon := newWriteEnv(t, nil)
	anon.anon = true
	noToken := newWriteEnv(t, func(c *Config) { c.Token = "" })
	insecure := newWriteEnv(t, func(c *Config) { c.InsecureListen = true })
	for _, r := range routes {
		if rec := anon.call(r.method, r.path, map[string]any{}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without the bearer: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
		if rec := noToken.call(r.method, r.path, map[string]any{}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_required") {
			t.Errorf("%s %s without a token: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
		if rec := insecure.call(r.method, r.path, map[string]any{}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tls_required") {
			t.Errorf("%s %s without TLS: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if n := len(anon.h.reqs) + len(noToken.h.reqs) + len(insecure.h.reqs); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// A panicking hello is logged with its value; the client gets a generic
// message (#61 review L3).
func TestHelloPanicLogged(t *testing.T) {
	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)
	h := &funcHelper{hello: func(int) (string, error) { panic("boom") }}
	srv := NewServer(Config{Helper: h})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := srv.helperState(ctx)
	if err == nil || strings.Contains(err.Error(), "boom") {
		t.Errorf("hello error: %v", err)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("log: %q", buf.String())
	}
}

// A stage's first edit of a library reads its changer afresh, not from the
// views' cache: a move made outside the daemon just before must not be
// undone (#61 smoke, dispositions). Later edits of the stage keep what the
// first read.
func TestConfigStageReadsLiveFresh(t *testing.T) {
	e := newWriteEnv(t, nil)
	if rec := e.call(http.MethodGet, "/api/v1/config", nil); rec.Code != http.StatusOK { // warms the cache
		t.Fatalf("GET /config: %d", rec.Code)
	}
	e.fc.mu.Lock()
	inv := e.fc.invByLib[10]
	inv.Slots = append([]changer.SlotState(nil), inv.Slots...)
	for i := range inv.Slots {
		switch inv.Slots[i].Number {
		case 2:
			inv.Slots[i].Full, inv.Slots[i].Barcode = false, ""
		case 24:
			inv.Slots[i].Full, inv.Slots[i].Barcode = true, "E01002L8"
		}
	}
	e.fc.invByLib[10] = inv
	e.fc.calls = nil
	e.fc.mu.Unlock()

	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/10/slots/25", map[string]any{"barcode": "NEW025L8"})
	got := file(t, st, "library_contents.10")
	if !strings.Contains(got, "\nSlot 2:\n") || !strings.Contains(got, "\nSlot 24: E01002L8\n") {
		t.Errorf("stage not built from the placement after the move:\n%s", got)
	}
	// The slot holding a cartridge now needs a disposition.
	rec := e.call(http.MethodDelete, "/api/v1/config/libraries/10/slots/24", map[string]any{"stage": st.Stage, "disposition": map[string]any{"to": "shelf"}})
	if rec.Code != http.StatusOK {
		t.Errorf("delete slot 24 to the shelf: %d %s", rec.Code, rec.Body)
	}
	e.fc.mu.Lock()
	reads := len(e.fc.calls)
	e.fc.mu.Unlock()
	if reads != 1 {
		t.Errorf("%d changer reads for two edits of one stage, want 1", reads)
	}
}

// An apply whose helper answer is lost (a transport error, or the apply's
// deadline) leaves its stage needing a new plan, and GET shows the attempt
// (#61 review N2).
func TestConfigApplyLost(t *testing.T) {
	e := newWriteEnv(t, nil)
	st := e.staged()
	e.answer(func(req apply.Request) apply.Response {
		if req.Op == apply.OpPlan {
			return apply.Response{Result: apply.ResultValidated, Units: []string{"vtllibrary@10.service"}}
		}
		return apply.Response{Result: notJSON}
	})
	e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage, "dry_run": true})
	rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), helperUnreachable) {
		t.Fatalf("apply with no answer: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[stageDTO](t, e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil))
	if got.State != stageOpen || got.LastApply == nil || got.LastApply.Error == nil || got.LastApply.Error.Code != helperUnreachable ||
		got.LastApply.Error.Message == "" || got.LastApply.Result != "unknown" {
		t.Errorf("stage after a lost apply: %+v", got)
	}
	if rec := e.call(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": st.Stage}); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "plan_required") {
		t.Errorf("apply again: %d %s", rec.Code, rec.Body)
	}
}

// A library whose vtllibrary@ is not running loads library_contents from
// disk at its next start, so the disk file is its placement: the stage
// starts from it, and says so. A running library must be read; one
// starting or stopping is read again later (plan, amendment "Phase 4
// client contract for next": "cur's file otherwise").
func TestConfigStageDownLibrary(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		e := newWriteEnv(t, nil)
		e.readable(30)
		e.fc.invByLib[30] = changer.Inventory{} // what a stale read would say
		e.h.setLib(30, state)
		st := e.edit(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
		if _, ok := st.Files["library_contents.30"]; ok {
			t.Errorf("%s: library_contents.30 staged, not the disk file", state)
		}
		if want := []string{"library 30 is not running; its placement comes from library_contents.30"}; !reflect.DeepEqual(st.Notes, want) {
			t.Errorf("%s: notes %q", state, st.Notes)
		}
		for _, c := range e.fc.calls {
			if c == "Inventory lib=30" {
				t.Errorf("%s: changer of a stopped library read", state)
			}
		}
	}
	for _, state := range []string{"activating", "deactivating", "reloading"} {
		e := newWriteEnv(t, nil)
		e.readable(30)
		e.h.setLib(30, state)
		rec := e.call(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "live_unreadable") || !strings.Contains(rec.Body.String(), state) {
			t.Errorf("%s: %d %s", state, rec.Code, rec.Body)
		}
	}
	// No unit state, no telling a stopped library from an unreadable one.
	e := newWriteEnv(t, nil)
	e.readable(30)
	e.h.mu.Lock()
	e.h.libStates = nil
	e.h.mu.Unlock()
	if rec := e.call(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"attrs": map[string]any{"Backoff": "500"}}); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "live_unreadable") {
		t.Errorf("units unanswered: %d %s", rec.Code, rec.Body)
	}
	// A refused edit drops its note.
	e = newWriteEnv(t, nil)
	e.h.setLib(30, "failed")
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/10", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
	if rec := e.call(http.MethodDelete, "/api/v1/config/libraries/30/slots/999", map[string]any{"stage": st.Stage}); rec.Code == http.StatusOK {
		t.Fatalf("delete of no slot: %d", rec.Code)
	}
	if got := decodeBody[stageDTO](t, e.call(http.MethodGet, "/api/v1/config/stages/"+st.Stage, nil)); len(got.Notes) != 0 {
		t.Errorf("refused edit left notes %q", got.Notes)
	}
}

// A stopped PERSIST library starts from the file vtllibrary loads at its
// next start: library_contents.N.persist when it exists and
// library_contents.N is not newer (apply.FileSource), so an unrelated edit
// keeps the moves .persist recorded (#61 review M1).
func TestConfigStageDownPersist(t *testing.T) {
	setup := func(persist bool, contentsNewer bool) *writeEnv {
		e := newWriteEnv(t, nil)
		e.h.setLib(30, "inactive")
		if persist {
			p := filepath.Join(e.dir, "device.conf")
			dc := conffile.ParseDeviceConf([]byte(e.disk("device.conf")))
			if err := dc.SetAttr("Library", 30, "PERSIST", "True"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, dc.Render(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		lc := e.disk("library_contents.30")
		moved := strings.Replace(strings.Replace(lc, "Slot 1: G03001TA\n", "Slot 1: G03002TA\n", 1), "Slot 2: G03002TA\n", "Slot 2: G03001TA\n", 1)
		if moved == lc {
			t.Fatal("fixture: no move")
		}
		pp := filepath.Join(e.dir, "library_contents.30.persist")
		if err := os.WriteFile(pp, []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		old, newer := time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)
		lcTime, pTime := old, newer
		if contentsNewer {
			lcTime, pTime = newer, old
		}
		if err := os.Chtimes(filepath.Join(e.dir, "library_contents.30"), lcTime, lcTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(pp, pTime, pTime); err != nil {
			t.Fatal(err)
		}
		return e
	}
	edit := map[string]any{"attrs": map[string]any{"Backoff": "500"}}

	e := setup(true, false)
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/30", edit)
	got := file(t, st, "library_contents.30")
	if !strings.Contains(got, "\nSlot 1: G03002TA\n") || !strings.Contains(got, "\nSlot 2: G03001TA\n") {
		t.Errorf("the moves .persist recorded are undone:\n%s", got)
	}
	if want := []string{"library 30 is not running; its placement comes from library_contents.30.persist"}; !reflect.DeepEqual(st.Notes, want) {
		t.Errorf("notes %q", st.Notes)
	}

	for name, e := range map[string]*writeEnv{"library_contents newer": setup(true, true), "PERSIST off": setup(false, false)} {
		st := e.edit(http.MethodPatch, "/api/v1/config/libraries/30", edit)
		if _, ok := st.Files["library_contents.30"]; ok {
			t.Errorf("%s: library_contents.30 staged, not the file as it is", name)
		}
		if want := []string{"library 30 is not running; its placement comes from library_contents.30"}; !reflect.DeepEqual(st.Notes, want) {
			t.Errorf("%s: notes %q", name, st.Notes)
		}
	}
}

// Deleting a library drops its note (#61 review L2).
func TestConfigStageDeleteDropsNote(t *testing.T) {
	e := newWriteEnv(t, nil)
	e.h.setLib(30, "failed")
	st := e.edit(http.MethodPatch, "/api/v1/config/libraries/30", map[string]any{"attrs": map[string]any{"Backoff": "500"}})
	if len(st.Notes) != 1 {
		t.Fatalf("notes %q", st.Notes)
	}
	st = e.edit(http.MethodDelete, "/api/v1/config/libraries/30", map[string]any{
		"stage": st.Stage, "dispositions": []map[string]any{{"barcode": "G03001TA", "to": "shelf"}}})
	if len(st.Notes) != 0 {
		t.Errorf("notes after the library's delete: %q", st.Notes)
	}
}
