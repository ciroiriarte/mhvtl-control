package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
)

// serviceLockWait bounds how long a service action waits for the libraries
// it acts on; a variable so a test can shorten it.
var serviceLockWait = time.Minute

// serviceRequest is the body of POST /service/{action} and POST
// /service/libraries/{id}/{action}.
type serviceRequest struct {
	// DryRun asks the helper what the action would do (service_plan).
	DryRun bool `json:"dry_run"`
	// Units are the units the dry run showed, which the action must
	// find unchanged.
	Units []string `json:"units"`
	// ForceToken is a drive_loaded dry run's token, for an action that
	// puts the loaded cartridges back in their slots.
	ForceToken string `json:"force_token"`
}

// serviceResultDTO is a service action's answer: applyResultDTO's fields
// for the helper's answer, and what was asked.
type serviceResultDTO struct {
	applyResultDTO
	Action  string `json:"action"`
	Target  bool   `json:"target,omitempty"`
	Library *int   `json:"library,omitempty"`
}

// serviceHTTPStatus is applyStatus for a service action. The refusals that
// say the units are not as the client saw them are 409: unit (expect_units
// differs from the helper's derivation, plan again), use_restart (D6: the
// library runs, so restart it) and drive_loaded (a cartridge is mounted:
// confirm with the force token). The rest keep applyStatus': budget and
// placement are 422, a rollback is 500, a recovery 409.
func serviceHTTPStatus(resp apply.Response) (int, string) {
	if resp.Result == apply.ResultRefused {
		switch resp.Error {
		case apply.ErrUnit, apply.ErrUseRestart, apply.ErrDriveLoaded:
			return http.StatusConflict, resp.Error
		}
	}
	return applyStatus(resp)
}

func (s *Server) handleServiceTarget(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, false)
}

func (s *Server) handleServiceLibrary(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, true)
}

// serviceAction sends a service action on the library of the path's id
// when library is set, else on mhvtl.target, to the helper (mhvtl-control#14, plan 4.3): a
// service_plan with dry_run, else a service expecting the dry run's units.
// It is gated as a configuration write. The action holds lockLibrary for
// every library it may stop or start, in ascending order, until the
// answer: no move or queue command of the daemon runs against a library
// the helper takes offline or restarts. The unlocks drop each library's
// live read, and the action drops the cached GET /service. A dry run takes
// no lock, so it does not wait out an action in progress: the helper
// answers it apply_in_progress, and the action checks again.
func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request, library bool) {
	// The gate answers first: with writes off, a caller learns nothing of
	// the actions or libraries there are.
	if !s.writeAllowed(w, r) {
		return
	}
	var lib *int
	if library {
		id, err := pathInt(r, "id")
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		lib = &id
	}
	action := r.PathValue("action")
	switch action {
	case apply.ActionRestart, apply.ActionStop, apply.ActionStart:
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown service action "+action+"; restart, stop or start")
		return
	}
	snap, ok := s.readConfigOrFail(w)
	if !ok {
		return
	}
	var ids []int
	for _, l := range snap.inv.Libraries {
		if lib == nil || l.ID == *lib {
			ids = append(ids, l.ID)
		}
	}
	if lib != nil && len(ids) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no library "+r.PathValue("id")+" in device.conf")
		return
	}
	var req serviceRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.DryRun && req.ForceToken != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "force_token is for the action, not a dry run")
		return
	}
	if !req.DryRun && len(req.Units) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "units is required: the units the dry run showed")
		return
	}
	hreq := apply.Request{Op: apply.OpServicePlan, Action: action, Library: lib, Target: lib == nil,
		Actor: "mhvtl-control API client " + r.RemoteAddr}
	ctx := r.Context()
	if !req.DryRun {
		hreq.Op, hreq.ExpectUnits, hreq.ForceToken = apply.OpService, req.Units, req.ForceToken
		// The action runs to its end even if the client goes away.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
		defer cancel()
	}
	if !req.DryRun {
		// A device.conf can repeat a library ID; the mutexes are not
		// reentrant.
		sort.Ints(ids)
		ids = slices.Compact(ids)
		// The wait for a library held by a move or queue command is
		// bounded: the action answers busy rather than queue behind a
		// command that hangs. Once held, the locks last until the answer.
		lctx, lcancel := context.WithTimeout(r.Context(), serviceLockWait)
		defer lcancel()
		for _, id := range ids {
			unlock, err := s.lockLibraryCtx(lctx, id)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "busy", fmt.Sprintf("library %d is busy with a move or queue command; try again", id))
				return
			}
			defer unlock()
		}
	}
	resp, err := s.cfg.Helper.Do(ctx, hreq)
	s.forgetHello()
	if !req.DryRun {
		s.service.forget()
	}
	if errors.Is(err, applyclient.ErrAbsent) {
		writeHelperAbsent(w)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, helperUnreachable, err.Error())
		return
	}
	status, code := serviceHTTPStatus(resp)
	out := serviceResultDTO{applyResultDTO: applyResultOf(resp, code), Action: action, Target: lib == nil, Library: lib}
	if !req.DryRun {
		out.ForceToken, out.ForceExpires = "", nil
	}
	writeJSON(w, status, out)
}
