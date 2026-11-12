package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/exports"
	"github.com/jdolan-exalink/openvms-rec/internal/playback"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// playbackTTL bounds how long a playback link works.
const playbackTTL = 15 * time.Minute

func (s *Server) mediaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cameras/{id}/playback", s.createPlayback)
	mux.HandleFunc("GET /api/v1/playback/{token}/index.m3u8", s.playlist)
	mux.HandleFunc("GET /api/v1/playback/{token}/segments/{seg}", s.segment)
	mux.HandleFunc("POST /api/v1/exports", s.createExport)
	mux.HandleFunc("GET /api/v1/exports/{id}", s.getExport)
	mux.HandleFunc("POST /api/v1/exports/{id}/cancel", s.cancelExport)
	mux.HandleFunc("GET /api/v1/exports/{id}/download", s.downloadExport)
	mux.HandleFunc("GET /api/v1/jobs", s.listJobs)
}

// volumeRoot returns the root of a reachable volume.
func (s *Server) volumeRoot(r *http.Request, id string) (string, bool) {
	for _, st := range s.inspect(r.Context()) {
		if st.Spec.ID == id {
			ok := st.State != volumes.StateMissing && st.State != volumes.StateUnready
			return st.Spec.Root, ok
		}
	}
	return "", false
}

func (s *Server) createPlayback(w http.ResponseWriter, r *http.Request) {
	if s.Signer == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "playback not configured")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	from, to, ok := parseRange(w, r)
	if !ok {
		return
	}
	rep := r.URL.Query().Get("representation")
	if rep == "" {
		rep = "main"
	}
	exp := s.now().Add(playbackTTL).UTC()
	tok := s.Signer.Sign(playback.Grant{CameraID: id, Representation: rep, From: from, To: to, Expires: exp})
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":        "/api/v1/playback/" + tok + "/index.m3u8",
		"expires_at": exp,
	})
}

func (s *Server) grant(w http.ResponseWriter, r *http.Request) (playback.Grant, bool) {
	if s.Signer == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "playback not configured")
		return playback.Grant{}, false
	}
	g, err := s.Signer.Verify(r.PathValue("token"), s.now())
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid_token", "playback token invalid or expired")
		return g, false
	}
	return g, true
}

func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	g, ok := s.grant(w, r)
	if !ok {
		return
	}
	segs, err := segments.New(s.DB).Range(r.Context(), g.CameraID, g.Representation, g.From, g.To)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	var playable []segments.Segment
	for _, sg := range segs {
		if _, ok := s.volumeRoot(r, sg.VolumeID); ok && sg.Status == segments.StatusFinal {
			playable = append(playable, sg)
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, playback.Playlist(playable, func(sg segments.Segment) string {
		return "segments/" + strconv.FormatInt(sg.ID, 10) + ".ts"
	}))
}

func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	g, ok := s.grant(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("seg")
	if filepath.Ext(idStr) != ".ts" {
		writeError(w, http.StatusNotFound, "not_found", "segment not found")
		return
	}
	id, err := strconv.ParseInt(idStr[:len(idStr)-3], 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "segment not found")
		return
	}
	sg, err := segments.New(s.DB).Get(r.Context(), id)
	// The token only grants segments of its camera stream within its range.
	if err != nil || sg.CameraID != g.CameraID || sg.RepresentationID != g.Representation ||
		!sg.Start.Before(g.To) || (!sg.End.IsZero() && !sg.End.After(g.From)) || sg.Status != segments.StatusFinal {
		writeError(w, http.StatusNotFound, "not_found", "segment not found")
		return
	}
	root, ok := s.volumeRoot(r, sg.VolumeID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "volume_unavailable", "the volume holding this segment is not available")
		return
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(sg.RelativePath)))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "segment file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", sg.End, f)
}

func (s *Server) createExport(w http.ResponseWriter, r *http.Request) {
	if s.Exports == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "exports not configured")
		return
	}
	var req exports.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "body must be JSON with camera_id, from, to")
		return
	}
	if _, ok := s.findCamera(req.CameraID); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	e, err := s.Exports.Create(r.Context(), req, r.Header.Get("Idempotency-Key"))
	if errors.Is(err, exports.ErrBadRequest) {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create export")
		return
	}
	writeJSON(w, http.StatusAccepted, e)
}

func (s *Server) getExport(w http.ResponseWriter, r *http.Request) {
	if s.Exports == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "exports not configured")
		return
	}
	e, err := s.Exports.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, exports.ErrNotFound) {
		writeError(w, http.StatusNotFound, "export_not_found", "no export with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) cancelExport(w http.ResponseWriter, r *http.Request) {
	if s.Exports == nil || !s.Exports.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusConflict, "not_cancellable", "export is not running")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *Server) downloadExport(w http.ResponseWriter, r *http.Request) {
	if s.Exports == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "exports not configured")
		return
	}
	p, e, err := s.Exports.Archive(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, exports.ErrNotFound):
		writeError(w, http.StatusNotFound, "export_not_found", "no export with that id")
		return
	case errors.Is(err, exports.ErrNotReady):
		writeError(w, http.StatusConflict, "export_not_ready", "export state is "+e.State)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeError(w, http.StatusGone, "export_missing", "export archive no longer exists")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+e.ID+`.zip"`)
	w.Header().Set("X-Checksum-SHA256", e.SHA256)
	http.ServeContent(w, r, e.ID+".zip", e.UpdatedAt, f)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	if s.Exports == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	jobs, err := s.Exports.List(r.Context(), r.URL.Query().Get("kind"), r.URL.Query().Get("state"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": jobs})
}
