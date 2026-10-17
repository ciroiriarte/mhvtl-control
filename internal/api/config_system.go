package api

import (
	"context"
	"net/http"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/conffile"
)

// systemKeys are the mhvtl.conf keys PATCH /config/system sets (plan 4.3).
var systemKeys = map[string]bool{"CAPACITY": true, "VERBOSE": true, "VTL_DEBUG": true}

type systemPatchRequest struct {
	Stage string `json:"stage"`
	// Values maps CAPACITY, VERBOSE or VTL_DEBUG to its new value.
	Values map[string]string `json:"values"`
}

// handleConfigSystemPatch stages mhvtl.conf with new values of CAPACITY,
// VERBOSE and VTL_DEBUG. make_vtl_media sources mhvtl.conf as bash, as
// root, so the helper accepts only the file it derives itself
// (apply.DeriveMhvtlConf), byte for byte. The stage is rebuilt from the
// file on disk with every value staged so far and the request's
// (apply.BuildMhvtlConf), not edited request by request: keys the file
// lacks are appended, and in the helper's order only then.
func (s *Server) handleConfigSystemPatch(w http.ResponseWriter, r *http.Request) {
	var req systemPatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		if len(req.Values) == 0 {
			return badEdit("no values to change")
		}
		for k := range req.Values {
			switch {
			case k == "MHVTL_HOME_PATH":
				return badEdit("MHVTL_HOME_PATH moves the media home of every library without its own: use mhvtl-move-home")
			case !systemKeys[k]:
				return badEdit("%s cannot be set here: only CAPACITY, VERBOSE and VTL_DEBUG", k)
			}
		}
		if err := s.adopt(st, mhvtlConf); err != nil {
			return err
		}
		if err := conffile.ParseMhvtlConf(st.disk[mhvtlConf]).Writable(); err != nil {
			return err
		}
		values := map[string]string{}
		staged, _ := st.file(mhvtlConf)
		sc := conffile.ParseMhvtlConf(staged)
		for k := range systemKeys {
			if v, ok := sc.Get(k); ok {
				values[k] = v
			}
		}
		for k, v := range req.Values {
			values[k] = v
		}
		next, err := apply.BuildMhvtlConf(st.disk[mhvtlConf], values)
		if err == nil {
			err = apply.DeriveMhvtlConf(st.disk[mhvtlConf], next)
		}
		if code, msg, ok := apply.Refused(err); ok {
			return &editError{http.StatusBadRequest, code, msg}
		} else if err != nil {
			return err
		}
		st.next[mhvtlConf] = next
		return nil
	})
}
