package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
	"mhvtld/internal/drivestats"
)

// DriveStatsService reads a tape drive's statistics; implemented by
// *drivestats.Reader.
type DriveStatsService interface {
	Read(d config.Drive) (drivestats.Stats, error)
}

var _ DriveStatsService = (*drivestats.Reader)(nil)

func (s *Server) handleDriveStats(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("drive"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "drive id must be an integer")
		return
	}
	var drive *config.Drive
	for i := range lib.Drives {
		if lib.Drives[i].ID == id {
			drive = &lib.Drives[i]
		}
	}
	if drive == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("drive %d is not in library %d", id, lib.ID))
		return
	}
	if s.cfg.Drives == nil {
		writeError(w, http.StatusServiceUnavailable, "drive_unavailable", "drive access not configured")
		return
	}
	st, err := s.cfg.Drives.Read(*drive)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, st)
	case errors.Is(err, drivestats.ErrReserved):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, drivestats.ErrUnavailable), errors.Is(err, changer.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "drive_unavailable", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "scsi_error", err.Error())
	}
}
