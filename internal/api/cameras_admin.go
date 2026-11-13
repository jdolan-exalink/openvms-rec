package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
)

func (s *Server) cameraAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cameras", s.createCamera)
	mux.HandleFunc("PUT /api/v1/cameras/{id}", s.updateCamera)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}", s.deleteCamera)
	mux.HandleFunc("POST /api/v1/cameras/test", s.testCamera)
}

// registry returns the camera registry; without one (tests, tools) only the
// cameras of the configuration file are listed and mutations fail.
func (s *Server) registry() *cameras.Registry {
	if s.Cameras != nil {
		return s.Cameras
	}
	s.staticOnce.Do(func() {
		s.staticReg, _ = cameras.Open(context.Background(), cameras.Options{Static: s.Cfg.Cameras})
	})
	return s.staticReg
}

func decodeCamera(w http.ResponseWriter, r *http.Request) (cameras.Input, bool) {
	var in cameras.Input
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return in, false
	}
	return in, true
}

func (s *Server) cameraError(w http.ResponseWriter, err error) {
	var inv *cameras.InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
			"code": "invalid_camera", "message": inv.Error(), "details": inv.Issues}})
	case errors.Is(err, cameras.ErrNotFound):
		writeError(w, http.StatusNotFound, "camera_not_found", err.Error())
	case errors.Is(err, cameras.ErrExists):
		writeError(w, http.StatusConflict, "camera_exists", err.Error())
	case errors.Is(err, cameras.ErrReadOnly):
		writeError(w, http.StatusConflict, "camera_read_only", err.Error())
	case errors.Is(err, cameras.ErrHasRecordings):
		writeError(w, http.StatusConflict, "camera_has_recordings", err.Error())
	default:
		s.Log.Error("camera change", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not save the camera")
	}
}

func (s *Server) audit(ctx context.Context, action, target string, details map[string]any) {
	b, _ := json.Marshal(details)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(at, actor, action, target, details) VALUES (?, 'api', ?, ?, ?)`,
		s.now().UnixMilli(), action, target, string(b)); err != nil {
		s.Log.Warn("audit", "err", err)
	}
}

func (s *Server) createCamera(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeCamera(w, r)
	if !ok {
		return
	}
	e, err := s.registry().Create(r.Context(), in)
	if err != nil {
		s.cameraError(w, err)
		return
	}
	s.audit(r.Context(), "camera.create", e.ID, map[string]any{"main_url": ingest.Redact(e.MainURL), "credentials": e.CredentialsRef != ""})
	writeJSON(w, http.StatusCreated, s.cameraDTO(e, s.streams()))
}

func (s *Server) updateCamera(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeCamera(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if in.ID != "" && in.ID != id {
		writeError(w, http.StatusBadRequest, "invalid_request", "the id cannot be changed")
		return
	}
	e, err := s.registry().Update(r.Context(), id, in)
	if err != nil {
		s.cameraError(w, err)
		return
	}
	s.audit(r.Context(), "camera.update", id, map[string]any{"main_url": ingest.Redact(e.MainURL),
		"credentials_changed": in.Username != nil || in.Password != nil})
	writeJSON(w, http.StatusOK, s.cameraDTO(e, s.streams()))
}

func (s *Server) deleteCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.registry().Delete(r.Context(), id); err != nil {
		s.cameraError(w, err)
		return
	}
	s.audit(r.Context(), "camera.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// TestRequest checks a stream before saving. With camera_id and no
// username, the stored credentials of that camera are used.
type TestRequest struct {
	CameraID  string  `json:"camera_id"`
	URL       string  `json:"url"`
	Transport string  `json:"rtsp_transport"`
	Username  *string `json:"username"`
	Password  *string `json:"password"`
}

func (s *Server) testCamera(w http.ResponseWriter, r *http.Request) {
	var req TestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_url", "url must be rtsp://host[:port]/path")
		return
	}
	if u.User != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_url", "put the username and password in their own fields, not in the url")
		return
	}
	user, pass := "", ""
	if req.Username != nil {
		user = *req.Username
		if req.Password != nil {
			pass = *req.Password
		}
	}
	if req.CameraID != "" {
		if c, ok := s.registry().Credentials(req.CameraID); ok {
			if req.Username == nil {
				user, pass = c.Username, c.Password
			} else if req.Password == nil && user == c.Username {
				pass = c.Password
			}
		}
	}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	if s.FFmpeg == "" {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "connection test needs ffmpeg")
		return
	}
	select {
	case s.probeSlots() <- struct{}{}:
		defer func() { <-s.probeSlots() }()
	default:
		writeError(w, http.StatusTooManyRequests, "busy", "another connection test is running; retry in a few seconds")
		return
	}
	probe := s.Probe
	if probe == nil {
		probe = ingest.Probe
	}
	res := probe(r.Context(), s.FFmpeg, ingest.Source{URL: u.String(), Transport: req.Transport})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) probeSlots() chan struct{} {
	s.probeOnce.Do(func() { s.probeSem = make(chan struct{}, 2) })
	return s.probeSem
}
