package api

import (
	"context"
	"net/http"
	"sort"

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
// root, so the file is the current one with only those values changed by
// conffile's round-trip setter, and must be what the helper derives from
// it (apply.DeriveMhvtlConf), which it compares byte for byte.
func (s *Server) handleConfigSystemPatch(w http.ResponseWriter, r *http.Request) {
	var req systemPatchRequest
	if !s.writeAllowed(w, r) || !decodeStrict(w, r, &req) {
		return
	}
	s.stageEdit(w, r, req.Stage, func(ctx context.Context, st *stage) error {
		if len(req.Values) == 0 {
			return badEdit("no values to change")
		}
		keys := make([]string, 0, len(req.Values))
		for k := range req.Values {
			switch {
			case k == "MHVTL_HOME_PATH":
				return badEdit("MHVTL_HOME_PATH moves the media home of every library without its own: use mhvtl-move-home")
			case !systemKeys[k]:
				return badEdit("%s cannot be set here: only CAPACITY, VERBOSE and VTL_DEBUG", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if err := s.adopt(st, mhvtlConf); err != nil {
			return err
		}
		data, _ := st.file(mhvtlConf)
		c := conffile.ParseMhvtlConf(data)
		if err := c.Writable(); err != nil {
			return err
		}
		for _, k := range keys {
			if err := c.Set(k, req.Values[k]); err != nil {
				return &editError{http.StatusBadRequest, apply.ErrMhvtlConfValue, err.Error()}
			}
		}
		next := c.Render()
		if code, msg, ok := apply.Refused(apply.DeriveMhvtlConf(st.disk[mhvtlConf], next)); ok {
			return &editError{http.StatusBadRequest, code, msg}
		}
		st.next[mhvtlConf] = next
		return nil
	})
}
