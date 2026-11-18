package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/events"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

func (s *Server) onvifAndEventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cameras/discover", s.discoverCameras)
	mux.HandleFunc("POST /api/v1/cameras/probe", s.probeCamera)
	mux.HandleFunc("POST /api/v1/cameras/{id}/probe-tvt", s.probeCameraTVT)
	mux.HandleFunc("GET /api/v1/cameras/{id}/sd-sync", s.sdSyncCameraStatus)
	mux.HandleFunc("POST /api/v1/cameras/{id}/sd-sync", s.sdSyncCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}/capabilities", s.cameraCapabilities)
	mux.HandleFunc("GET /api/v1/cameras/{id}/events", s.listCameraEvents)
	mux.HandleFunc("POST /api/v1/cameras/{id}/events", s.createCameraEvent)
	mux.HandleFunc("GET /api/v1/events", s.listAllEvents)
	mux.HandleFunc("POST /api/v1/events", s.createEvent)
	mux.HandleFunc("GET /api/v1/events/{id}/snapshot", s.eventSnapshot)

	if s.Provision != nil {
		mux.Handle("POST /{$}", s.Provision)
		mux.Handle("/api/v1/events/provision", s.Provision)
		mux.Handle("/v1/provision-lpr/event", s.Provision)
		mux.Handle("/v1/provision-lpr/keepalive", s.Provision)
		mux.Handle("/SendKeepalive", s.Provision)
		mux.Handle("/SendAlarmData", s.Provision)
		mux.Handle("/SendSmartData", s.Provision)
		mux.Handle("/SendDeviceInfo", s.Provision)
		mux.Handle("/Keepalive", s.Provision)
	}
}

func (s *Server) discoverCameras(w http.ResponseWriter, r *http.Request) {
	timeout := 2500 * time.Millisecond
	if raw := r.URL.Query().Get("timeout_ms"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 500 && ms <= 10000 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	disc := s.Discover
	if disc == nil {
		disc = onvif.Discover
	}
	devs, err := disc(r.Context(), timeout)
	if err != nil && len(devs) == 0 {
		writeError(w, http.StatusBadGateway, "discovery_failed", err.Error())
		return
	}
	if devs == nil {
		devs = []onvif.DiscoveredDevice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": devs})
}

func (s *Server) probeCamera(w http.ResponseWriter, r *http.Request) {
	var req onvif.ProbeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_host", "host is required")
		return
	}
	if err := checkCameraHost(r.Context(), s.Cfg.Server.CameraNetworks, req.Host); err != nil {
		writeError(w, http.StatusForbidden, "forbidden_network", err.Error())
		return
	}
	if req.CameraID != "" && req.Username == "" {
		if c, ok := s.registry().Credentials(req.CameraID); ok {
			req.Username = c.Username
			req.Password = c.Password
		}
	}
	select {
	case s.probeSlots() <- struct{}{}:
		defer func() { <-s.probeSlots() }()
	default:
		writeError(w, http.StatusTooManyRequests, "busy", "another probe is running; retry in a few seconds")
		return
	}
	probeFn := s.ONVIFProbe
	if probeFn == nil {
		probeFn = func(ctx context.Context, preq onvif.ProbeRequest) (onvif.ProbeResult, error) {
			return onvif.Probe(ctx, nil, preq, s.now().UTC())
		}
	}
	res, err := probeFn(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "onvif_probe_failed", err.Error())
		return
	}
	var tvtRes *events.TVTProbeResult
	tvtFn := s.TVTProbe
	if tvtFn == nil {
		tvtFn = func(ctx context.Context, host string, port int, user, pass string) (events.TVTProbeResult, error) {
			return events.ProbeTVT(ctx, nil, host, port, user, pass, s.now().UTC())
		}
	}
	mfgLow := strings.ToLower(res.Manufacturer)
	camBrand := ""
	if req.CameraID != "" {
		if c, ok := s.findCamera(req.CameraID); ok {
			camBrand = strings.ToLower(c.Brand)
		}
	}
	if s.TVTProbe != nil || strings.Contains(mfgLow, "provision") || strings.Contains(mfgLow, "tvt") ||
		camBrand == "provisionisr" || camBrand == "provision" || camBrand == "tvt" || camBrand == "vtv" {
		if tr, tErr := tvtFn(r.Context(), req.Host, req.ONVIFPort, req.Username, req.Password); tErr == nil {
			tvtRes = &tr
			res.Capabilities = append(res.Capabilities, tr.Capabilities...)
		}
	}
	if req.CameraID != "" && s.Events != nil {
		_ = s.Events.SaveCapabilities(r.Context(), req.CameraID, res.Capabilities)
	}
	writeJSON(w, http.StatusOK, struct {
		onvif.ProbeResult
		TVT *events.TVTProbeResult `json:"tvt,omitempty"`
	}{ProbeResult: res, TVT: tvtRes})
}

func (s *Server) probeCameraTVT(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cam, ok := s.findCamera(id)
	if !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	host := cam.EffectiveHost()
	if host == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_host", "camera host is not configured")
		return
	}
	if err := checkCameraHost(r.Context(), s.Cfg.Server.CameraNetworks, host); err != nil {
		writeError(w, http.StatusForbidden, "forbidden_network", err.Error())
		return
	}
	var user, pass string
	if c, ok := s.registry().Credentials(id); ok {
		user, pass = c.Username, c.Password
	}
	tvtFn := s.TVTProbe
	if tvtFn == nil {
		tvtFn = func(ctx context.Context, h string, p int, u, pw string) (events.TVTProbeResult, error) {
			return events.ProbeTVT(ctx, nil, h, p, u, pw, s.now().UTC())
		}
	}
	res, err := tvtFn(r.Context(), host, cam.EffectiveONVIFPort(), user, pass)
	if err != nil {
		writeError(w, http.StatusBadGateway, "tvt_probe_failed", err.Error())
		return
	}
	if s.Events != nil && len(res.Capabilities) > 0 {
		_ = s.Events.SaveCapabilities(r.Context(), id, res.Capabilities)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) sdSyncCameraStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	if s.ProfileGSync == nil {
		writeJSON(w, http.StatusOK, events.ProfileGSyncResult{CameraID: id, Status: "unavailable"})
		return
	}
	if res, ok := s.ProfileGSync.LastResult(r.Context(), id); ok {
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusOK, events.ProfileGSyncResult{CameraID: id, Status: "never_run"})
}

func (s *Server) sdSyncCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	if s.ProfileGSync == nil {
		writeError(w, http.StatusServiceUnavailable, "sd_sync_unavailable", "Profile G SD sync is not configured")
		return
	}
	lookback := 24 * time.Hour
	if rawH := r.URL.Query().Get("hours"); rawH != "" {
		if h, err := strconv.Atoi(rawH); err == nil && h >= 1 && h <= 720 {
			lookback = time.Duration(h) * time.Hour
		}
	}
	res, err := s.ProfileGSync.RunCameraSync(r.Context(), id, lookback)
	if err != nil {
		writeError(w, http.StatusBadGateway, "sd_sync_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) cameraCapabilities(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	if s.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []onvif.Capability{}})
		return
	}
	caps, err := s.Events.ListCapabilities(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": caps})
}

func (s *Server) listCameraEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	s.serveEventsQuery(w, r, id)
}

func (s *Server) listAllEvents(w http.ResponseWriter, r *http.Request) {
	s.serveEventsQuery(w, r, r.URL.Query().Get("camera_id"))
}

func (s *Server) serveEventsQuery(w http.ResponseWriter, r *http.Request, cameraID string) {
	if s.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []events.Event{}, "next_cursor": ""})
		return
	}
	q := events.Query{
		CameraID: cameraID,
		Type:     strings.TrimSpace(r.URL.Query().Get("type")),
		Class:    strings.TrimSpace(r.URL.Query().Get("class")),
		Source:   strings.TrimSpace(r.URL.Query().Get("source")),
		Cursor:   r.URL.Query().Get("cursor"),
		Limit:    100,
	}
	if rawMin := strings.TrimSpace(r.URL.Query().Get("min_confidence")); rawMin != "" {
		v, err := strconv.ParseFloat(rawMin, 64)
		if err != nil || v < 0 || v > 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "min_confidence must be in [0, 1]")
			return
		}
		q.MinConfidence = &v
	}
	if rawLim := r.URL.Query().Get("limit"); rawLim != "" {
		if n, err := strconv.Atoi(rawLim); err == nil && n >= 1 && n <= 500 {
			q.Limit = n
		}
	}
	if rawFrom := r.URL.Query().Get("from"); rawFrom != "" {
		t, err := time.Parse(time.RFC3339Nano, rawFrom)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_range", "from must be RFC 3339")
			return
		}
		q.From = t.UTC()
	}
	if rawTo := r.URL.Query().Get("to"); rawTo != "" {
		t, err := time.Parse(time.RFC3339Nano, rawTo)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_range", "to must be RFC 3339")
			return
		}
		q.To = t.UTC()
	}
	items, next, err := s.Events.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) createCameraEvent(w http.ResponseWriter, r *http.Request) {
	s.ingestRESTEvent(w, r, r.PathValue("id"))
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	s.ingestRESTEvent(w, r, "")
}

func (s *Server) ingestRESTEvent(w http.ResponseWriter, r *http.Request, pathCameraID string) {
	store := s.Events
	if store == nil && s.DB != nil {
		store = events.NewStore(s.DB)
	}
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "events_unavailable", "event store is not configured")
		return
	}
	var req struct {
		CameraID     string             `json:"camera_id"`
		TimestampUTC string             `json:"timestamp_utc"`
		Type         string             `json:"type"`
		Class        string             `json:"class"`
		Confidence   *float64           `json:"confidence"`
		BBox         []float64          `json:"bbox"`
		Detections   []events.Detection `json:"detections"`
		Zone         string             `json:"zone"`
		Snapshot     string             `json:"snapshot"`
		Source       string             `json:"source"`
		RawRef       string             `json:"raw_ref"`
		DedupeKey    string             `json:"dedupe_key"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	camID := strings.TrimSpace(pathCameraID)
	if camID == "" {
		camID = strings.TrimSpace(req.CameraID)
	}
	if camID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "camera_id is required")
		return
	}
	if _, ok := s.findCamera(camID); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	evType := strings.TrimSpace(req.Type)
	if evType == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "type is required")
		return
	}
	if req.Confidence != nil && (*req.Confidence < 0 || *req.Confidence > 1) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "confidence must be between 0 and 1")
		return
	}
	if len(req.BBox) != 0 {
		if len(req.BBox) != 4 {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "bbox must have 4 coordinates")
			return
		}
		for _, v := range req.BBox {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				writeError(w, http.StatusUnprocessableEntity, "invalid_request", "bbox coordinates must be finite numbers")
				return
			}
		}
		if req.BBox[2] <= 0 || req.BBox[3] <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "bbox width and height must be > 0")
			return
		}
	}
	ts := s.now().UTC()
	if rawTS := strings.TrimSpace(req.TimestampUTC); rawTS != "" {
		parsed, err := time.Parse(time.RFC3339Nano, rawTS)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_timestamp", "timestamp_utc must be RFC 3339")
			return
		}
		ts = parsed.UTC()
	}
	src := strings.TrimSpace(req.Source)
	if src == "" {
		src = "http_rest"
	}
	ev := events.Event{
		EventID:      events.NewEventID(),
		CameraID:     camID,
		TimestampUTC: ts,
		Type:         evType,
		Class:        strings.TrimSpace(req.Class),
		Confidence:   req.Confidence,
		BBox:         req.BBox,
		Detections:   req.Detections,
		Zone:         strings.TrimSpace(req.Zone),
		Snapshot:     strings.TrimSpace(req.Snapshot),
		Source:       src,
		RawRef:       strings.TrimSpace(req.RawRef),
	}
	events.NormalizeEvent(&ev, 0)
	created, err := store.Insert(r.Context(), ev, strings.TrimSpace(req.DedupeKey))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to store event")
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"created": created,
		"event":   ev,
	})
}

func (s *Server) eventSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.SnapshotsDir == "" {
		writeError(w, http.StatusNotFound, "snapshot_not_found", "event snapshots are not configured")
		return
	}
	store := s.Events
	if store == nil && s.DB != nil {
		store = events.NewStore(s.DB)
	}
	if store == nil {
		writeError(w, http.StatusNotFound, "snapshot_not_found", "event store unavailable")
		return
	}
	ref, err := store.GetSnapshotRef(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	if ref == "" {
		writeError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot for this event")
		return
	}
	base := filepath.Clean(s.SnapshotsDir)
	full := filepath.Clean(filepath.Join(base, filepath.FromSlash(ref)))
	if !strings.HasPrefix(full, base+string(filepath.Separator)) {
		writeError(w, http.StatusForbidden, "invalid_path", "invalid snapshot reference")
		return
	}
	img, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "snapshot_not_found", "snapshot file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not read snapshot")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img)
}
