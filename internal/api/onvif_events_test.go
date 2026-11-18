package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/events"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

func TestONVIFAndEventRoutes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(tmp, "rec.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	reg, err := cameras.Open(ctx, cameras.Options{
		DB:        database,
		CredsFile: filepath.Join(tmp, "creds.json"),
	})
	if err != nil {
		t.Fatalf("cameras.Open: %v", err)
	}
	_, err = reg.Create(ctx, cameras.Input{
		ID:      "cam-front",
		Name:    "Front",
		MainURL: "rtsp://127.0.0.1:554/profile1",
		Host:    "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("reg.Create: %v", err)
	}

	evStore := events.NewStore(database)
	prov := events.NewProvisionHandler(evStore, reg, slog.Default())
	srv := &Server{
		Cfg:       config.Default(),
		DB:        database,
		Cameras:   reg,
		Events:    evStore,
		Provision: prov,
		Discover: func(ctx context.Context, timeout time.Duration) ([]onvif.DiscoveredDevice, error) {
			return []onvif.DiscoveredDevice{{
				EndpointReference: "urn:uuid:cam-1",
				XAddrs:            []string{"http://192.0.2.225/onvif/device_service"},
				Host:              "192.0.2.225",
				ONVIFPort:         80,
				Name:              "I4-320IPSN-VF-V4",
			}}, nil
		},
		ONVIFProbe: func(ctx context.Context, req onvif.ProbeRequest) (onvif.ProbeResult, error) {
			return onvif.ProbeResult{
				Manufacturer: "Provision-ISR",
				Model:        "I4-320IPSN-VF-V4",
				MainURL:      "rtsp://" + req.Host + ":554/profile1",
				SubURL:       "rtsp://" + req.Host + ":554/profile2",
				Capabilities: []onvif.Capability{{
					Capability: "analytics.line_crossing",
					Status:     onvif.CapUnknown,
					Evidence:   "ONVIF topic advertised",
				}},
			}, nil
		},
	}
	h := srv.Handler()

	// 1. Discover
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/discover", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status=%d body=%s", rec.Code, rec.Body.String())
	}
	var discResp struct {
		Items []onvif.DiscoveredDevice `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &discResp); err != nil || len(discResp.Items) != 1 {
		t.Fatalf("discover response: %v %+v", err, discResp)
	}

	// 2. Probe + persist capabilities
	probeBody, _ := json.Marshal(onvif.ProbeRequest{Host: "192.0.2.225", CameraID: "cam-front"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/probe", bytes.NewReader(probeBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 3. Capabilities endpoint
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-front/capabilities", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "analytics.line_crossing") {
		t.Fatalf("capabilities status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 4. Provision-ISR HTTP push event
	xmlPayload := `<?xml version="1.0" encoding="UTF-8"?>
<SmartEvent>
  <DeviceIP>127.0.0.1</DeviceIP>
  <EventType>LPR</EventType>
  <PlateNumber>ABC123</PlateNumber>
</SmartEvent>`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/events/provision", strings.NewReader(xmlPayload))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/xml")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("provision status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5. Query events by camera and globally
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-front/events?type=lpr", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ABC123") {
		t.Fatalf("camera events status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/events?camera_id=cam-front", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ABC123") {
		t.Fatalf("all events status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. SSRF allowlist blocks hosts outside server.camera_networks
	srv.Cfg.Server.CameraNetworks = []string{"192.0.2.0/24"}
	blockedBody, _ := json.Marshal(onvif.ProbeRequest{Host: "192.168.99.50"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/probe", bytes.NewReader(blockedBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for outside camera_networks, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 7. Register events via HTTP REST (POST /api/v1/events and POST /api/v1/cameras/{id}/events)
	restBody := `{"camera_id":"cam-front","type":"person","class":"person","confidence":0.95,"bbox":[0.2,0.1,0.3,0.6],"zone":"porton","dedupe_key":"rest-1"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(restBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"created":true`) || !strings.Contains(rec.Body.String(), `"evt_`) {
		t.Fatalf("POST /api/v1/events status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Invalid bbox (width <= 0) returns 422
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-front/events", strings.NewReader(`{"type":"vehicle","bbox":[0.1,0.1,0,0.4]}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid bbox status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Duplicate dedupe_key returns 200 with created:false
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-front/events", strings.NewReader(`{"type":"person","dedupe_key":"rest-1"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("duplicate POST /api/v1/cameras/cam-front/events status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Unknown camera returns 404
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/nope/events", strings.NewReader(`{"type":"motion"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown camera status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Verify REST event is returned in GET /api/v1/events
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events?camera_id=cam-front&type=person", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"zone":"porton"`) || !strings.Contains(rec.Body.String(), `"source":"http_rest"`) {
		t.Fatalf("query REST event status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 8. Event snapshot serving (GET /api/v1/events/{id}/snapshot)
	snapDir := t.TempDir()
	srv.SnapshotsDir = snapDir
	if err := os.MkdirAll(filepath.Join(snapDir, "cam-front"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "cam-front", "evt_snap_1.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err = srv.Events.Insert(context.Background(), events.Event{
		EventID:  "evt_snap_1",
		CameraID: "cam-front",
		Type:     "person",
		Source:   "http_rest",
		Snapshot: "cam-front/evt_snap_1.jpg",
	}, "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events/evt_snap_1/snapshot", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.Len() != 4 {
		t.Fatalf("GET event snapshot status=%d ct=%q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events/evt_missing/snapshot", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing snapshot, got %d", rec.Code)
	}

	// 9. Multi-object detections REST ingest & class / source / min_confidence filtering
	motoBody := `{"camera_id":"cam-front","type":"vehicle","class":"motorbike","confidence":0.91,"bbox":[0.3,0.3,0.2,0.4],"detections":[{"type":"person","class":"pedestrian","confidence":0.82,"bbox":[0.1,0.2,0.1,0.4]},{"type":"vehicle","class":"motorcycle","confidence":0.91,"bbox":[0.3,0.3,0.2,0.4]}],"dedupe_key":"moto-1"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/events", strings.NewReader(motoBody))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"motorcycle"`) {
		t.Fatalf("REST multi-detections status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events?class=motorcycle&min_confidence=0.85&source=http_rest", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"motorcycle"`) {
		t.Fatalf("filtered events status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/events?min_confidence=1.5", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid min_confidence, got %d", rec.Code)
	}

	// 10. TVT endpoint probe (/api/v1/cameras/{id}/probe-tvt) & SD sync missing service
	srv.Cfg.Server.CameraNetworks = nil
	srv.TVTProbe = func(_ context.Context, _ string, _ int, _, _ string) (events.TVTProbeResult, error) {
		return events.TVTProbeResult{
			Supported: true,
			Warnings:  []string{"Perímetro DDA habilitado en la cámara, pero sin polígonos de zona dibujados (count=0)"},
		}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-front/probe-tvt", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "count=0") {
		t.Fatalf("probe-tvt status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-front/sd-sync", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when ProfileGSync nil, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-front/sd-sync", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unavailable"`) {
		t.Fatalf("expected 200 unavailable status when ProfileGSync nil, got %d body=%s", rec.Code, rec.Body.String())
	}
	srv.ProfileGSync = &events.ProfileGSyncService{}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/cam-front/sd-sync", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"never_run"`) {
		t.Fatalf("expected 200 never_run status, got %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cameras/nope/sd-sync", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 unknown cam sd-sync status, got %d", rec.Code)
	}
}
