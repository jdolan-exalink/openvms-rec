package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// RetentionPolicy defines the 3-stage Avigilon-style aging schedule.
type RetentionPolicy struct {
	FullDays       int `json:"full_days"`
	HalfFPSDays    int `json:"half_fps_days"`
	EventsOnlyDays int `json:"events_only_days"`
	TotalDays      int `json:"total_days"`
}

// CameraEstimate reports recording rate and estimated retention for one camera.
type CameraEstimate struct {
	CameraID         string  `json:"camera_id"`
	Name             string  `json:"name"`
	State            string  `json:"state"`
	Recording        bool    `json:"recording"`
	BitrateKbps      float64 `json:"bitrate_kbps"`
	EstimatedBitrate bool    `json:"estimated_bitrate"`
	GBPerDay         float64 `json:"gb_per_day"`
	RecordedBytes    int64   `json:"recorded_bytes"`
	RecordedDays     float64 `json:"recorded_days"`
	EstimatedDays    float64 `json:"estimated_days"`
}

// StorageEstimate summarizes global and per-camera retention capacity.
type StorageEstimate struct {
	Policy              RetentionPolicy  `json:"policy"`
	QuotaBytes          uint64           `json:"quota_bytes"`
	UsedBytes           uint64           `json:"used_bytes"`
	RequiredBytes       float64          `json:"required_bytes"`
	TotalBitrateKbps    float64          `json:"total_bitrate_kbps"`
	TotalGBPerDay       float64          `json:"total_gb_per_day"`
	GlobalEstimatedDays float64          `json:"global_estimated_days"`
	Cameras             []CameraEstimate `json:"cameras"`
}

func (s *Server) storageEstimateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/storage/retention", s.getRetentionPolicy)
	mux.HandleFunc("PUT /api/v1/storage/retention", s.putRetentionPolicy)
	mux.HandleFunc("GET /api/v1/storage/estimate", s.getStorageEstimate)
}

func (s *Server) loadRetentionPolicy(ctx context.Context) RetentionPolicy {
	pol := RetentionPolicy{FullDays: 7, HalfFPSDays: 14, EventsOnlyDays: 9}
	if s.DB != nil {
		var f, h, e int
		err := s.DB.QueryRowContext(ctx, `SELECT full_days, half_fps_days, events_only_days FROM retention_policy WHERE id = 1`).Scan(&f, &h, &e)
		if err == nil {
			pol.FullDays, pol.HalfFPSDays, pol.EventsOnlyDays = f, h, e
		}
	}
	pol.TotalDays = pol.FullDays + pol.HalfFPSDays + pol.EventsOnlyDays
	return pol
}

func (s *Server) getRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.loadRetentionPolicy(r.Context()))
}

func (s *Server) putRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var in RetentionPolicy
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if in.FullDays < 0 || in.HalfFPSDays < 0 || in.EventsOnlyDays < 0 ||
		in.FullDays > 3650 || in.HalfFPSDays > 3650 || in.EventsOnlyDays > 3650 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_retention", "days must be between 0 and 3650")
		return
	}
	if s.DB != nil {
		_, err := s.DB.ExecContext(r.Context(), `
			INSERT INTO retention_policy (id, full_days, half_fps_days, events_only_days, updated_at)
			VALUES (1, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				full_days = excluded.full_days,
				half_fps_days = excluded.half_fps_days,
				events_only_days = excluded.events_only_days,
				updated_at = excluded.updated_at`,
			in.FullDays, in.HalfFPSDays, in.EventsOnlyDays, time.Now().UTC().UnixMilli())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to save retention policy")
			return
		}
	}
	in.TotalDays = in.FullDays + in.HalfFPSDays + in.EventsOnlyDays
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) getStorageEstimate(w http.ResponseWriter, r *http.Request) {
	pol := s.loadRetentionPolicy(r.Context())
	if v, ok := parseQueryInt(r, "full_days"); ok && v >= 0 {
		pol.FullDays = v
	}
	if v, ok := parseQueryInt(r, "half_fps_days"); ok && v >= 0 {
		pol.HalfFPSDays = v
	}
	if v, ok := parseQueryInt(r, "events_only_days"); ok && v >= 0 {
		pol.EventsOnlyDays = v
	}
	pol.TotalDays = pol.FullDays + pol.HalfFPSDays + pol.EventsOnlyDays

	var quotaBytes, usedBytes uint64
	sts := s.inspect(r.Context())
	for _, st := range sts {
		if st.State == volumes.StateHealthy || st.State == volumes.StateFull {
			dto := toDTO(st, false)
			quotaBytes += dto.Limits.EffectiveBytes
			usedBytes += dto.RecorderBytes
		}
	}

	type segStat struct {
		bytes      int64
		minStartMS int64
		maxEndMS   int64
		durMS      int64
	}
	segStats := map[string]segStat{}
	if s.DB != nil {
		rows, err := s.DB.QueryContext(r.Context(), `
			SELECT camera_id, COALESCE(SUM(size_bytes),0), COALESCE(MIN(start_ms),0), COALESCE(MAX(end_ms),0),
			       COALESCE(SUM(CASE WHEN end_ms > start_ms THEN end_ms - start_ms ELSE 0 END), 0)
			FROM segments
			GROUP BY camera_id`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				var st segStat
				if rows.Scan(&id, &st.bytes, &st.minStartMS, &st.maxEndMS, &st.durMS) == nil {
					segStats[id] = st
				}
			}
		}
	}

	live := map[string]StreamStatus{}
	if s.Streams != nil {
		for _, st := range s.Streams() {
			if _, ok := live[st.CameraID]; !ok || st.Representation == "main" {
				live[st.CameraID] = st
			}
		}
	}

	cams := []CameraEstimate{}
	var totalBytesPerDay float64
	var totalKbps float64

	for _, e := range s.registry().List() {
		ss, hasLive := live[e.ID]
		seg := segStats[e.ID]
		state := "stopped"
		if !e.Camera.IsEnabled() {
			state = "disabled"
		} else if hasLive {
			state = ss.State
		}

		kbps := 0.0
		estBitrate := false
		if hasLive && ss.BitrateKbps > 0 {
			kbps = ss.BitrateKbps
		} else if seg.durMS > 0 && seg.bytes > 0 {
			kbps = float64(seg.bytes) * 8.0 / (float64(seg.durMS) / 1000.0) / 1000.0
		} else if e.Camera.IsEnabled() {
			kbps = 2000 // conservative default before first segment closes
			estBitrate = true
		}

		recDays := 0.0
		if seg.maxEndMS > seg.minStartMS {
			recDays = float64(seg.maxEndMS-seg.minStartMS) / 86400000.0
		}

		bytesPerDay := kbps * 1000.0 / 8.0 * 86400.0
		if e.Camera.IsEnabled() {
			totalBytesPerDay += bytesPerDay
			totalKbps += kbps
		}

		cams = append(cams, CameraEstimate{
			CameraID:         e.ID,
			Name:             e.Name,
			State:            state,
			Recording:        state == "recording",
			BitrateKbps:      kbps,
			EstimatedBitrate: estBitrate,
			GBPerDay:         bytesPerDay / 1e9,
			RecordedBytes:    seg.bytes,
			RecordedDays:     recDays,
		})
	}

	globalDays, reqBytes := estimateDays(float64(quotaBytes), totalBytesPerDay, pol)
	for i := range cams {
		if totalBytesPerDay > 0 && cams[i].GBPerDay > 0 {
			share := (cams[i].GBPerDay * 1e9) / totalBytesPerDay
			cams[i].EstimatedDays, _ = estimateDays(float64(quotaBytes)*share, cams[i].GBPerDay*1e9, pol)
		}
	}

	writeJSON(w, http.StatusOK, StorageEstimate{
		Policy:              pol,
		QuotaBytes:          quotaBytes,
		UsedBytes:           usedBytes,
		RequiredBytes:       reqBytes,
		TotalBitrateKbps:    totalKbps,
		TotalGBPerDay:       totalBytesPerDay / 1e9,
		GlobalEstimatedDays: globalDays,
		Cameras:             cams,
	})
}

func estimateDays(quotaBytes, bytesPerDay float64, pol RetentionPolicy) (days float64, requiredBytes float64) {
	if bytesPerDay <= 0 {
		return 0, 0
	}
	const halfFactor = 0.50
	const eventsFactor = 0.05

	s1 := float64(pol.FullDays) * bytesPerDay
	s2 := float64(pol.HalfFPSDays) * halfFactor * bytesPerDay
	s3 := float64(pol.EventsOnlyDays) * eventsFactor * bytesPerDay
	requiredBytes = s1 + s2 + s3

	if quotaBytes <= 0 {
		return 0, requiredBytes
	}
	if pol.FullDays == 0 && pol.HalfFPSDays == 0 && pol.EventsOnlyDays == 0 {
		return quotaBytes / bytesPerDay, 0
	}
	if quotaBytes <= s1 {
		return quotaBytes / bytesPerDay, requiredBytes
	}
	rem := quotaBytes - s1
	if rem <= s2 {
		return float64(pol.FullDays) + rem/(halfFactor*bytesPerDay), requiredBytes
	}
	rem -= s2
	if rem <= s3 {
		return float64(pol.FullDays+pol.HalfFPSDays) + rem/(eventsFactor*bytesPerDay), requiredBytes
	}
	rem -= s3
	return float64(pol.FullDays+pol.HalfFPSDays+pol.EventsOnlyDays) + rem/(eventsFactor*bytesPerDay), requiredBytes
}

func parseQueryInt(r *http.Request, name string) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

var _ = sql.ErrNoRows
