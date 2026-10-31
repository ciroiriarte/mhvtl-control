package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
)

var lib10Units = []string{"vtllibrary@10.service", "vtltape@11.service"}

// A dry run asks the helper for a service_plan of the action on the
// library or the target; the action sends service with the units the
// client echoes, run apart from the request's context.
func TestServiceActionRequests(t *testing.T) {
	e := newWriteEnv(t, nil)
	e.answer(func(req apply.Request) apply.Response {
		if req.Op == apply.OpServicePlan {
			return apply.Response{Result: apply.ResultValidated, Units: lib10Units, Warnings: []string{"library 10: ch"}}
		}
		return apply.Response{Result: apply.ResultApplied, Units: lib10Units}
	})
	rec := e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"dry_run": true})
	out := decodeBody[serviceResultDTO](t, rec)
	if rec.Code != http.StatusOK || out.Result != apply.ResultValidated || !slices.Equal(out.Plan.Units, lib10Units) ||
		!slices.Equal(out.Plan.OfflineLibraries, []int{10}) || out.Action != "restart" || out.Library == nil || *out.Library != 10 || out.Target ||
		len(out.Warnings) != 1 || strings.Contains(rec.Body.String(), `"stage"`) {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"units": lib10Units}); rec.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(http.MethodPost, "/api/v1/service/stop", map[string]any{"units": lib10Units, "force_token": "tok"}); rec.Code != http.StatusOK {
		t.Fatalf("target stop: %d %s", rec.Code, rec.Body)
	}
	reqs := e.requests()
	if len(reqs) != 3 {
		t.Fatalf("helper requests %+v", reqs)
	}
	plan, act, tgt := reqs[0], reqs[1], reqs[2]
	if plan.Op != apply.OpServicePlan || plan.Action != "restart" || plan.Library == nil || *plan.Library != 10 || plan.Target || plan.ExpectUnits != nil {
		t.Errorf("dry run sent %+v", plan)
	}
	if act.Op != apply.OpService || !slices.Equal(act.ExpectUnits, lib10Units) || act.ForceToken != "" || !strings.HasPrefix(act.Actor, "mhvtl-control API client") {
		t.Errorf("restart sent %+v", act)
	}
	if tgt.Op != apply.OpService || tgt.Action != "stop" || !tgt.Target || tgt.Library != nil || tgt.ForceToken != "tok" {
		t.Errorf("target stop sent %+v", tgt)
	}
}

// Unknown actions and libraries are 404; a dry run with a token and an
// action without units are 400, none reaching the helper.
func TestServiceActionBadRequests(t *testing.T) {
	e := newWriteEnv(t, nil)
	for _, c := range []struct {
		path   string
		body   map[string]any
		status int
	}{
		{"/api/v1/service/reload", map[string]any{"dry_run": true}, http.StatusNotFound},
		{"/api/v1/service/libraries/10/reload", map[string]any{"dry_run": true}, http.StatusNotFound},
		{"/api/v1/service/libraries/99/restart", map[string]any{"dry_run": true}, http.StatusNotFound},
		{"/api/v1/service/libraries/x/restart", map[string]any{"dry_run": true}, http.StatusNotFound},
		{"/api/v1/service/libraries/10/restart", map[string]any{"dry_run": true, "force_token": "t"}, http.StatusBadRequest},
		{"/api/v1/service/libraries/10/restart", map[string]any{}, http.StatusBadRequest},
		{"/api/v1/service/restart", map[string]any{"dry_run": true, "units": 3}, http.StatusBadRequest},
	} {
		if rec := e.call(http.MethodPost, c.path, c.body); rec.Code != c.status {
			t.Errorf("%s %v: %d %s, want %d", c.path, c.body, rec.Code, rec.Body, c.status)
		}
	}
	if n := len(e.requests()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// The routes are gated as configuration writes: 401 without the bearer,
// 403 token_required and tls_required, 409 helper_absent without the
// helper's client or socket.
func TestServiceActionGated(t *testing.T) {
	paths := []string{"/api/v1/service/restart", "/api/v1/service/libraries/10/stop"}
	anon := newWriteEnv(t, nil)
	anon.anon = true
	noToken := newWriteEnv(t, func(c *Config) { c.Token = "" })
	insecure := newWriteEnv(t, func(c *Config) { c.InsecureListen = true })
	noClient := newWriteEnv(t, func(c *Config) { c.Helper = nil })
	gone := newWriteEnv(t, nil)
	gone.h.ln.Close()
	os.Remove(gone.socket)
	body := map[string]any{"dry_run": true}
	for _, p := range paths {
		if rec := anon.call(http.MethodPost, p, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without the bearer: %d %s", p, rec.Code, rec.Body)
		}
		if rec := noToken.call(http.MethodPost, p, body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_required") {
			t.Errorf("%s without a token: %d %s", p, rec.Code, rec.Body)
		}
		if rec := insecure.call(http.MethodPost, p, body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tls_required") {
			t.Errorf("%s without TLS: %d %s", p, rec.Code, rec.Body)
		}
		for _, e := range []*writeEnv{noClient, gone} {
			if rec := e.call(http.MethodPost, p, body); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "helper_absent") {
				t.Errorf("%s without the helper: %d %s", p, rec.Code, rec.Body)
			}
		}
	}
	if n := len(anon.h.reqs) + len(noToken.h.reqs) + len(insecure.h.reqs); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// The helper's answers map to statuses: the refusals that say the units
// are not as the client saw them are 409, budget and placement 422, a
// rollback 500 with its unit and journal, a recovery 409.
func TestServiceActionStatuses(t *testing.T) {
	expires := time.Date(2026, 10, 30, 12, 2, 0, 0, time.UTC)
	for _, c := range []struct {
		resp   apply.Response
		dry    bool
		status int
		code   string
	}{
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrUnit, Message: "expect_units differs"}, false, http.StatusConflict, "unit"},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrUseRestart, Message: "library 10 is already active"}, true, http.StatusConflict, "use_restart"},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrDriveLoaded, Units: lib10Units, ForceToken: "tok", ForceExpires: &expires}, true, http.StatusConflict, "drive_loaded"},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrBudget, Message: "restart the libraries one at a time"}, true, http.StatusUnprocessableEntity, "budget"},
		{apply.Response{Result: apply.ResultRefused, Error: apply.ErrPlacement}, true, http.StatusUnprocessableEntity, "placement"},
		{apply.Response{Result: apply.ResultRolledBack, Error: apply.ErrStart, FailedUnit: "vtltape@11.service", JournalTail: "tail"}, false, http.StatusInternalServerError, "rolled_back"},
		{apply.Response{Result: apply.ResultRecovered, Message: "recovered"}, false, http.StatusConflict, "recovered"},
		{apply.Response{Result: apply.ResultInProgress}, true, http.StatusConflict, "apply_in_progress"},
		{apply.Response{Result: apply.ResultError, Error: apply.ErrStart, FailedUnit: "vtltape@13.service"}, false, http.StatusInternalServerError, "start_failed"},
	} {
		e := newWriteEnv(t, nil)
		resp := c.resp
		e.answer(func(apply.Request) apply.Response { return resp })
		body := map[string]any{"dry_run": c.dry}
		if !c.dry {
			body["units"] = lib10Units
		}
		rec := e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", body)
		out := decodeBody[serviceResultDTO](t, rec)
		if rec.Code != c.status || out.Error == nil || out.Error.Code != c.code || out.Result != c.resp.Result {
			t.Errorf("%s/%s: %d %s", c.resp.Result, c.resp.Error, rec.Code, rec.Body)
		}
		if c.code == "drive_loaded" && (out.ForceToken != "tok" || out.ForceExpires == nil) {
			t.Errorf("drive_loaded dry run lost its token: %s", rec.Body)
		}
		if c.resp.FailedUnit != "" && out.FailedUnit != c.resp.FailedUnit {
			t.Errorf("%s: failed unit %q", c.code, out.FailedUnit)
		}
	}
}

// blockedHelper holds the service answer until release is closed.
func blockedHelper(e *writeEnv) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	e.answer(func(req apply.Request) apply.Response {
		if req.Op == apply.OpService {
			close(started)
			<-release
		}
		return apply.Response{Result: apply.ResultApplied, Units: lib10Units}
	})
	return started, release
}

// lockedChangers serializes Transfer, which the fake changers do not, for
// moves of two libraries at once.
type lockedChangers struct {
	ChangerService
	mu sync.Mutex
}

func (c *lockedChangers) Transfer(lib config.Library, from, to changer.Location) (changer.Move, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ChangerService.Transfer(lib, from, to)
}

// A move of library 10 waits while a restart of library 10 is in the
// helper, and goes once it answered; one of library 30 does not wait. A
// target action holds every library.
func TestServiceActionHoldsLibraryLocks(t *testing.T) {
	for _, path := range []string{"/api/v1/service/libraries/10/restart", "/api/v1/service/restart"} {
		e := newWriteEnv(t, func(c *Config) { c.Changers = &lockedChangers{ChangerService: c.Changers} })
		started, release := blockedHelper(e)
		done := make(chan int)
		go func() {
			done <- e.call(http.MethodPost, path, map[string]any{"units": lib10Units}).Code
		}()
		<-started
		move := func(id string) chan int {
			ch := make(chan int)
			go func() {
				ch <- e.call(http.MethodPost, "/api/v1/libraries/"+id+"/move", map[string]any{"from": map[string]any{"kind": "slot", "number": 2}, "to": map[string]any{"kind": "slot", "number": 3}}).Code
			}()
			return ch
		}
		moved10 := move("10")
		other := move("30")
		if path != "/api/v1/service/restart" {
			select {
			case <-other:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: a move of library 30 waited for library 10", path)
			}
		}
		select {
		case code := <-moved10:
			t.Fatalf("%s: the move ran during the restart: %d", path, code)
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		if code := <-done; code != http.StatusOK {
			t.Errorf("%s: %d", path, code)
		}
		select {
		case <-moved10:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the move did not run after the restart", path)
		}
		if path == "/api/v1/service/restart" {
			<-other
		}
	}
}

// An action drops the cached GET /service, so the next read asks the
// helper again at once; a dry run does not.
func TestServiceActionForgetsStatus(t *testing.T) {
	e := newWriteEnv(t, nil)
	e.answer(func(req apply.Request) apply.Response {
		return apply.Response{Result: apply.ResultApplied, Units: lib10Units}
	})
	get := func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/service", nil)
		r.Header.Set("Authorization", "Bearer "+e.srv.cfg.Token)
		e.handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /service: %d %s", rec.Code, rec.Body)
		}
	}
	get()
	n := e.h.calls()
	get()
	if e.h.calls() != n {
		t.Fatalf("the second read was not cached")
	}
	e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"dry_run": true})
	n = e.h.calls()
	get()
	if e.h.calls() != n {
		t.Errorf("a dry run dropped the cached status")
	}
	e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"units": lib10Units})
	n = e.h.calls()
	get()
	if e.h.calls() == n {
		t.Errorf("the status read after the restart was the cached one")
	}
}

// A read in flight when an action ends is not cached: it may predate it.
func TestServiceForgetDetachesFlight(t *testing.T) {
	var c serviceCache
	f := &serviceFlight{done: make(chan struct{}), gen: c.gen}
	c.flight = f
	c.forget()
	if c.flight != nil || c.gen == f.gen {
		t.Errorf("forget left flight %v, gen %d", c.flight, c.gen)
	}
}

// #92 review 1: a device.conf that repeats a library ID does not lock the
// library twice: the action answers, and the library takes moves again.
func TestServiceActionDuplicateLibraryID(t *testing.T) {
	e := newWriteEnv(t, nil)
	dc, err := os.ReadFile(filepath.Join(e.dir, "device.conf"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(dc), "Library: 10 ")
	j := i + strings.Index(string(dc[i:]), "# fifo")
	if err := os.WriteFile(filepath.Join(e.dir, "device.conf"), append(dc, "\n"+string(dc[i:j])...), 0o644); err != nil {
		t.Fatal(err)
	}
	e.answer(func(apply.Request) apply.Response {
		return apply.Response{Result: apply.ResultApplied, Units: lib10Units}
	})
	for _, path := range []string{"/api/v1/service/libraries/10/restart", "/api/v1/service/restart"} {
		done := make(chan int, 1)
		go func() { done <- e.call(http.MethodPost, path, map[string]any{"units": lib10Units}).Code }()
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Errorf("%s: %d", path, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s hung on the repeated library", path)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := e.srv.lockLibraryCtx(ctx, 10)
	if err != nil {
		t.Fatalf("library 10 still locked: %v", err)
	}
	unlock()
}

// #92 review 2: with writes off the gate answers first, whatever the
// action or library: a caller learns nothing of the libraries there are.
func TestServiceActionGateFirst(t *testing.T) {
	e := newWriteEnv(t, func(c *Config) { c.Token = "" })
	for _, p := range []string{"/api/v1/service/reload", "/api/v1/service/libraries/99/restart", "/api/v1/service/libraries/x/stop", "/api/v1/service/libraries/10/reload"} {
		if rec := e.call(http.MethodPost, p, map[string]any{"dry_run": true}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "token_required") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
}

// #92 review 3: an action waits a bounded time for a library a move or
// queue command holds, then answers 503 busy without asking the helper.
func TestServiceActionLockWaitBounded(t *testing.T) {
	saved := serviceLockWait
	serviceLockWait = 50 * time.Millisecond
	t.Cleanup(func() { serviceLockWait = saved })
	e := newWriteEnv(t, nil)
	e.answer(func(apply.Request) apply.Response {
		return apply.Response{Result: apply.ResultApplied, Units: lib10Units}
	})
	unlock := e.srv.lockLibrary(30)
	rec := e.call(http.MethodPost, "/api/v1/service/restart", map[string]any{"units": lib10Units})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"busy"`) || !strings.Contains(rec.Body.String(), "library 30") {
		t.Errorf("held library: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.requests()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
	// Library 10, taken before 30, was released again.
	if rec := e.call(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"units": lib10Units}); rec.Code != http.StatusOK {
		t.Errorf("library 10 after the busy answer: %d %s", rec.Code, rec.Body)
	}
	unlock()
	if rec := e.call(http.MethodPost, "/api/v1/service/restart", map[string]any{"units": lib10Units}); rec.Code != http.StatusOK {
		t.Errorf("after the move: %d %s", rec.Code, rec.Body)
	}
}
