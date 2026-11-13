package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

func (s *Server) volumeAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/storage/volumes", s.createVolume)
	mux.HandleFunc("PATCH /api/v1/storage/volumes/{id}", s.updateVolume)
	mux.HandleFunc("PUT /api/v1/storage/volumes/{id}", s.updateVolume)
	mux.HandleFunc("DELETE /api/v1/storage/volumes/{id}", s.deleteVolume)
}

func decodeVolume(w http.ResponseWriter, r *http.Request) (storage.VolumeInput, bool) {
	var in storage.VolumeInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return in, false
	}
	return in, true
}

func (s *Server) volumeError(w http.ResponseWriter, err error) {
	var inv *storage.InvalidVolumeError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
			"code": "invalid_volume", "message": inv.Error(), "details": inv.Issues}})
	case errors.Is(err, storage.ErrVolumeNotFound):
		writeError(w, http.StatusNotFound, "volume_not_found", err.Error())
	case errors.Is(err, storage.ErrVolumeExists):
		writeError(w, http.StatusConflict, "volume_exists", err.Error())
	case errors.Is(err, storage.ErrVolumeReadOnly):
		writeError(w, http.StatusConflict, "volume_read_only", err.Error())
	case errors.Is(err, storage.ErrVolumeHasData):
		writeError(w, http.StatusConflict, "volume_has_recordings", err.Error())
	case errors.Is(err, volumes.ErrNotEmpty):
		writeError(w, http.StatusConflict, "volume_not_empty", err.Error())
	case errors.Is(err, storage.ErrVolumesDisabled):
		writeError(w, http.StatusServiceUnavailable, "volumes_read_only", err.Error())
	default:
		// Initialization errors (permissions, marker of another volume) are
		// actionable for the operator, so their text is returned.
		s.Log.Warn("volume change", "err", err)
		writeError(w, http.StatusConflict, "volume_validation_failed", err.Error())
	}
}

// volumeResponse returns the freshly inspected volume.
func (s *Server) volumeResponse(w http.ResponseWriter, r *http.Request, id string, status int) {
	sts := s.inspect(r.Context())
	cands := storage.Candidates(sts, s.Cfg.Storage)
	for i, st := range sts {
		if st.Spec.ID == id {
			writeJSON(w, status, s.volumeDTO(st, cands[i].Writable))
			return
		}
	}
	writeError(w, http.StatusNotFound, "volume_not_found", "volume not found after saving")
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	if s.Storage == nil {
		s.volumeError(w, storage.ErrVolumesDisabled)
		return
	}
	in, ok := decodeVolume(w, r)
	if !ok {
		return
	}
	v, err := s.Storage.CreateVolume(r.Context(), in)
	if err != nil {
		s.volumeError(w, err)
		return
	}
	s.audit(r.Context(), "volume.create", v.ID, map[string]any{"path": v.Path, "initialize": in.Initialize})
	s.volumeResponse(w, r, v.ID, http.StatusCreated)
}

func (s *Server) updateVolume(w http.ResponseWriter, r *http.Request) {
	if s.Storage == nil {
		s.volumeError(w, storage.ErrVolumesDisabled)
		return
	}
	in, ok := decodeVolume(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if in.ID != "" && in.ID != id {
		writeError(w, http.StatusBadRequest, "invalid_request", "the id cannot be changed")
		return
	}
	if _, err := s.Storage.UpdateVolume(r.Context(), id, in); err != nil {
		s.volumeError(w, err)
		return
	}
	s.audit(r.Context(), "volume.update", id, map[string]any{"initialize": in.Initialize})
	s.volumeResponse(w, r, id, http.StatusOK)
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if s.Storage == nil {
		s.volumeError(w, storage.ErrVolumesDisabled)
		return
	}
	id := r.PathValue("id")
	if err := s.Storage.DeleteVolume(r.Context(), id); err != nil {
		s.volumeError(w, err)
		return
	}
	s.audit(r.Context(), "volume.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
}
