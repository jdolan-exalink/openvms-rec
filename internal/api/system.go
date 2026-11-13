package api

import (
	"net"
	"net/http"
	"runtime"

	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
	"github.com/jdolan-exalink/openvms-rec/internal/sysinfo"
)

// StreamResource is one recorded stream with its FFmpeg usage.
type StreamResource struct {
	CameraID       string        `json:"camera_id"`
	Representation string        `json:"representation"`
	State          string        `json:"state"`
	LastError      string        `json:"last_error,omitempty"`
	BitrateKbps    float64       `json:"bitrate_kbps"`
	Reconnects     int64         `json:"reconnects"`
	Segments       int64         `json:"segments"`
	FFmpeg         *sysinfo.Proc `json:"ffmpeg,omitempty"`
}

// ResourcesDTO is GET /api/v1/system/resources.
type ResourcesDTO struct {
	Version       string             `json:"version"`
	OS            string             `json:"os"`
	Arch          string             `json:"arch"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	Recorder      sysinfo.Proc       `json:"recorder"`
	Goroutines    int                `json:"goroutines"`
	FFmpegTotal   sysinfo.Proc       `json:"ffmpeg_total"`
	Host          sysinfo.Host       `json:"host"`
	Streams       []StreamResource   `json:"streams"`
	RecentLogs    []sysinfo.LogEntry `json:"recent_logs"`
}

func (s *Server) systemRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/system/resources", s.resources)
	mux.HandleFunc("GET /health/status", s.healthStatus)
}

func (s *Server) sampler() *sysinfo.Sampler {
	s.samplerOnce.Do(func() {
		if s.Sampler == nil {
			s.Sampler = sysinfo.NewSampler()
		}
	})
	return s.Sampler
}

func (s *Server) resources(w http.ResponseWriter, _ *http.Request) {
	sm := s.sampler()
	out := ResourcesDTO{Version: s.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		UptimeSeconds: int64(sm.Uptime().Seconds()), Recorder: sm.Self(), Goroutines: runtime.NumGoroutine(),
		Host: sm.Host(), Streams: []StreamResource{}, RecentLogs: []sysinfo.LogEntry{}}
	keep := map[int]bool{}
	for _, st := range s.streams() {
		sr := StreamResource{CameraID: st.CameraID, Representation: st.Representation, State: st.State,
			LastError: st.LastError, BitrateKbps: st.BitrateKbps, Reconnects: st.Reconnects, Segments: st.Segments}
		if st.PID > 0 {
			if p, ok := sm.Process(st.PID); ok {
				sr.FFmpeg = &p
				keep[st.PID] = true
				out.FFmpegTotal.CPUPercent += p.CPUPercent
				out.FFmpegTotal.RSSBytes += p.RSSBytes
			}
		}
		out.Streams = append(out.Streams, sr)
	}
	sm.Forget(keep)
	if s.Logs != nil {
		out.RecentLogs = s.Logs.Entries()
	}
	writeJSON(w, http.StatusOK, out)
}

// HealthStatus is GET /health/status: a small summary for the tray panel,
// served without an API key but only to loopback clients.
type HealthStatus struct {
	State            string   `json:"state"` // ok | warning
	Reasons          []string `json:"reasons"`
	Version          string   `json:"version"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	StreamsTotal     int      `json:"streams_total"`
	StreamsRecording int      `json:"streams_recording"`
	VolumesTotal     int      `json:"volumes_total"`
	VolumesWritable  int      `json:"volumes_writable"`
	HeadroomBytes    uint64   `json:"headroom_bytes"`
	Alerts           int      `json:"alerts"`
}

func isLoopbackRequest(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) healthStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	st := HealthStatus{State: "ok", Reasons: []string{}, Version: s.Version, UptimeSeconds: int64(s.sampler().Uptime().Seconds())}
	for _, x := range s.streams() {
		st.StreamsTotal++
		if x.State == "recording" {
			st.StreamsRecording++
		}
	}
	sts := s.inspect(r.Context())
	cands := storage.Candidates(sts, s.Cfg.Storage)
	for i, v := range sts {
		if !v.Spec.Enabled && v.State == volumes.StateDisabled {
			continue
		}
		st.VolumesTotal++
		if cands[i].Writable {
			st.VolumesWritable++
			st.HeadroomBytes += v.Limits.Headroom
		}
	}
	if s.Storage != nil {
		st.Alerts = len(s.Storage.Alerts())
	}
	if st.StreamsRecording < st.StreamsTotal {
		st.Reasons = append(st.Reasons, "streams_not_recording")
	}
	if st.VolumesWritable == 0 || st.VolumesWritable < st.VolumesTotal {
		st.Reasons = append(st.Reasons, "volumes_not_writable")
	}
	if st.Alerts > 0 {
		st.Reasons = append(st.Reasons, "storage_alerts")
	}
	if len(st.Reasons) > 0 {
		st.State = "warning"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}
