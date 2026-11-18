package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jdolan-exalink/openvms-rec/internal/core"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
)

func (s *Server) coreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/core/status", s.coreStatus)
	mux.HandleFunc("PUT /api/v1/core/config", s.coreUpdateConfig)
	mux.HandleFunc("POST /api/v1/core/sync", s.coreSyncNow)
	mux.HandleFunc("GET /api/v1/core/manifest", s.coreManifest)
}

func (s *Server) coreStatus(w http.ResponseWriter, r *http.Request) {
	if s.Core == nil {
		writeJSON(w, http.StatusOK, core.Status{Status: "disabled"})
		return
	}
	writeJSON(w, http.StatusOK, s.Core.Status(r.Context()))
}

func (s *Server) coreUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if s.Core == nil {
		writeError(w, http.StatusServiceUnavailable, "core_unavailable", "OpenVMS Core adapter is not initialized")
		return
	}
	var patch core.ConfigPatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	st, err := s.Core.UpdateConfig(r.Context(), patch)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_core_config", err.Error())
		return
	}
	s.audit(r.Context(), "core.config", st.NodeID, map[string]any{
		"enabled":  st.Enabled,
		"core_url": st.CoreURL,
		"site_id":  st.SiteID,
	})
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) coreSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.Core == nil {
		writeError(w, http.StatusServiceUnavailable, "core_unavailable", "OpenVMS Core adapter is not initialized")
		return
	}
	syncErr := s.Core.SyncOnce(r.Context())
	st := s.Core.Status(r.Context())
	if syncErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status": st,
			"error":  syncErr.Error(),
		})
		return
	}
	s.audit(r.Context(), "core.sync", st.NodeID, map[string]any{"events_synced": st.EventsSynced})
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) coreManifest(w http.ResponseWriter, r *http.Request) {
	if s.Core != nil {
		writeJSON(w, http.StatusOK, s.Core.Manifest(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, s.CoreManifestSnapshot(r.Context()))
}

// CoreManifestSnapshot builds the normalized OpenVMS Core federation manifest.
func (s *Server) CoreManifestSnapshot(ctx context.Context) core.Manifest {
	return s.buildCoreManifest(ctx)
}

func (s *Server) buildCoreManifest(ctx context.Context) core.Manifest {
	var base core.Manifest
	if s.Core != nil {
		// Prevent recursion if BuildManifest delegates back here.
		st := s.Core.Status(ctx)
		base.NodeID = st.NodeID
		base.SiteID = st.SiteID
	}

	streams := s.streams()
	list := s.registry().List()
	cams := make([]any, 0, len(list))
	for _, e := range list {
		dto := s.cameraDTO(e, streams)
		var caps []any
		if s.Events != nil {
			if rows, err := s.Events.ListCapabilities(ctx, e.ID); err == nil {
				for _, c := range rows {
					caps = append(caps, c)
				}
			}
		}
		cams = append(cams, map[string]any{
			"camera":          dto,
			"capabilities":    caps,
			"webrtc_path":     "/api/v1/cameras/" + e.ID + "/webrtc",
			"snapshot_path":   "/api/v1/cameras/" + e.ID + "/snapshot",
			"recordings_path": "/api/v1/cameras/" + e.ID + "/recordings",
		})
	}

	sts := s.inspect(ctx)
	sc := s.liveStorageCfg()
	cands := storage.Candidates(sts, sc)
	vols := make([]VolumeDTO, 0, len(sts))
	sum := SummaryDTO{Placement: sc.Placement, PressurePolicy: sc.PressurePolicy, ByState: map[string]int{}}
	for i, st := range sts {
		vols = append(vols, s.volumeDTO(st, cands[i].Writable))
		sum.Volumes++
		sum.ByState[string(st.State)]++
		if cands[i].Writable {
			sum.WritableVolumes++
		}
		sum.EffectiveLimitBytes += st.Limits.Effective
		sum.HeadroomBytes += st.Limits.Headroom
		sum.RecorderBytes += st.Usage.RecorderBytes
	}

	streamAny := make([]any, 0, len(streams))
	for _, st := range streams {
		streamAny = append(streamAny, st)
	}

	return core.Manifest{
		SchemaVersion: "openvms-rec.v1",
		NodeID:        base.NodeID,
		SiteID:        base.SiteID,
		Version:       s.Version,
		Capabilities: map[string]any{
			"sd_auto_sync":             s.ProfileGSync != nil,
			"camera_recording_control": true,
			"storage_policies":         true,
			"volume_drain":             true,
			"exports":                  true,
			"webrtc":                   s.WebRTC != nil,
		},
		Cameras: cams,
		Storage: map[string]any{
			"summary": sum,
			"volumes": vols,
		},
		Streams: streamAny,
	}
}
