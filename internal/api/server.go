// Package api serves the Recorder REST API (/api/v1) and health endpoints.
// The contract is api/openapi.yaml; operations marked x-status: planned there
// are not routed yet and return 404.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/exports"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/playback"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/health"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
	"github.com/jdolan-exalink/openvms-rec/web"
)

// Server holds dependencies of the HTTP handlers.
type Server struct {
	Cfg     config.Config
	DB      *sql.DB
	Prober  volumes.Prober
	Version string
	Log     *slog.Logger
	Now     func() time.Time
	// Storage, when set, provides live volume state (including ingest
	// failures). Without it volumes are inspected on each request.
	Storage *storage.Manager
	// Streams returns ingest status per camera stream.
	Streams func() []StreamStatus
	// Exports and Signer enable exports and HLS playback.
	Exports *exports.Service
	Signer  *playback.Signer
	// Cameras is the camera registry. Nil: cameras of Cfg, read-only.
	Cameras *cameras.Registry
	// FFmpeg runs connection tests; Probe overrides ingest.Probe in tests.
	FFmpeg string
	Probe  func(ctx context.Context, ffmpeg string, src ingest.Source) ingest.ProbeResult

	staticOnce sync.Once
	staticReg  *cameras.Registry
	probeOnce  sync.Once
	probeSem   chan struct{}
}

// Handler returns the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /{$}", s.ui)
	mux.HandleFunc("GET /api/v1/system/capabilities", s.capabilities)
	mux.HandleFunc("GET /api/v1/storage", s.storageSummary)
	mux.HandleFunc("GET /api/v1/storage/volumes", s.listVolumes)
	mux.HandleFunc("POST /api/v1/storage/volumes/{id}/validate", s.validateVolume)
	mux.HandleFunc("GET /api/v1/cameras", s.listCameras)
	mux.HandleFunc("GET /api/v1/cameras/{id}", s.getCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}/recordings", s.recordings)
	s.cameraAdminRoutes(mux)
	s.mediaRoutes(mux)
	return mux
}

func (s *Server) ui(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; media-src 'self' blob:; img-src 'self' data:; frame-ancestors 'none'")
	w.Write(web.Index)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// capabilities reports what this build implements. Camera capabilities are
// per camera and only ever come from probes against real hardware.
func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.Version,
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
		"features": map[string]string{
			"storage.multi_volume":    "implemented",
			"storage.retention":       "implemented",
			"recording.rtsp":          "implemented",
			"cameras.onvif_discovery": "planned",
			"camera_sd.profile_g":     "planned",
			"playback.hls":            "implemented",
			"exports":                 "implemented",
		},
	})
}

func (s *Server) inspect(ctx context.Context) []volumes.Status {
	if s.Storage != nil {
		if sts := s.Storage.Statuses(); len(sts) > 0 {
			return sts
		}
		return s.Storage.Refresh(ctx)
	}
	store := segments.New(s.DB)
	in := volumes.Inspector{
		Prober: s.Prober,
		Now:    s.now,
		RecorderBytes: func(id string) uint64 {
			n, err := store.RecorderBytes(ctx, id)
			if err != nil {
				s.Log.Warn("recorder bytes", "volume", id, "err", err)
			}
			return n
		},
	}
	return in.InspectAll(storage.SpecsFromConfig(s.Cfg, runtime.GOOS))
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	sts := s.inspect(r.Context())
	cands := storage.Candidates(sts, s.Cfg.Storage)
	out := make([]VolumeDTO, 0, len(sts))
	for i, st := range sts {
		out = append(out, toDTO(st, cands[i].Writable))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) storageSummary(w http.ResponseWriter, r *http.Request) {
	sts := s.inspect(r.Context())
	cands := storage.Candidates(sts, s.Cfg.Storage)
	sum := SummaryDTO{Placement: s.Cfg.Storage.Placement, PressurePolicy: s.Cfg.Storage.PressurePolicy, ByState: map[string]int{}}
	for i, st := range sts {
		sum.Volumes++
		sum.ByState[string(st.State)]++
		if cands[i].Writable {
			sum.WritableVolumes++
			sum.EffectiveLimitBytes += st.Limits.Effective
			sum.HeadroomBytes += st.Limits.Headroom
		}
		sum.RecorderBytes += st.Usage.RecorderBytes
	}
	sum.Warnings = []string{volumes.WarnNoRedundancy}
	sum.Alerts = []AlertDTO{}
	if s.Storage != nil {
		for _, a := range s.Storage.Alerts() {
			sum.Alerts = append(sum.Alerts, AlertDTO{Code: a.Code, CameraID: a.CameraID, Message: a.Message})
		}
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) validateVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var spec *volumes.Spec
	for _, sp := range storage.SpecsFromConfig(s.Cfg, runtime.GOOS) {
		if sp.ID == id {
			spec = &sp
			break
		}
	}
	if spec == nil {
		writeError(w, http.StatusNotFound, "volume_not_found", "no configured volume with that id")
		return
	}
	if _, err := volumes.Initialize(s.Prober, *spec, s.now()); err != nil {
		code := "volume_validation_failed"
		if errors.Is(err, volumes.ErrNotEmpty) {
			code = "volume_not_empty"
		}
		writeError(w, http.StatusConflict, code, err.Error())
		return
	}
	probe := health.Probe(spec.Root)
	var sts []volumes.Status
	if s.Storage != nil {
		sts = s.Storage.Refresh(r.Context())
	} else {
		sts = s.inspect(r.Context())
	}
	cands := storage.Candidates(sts, s.Cfg.Storage)
	for i, st := range sts {
		if st.Spec.ID != id {
			continue
		}
		dto := toDTO(st, cands[i].Writable)
		dto.Probe = &ProbeDTO{OK: probe.OK, LatencyMS: float64(probe.Latency.Microseconds()) / 1000}
		if probe.Err != nil {
			dto.Probe.Error = probe.Err.Error()
		}
		writeJSON(w, http.StatusOK, dto)
		return
	}
}

// writeJSON writes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the stable error envelope defined in openapi.yaml.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
