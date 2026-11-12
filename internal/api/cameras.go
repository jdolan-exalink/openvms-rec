package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// CameraDTO is the Camera schema (config-managed in this build).
type CameraDTO struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Enabled        bool           `json:"enabled"`
	Transport      string         `json:"rtsp_transport"`
	Priority       int            `json:"priority"`
	DualRecording  bool           `json:"dual_recording"`
	HasCredentials bool           `json:"has_credentials"`
	Streams        []StreamStatus `json:"streams"`
}

func (s *Server) cameraDTO(c config.Camera, streams []StreamStatus) CameraDTO {
	t := c.Transport
	if t == "" {
		t = "tcp"
	}
	d := CameraDTO{ID: c.ID, Name: c.Name, Enabled: c.IsEnabled(), Transport: t, Priority: c.Priority,
		DualRecording: c.DualRecording || s.Cfg.Recording.DualRecording, HasCredentials: c.CredentialsRef != "",
		Streams: []StreamStatus{}}
	for _, st := range streams {
		if st.CameraID == c.ID {
			d.Streams = append(d.Streams, st)
		}
	}
	return d
}

func (s *Server) streams() []StreamStatus {
	if s.Streams == nil {
		return nil
	}
	return s.Streams()
}

func (s *Server) listCameras(w http.ResponseWriter, _ *http.Request) {
	streams := s.streams()
	out := make([]CameraDTO, 0, len(s.Cfg.Cameras))
	for _, c := range s.Cfg.Cameras {
		out = append(out, s.cameraDTO(c, streams))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) findCamera(id string) (config.Camera, bool) {
	for _, c := range s.Cfg.Cameras {
		if c.ID == id {
			return c, true
		}
	}
	return config.Camera{}, false
}

func (s *Server) getCamera(w http.ResponseWriter, r *http.Request) {
	c, ok := s.findCamera(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	writeJSON(w, http.StatusOK, s.cameraDTO(c, s.streams()))
}

// IntervalDTO / GapDTO / TimelineDTO follow the Timeline schema.
type IntervalDTO struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Source    string    `json:"source"`
	Origin    string    `json:"origin"`
	Available bool      `json:"available"`
	VolumeIDs []string  `json:"volume_ids"`
	Segments  int       `json:"segments"`
}

type GapDTO struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitzero"`
	Reason string    `json:"reason"`
}

type TimelineDTO struct {
	Intervals []IntervalDTO `json:"intervals"`
	Gaps      []GapDTO      `json:"gaps"`
}

// maxJoin is the largest hole between consecutive segments still drawn as
// one continuous interval.
const maxJoin = 2 * time.Second

func (s *Server) recordings(w http.ResponseWriter, r *http.Request) {
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
	store := segments.New(s.DB)
	segs, err := store.Range(r.Context(), id, rep, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	gaps, err := store.Gaps(r.Context(), id, rep, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	avail := map[string]bool{}
	for _, st := range s.inspect(r.Context()) {
		avail[st.Spec.ID] = st.State != volumes.StateMissing && st.State != volumes.StateUnready
	}
	tl := TimelineDTO{Intervals: []IntervalDTO{}, Gaps: []GapDTO{}}
	for _, g := range segs {
		end := g.End
		if end.IsZero() {
			end = s.now().UTC() // still writing
		}
		a := avail[g.VolumeID]
		if n := len(tl.Intervals); n > 0 {
			last := &tl.Intervals[n-1]
			if last.Available == a && last.Origin == g.Source && g.Start.Sub(last.End) <= maxJoin {
				last.End = end
				last.Segments++
				if !slices.Contains(last.VolumeIDs, g.VolumeID) {
					last.VolumeIDs = append(last.VolumeIDs, g.VolumeID)
				}
				continue
			}
		}
		tl.Intervals = append(tl.Intervals, IntervalDTO{Start: g.Start, End: end, Source: "recorder_local",
			Origin: g.Source, Available: a, VolumeIDs: []string{g.VolumeID}, Segments: 1})
	}
	for _, g := range gaps {
		tl.Gaps = append(tl.Gaps, GapDTO{Start: g.Start, End: g.End, Reason: g.Reason})
	}
	writeJSON(w, http.StatusOK, tl)
}

// parseRange reads from/to as RFC 3339; the range is capped at 31 days.
func parseRange(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339Nano, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339Nano, q.Get("to"))
	switch {
	case err1 != nil || err2 != nil:
		writeError(w, http.StatusBadRequest, "invalid_range", "from and to must be RFC 3339 timestamps")
	case !to.After(from):
		writeError(w, http.StatusBadRequest, "invalid_range", "to must be after from")
	case to.Sub(from) > 31*24*time.Hour:
		writeError(w, http.StatusBadRequest, "invalid_range", "range limited to 31 days")
	default:
		return from.UTC(), to.UTC(), true
	}
	return time.Time{}, time.Time{}, false
}
