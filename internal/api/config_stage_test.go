package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
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
