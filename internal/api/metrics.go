package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// SecurityHeaders adds conservative headers to every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

var volumeStates = []volumes.State{volumes.StateHealthy, volumes.StateFull, volumes.StateDegraded, volumes.StateMissing,
	volumes.StateDisabled, volumes.StateDraining, volumes.StateUnready}

// metrics writes the Prometheus text format without extra dependencies.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	help := func(name, typ, text string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ) }
	esc := func(v string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) }

	streams := s.streams()
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].CameraID+streams[i].Representation < streams[j].CameraID+streams[j].Representation
	})
	type sm struct {
		name, typ, help string
		val             func(StreamStatus) float64
	}
	for _, m := range []sm{
		{"recorder_stream_recording", "gauge", "1 when the stream is being recorded", func(x StreamStatus) float64 { return b2f(x.State == "recording") }},
		{"recorder_stream_segments_total", "counter", "Segments written since start", func(x StreamStatus) float64 { return float64(x.Segments) }},
		{"recorder_stream_bytes_total", "counter", "Bytes written since start", func(x StreamStatus) float64 { return float64(x.Bytes) }},
		{"recorder_stream_reconnects_total", "counter", "Stream reconnections since start", func(x StreamStatus) float64 { return float64(x.Reconnects) }},
		{"recorder_stream_bitrate_kbps", "gauge", "Bitrate of the last segment", func(x StreamStatus) float64 { return x.BitrateKbps }},
		{"recorder_stream_last_write_seconds", "gauge", "Unix time of the last finished segment", func(x StreamStatus) float64 {
			if x.LastWrite.IsZero() {
				return 0
			}
			return float64(x.LastWrite.UnixMilli()) / 1000
		}},
	} {
		help(m.name, m.typ, m.help)
		for _, st := range streams {
			fmt.Fprintf(&b, "%s{camera=\"%s\",stream=\"%s\"} %g\n", m.name, esc(st.CameraID), esc(st.Representation), m.val(st))
		}
	}

	sts := s.inspect(r.Context())
	type vm struct {
		name, help string
		val        func(volumes.Status) float64
	}
	for _, m := range []vm{
		{"recorder_volume_capacity_bytes", "Filesystem capacity", func(v volumes.Status) float64 { return float64(v.Usage.CapacityBytes) }},
		{"recorder_volume_free_bytes", "Free bytes available to the service", func(v volumes.Status) float64 { return float64(v.Usage.FreeBytes) }},
		{"recorder_volume_recorder_bytes", "Bytes of recordings indexed on the volume", func(v volumes.Status) float64 { return float64(v.Usage.RecorderBytes) }},
		{"recorder_volume_effective_limit_bytes", "Effective write limit", func(v volumes.Status) float64 { return float64(v.Limits.Effective) }},
		{"recorder_volume_cleanup_start_bytes", "Retention high mark", func(v volumes.Status) float64 { return float64(v.Limits.CleanupStart) }},
	} {
		help(m.name, "gauge", m.help)
		for _, v := range sts {
			fmt.Fprintf(&b, "%s{volume=\"%s\"} %g\n", m.name, esc(v.Spec.ID), m.val(v))
		}
	}
	help("recorder_volume_state", "gauge", "1 for the current state of each volume")
	for _, v := range sts {
		for _, st := range volumeStates {
			fmt.Fprintf(&b, "recorder_volume_state{volume=\"%s\",state=\"%s\"} %g\n", esc(v.Spec.ID), st, b2f(v.State == st))
		}
	}
	if s.Storage != nil {
		help("recorder_storage_alerts", "gauge", "Active retention/storage alerts by code")
		counts := map[string]int{}
		for _, a := range s.Storage.Alerts() {
			counts[a.Code]++
		}
		for _, code := range []string{"min_retention_violated", "retention_pressure", "protected_data_exceeds_target"} {
			fmt.Fprintf(&b, "recorder_storage_alerts{code=\"%s\"} %d\n", code, counts[code])
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, b.String())
}

func b2f(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
