package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func TestRetentionPolicyAndEstimate(t *testing.T) {
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
	if _, err := reg.Create(ctx, cameras.Input{
		ID:      "cam-1",
		Name:    "Cam 1",
		MainURL: "rtsp://192.0.2.10:554/stream1",
	}); err != nil {
		t.Fatalf("reg.Create: %v", err)
	}

	srv := &Server{
		Cfg:     config.Default(),
		DB:      database,
		Cameras: reg,
		Streams: func() []StreamStatus {
			return []StreamStatus{{
				CameraID:       "cam-1",
				Representation: "main",
				State:          "recording",
				BitrateKbps:    4000,
			}}
		},
	}
	h := srv.Handler()

	// Save 3-stage policy
	body, _ := json.Marshal(RetentionPolicy{FullDays: 7, HalfFPSDays: 14, EventsOnlyDays: 9})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/storage/retention", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put retention status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Read back
	req = httptest.NewRequest(http.MethodGet, "/api/v1/storage/retention", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get retention status=%d", rec.Code)
	}
	var pol RetentionPolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &pol)
	if pol.TotalDays != 30 {
		t.Fatalf("expected TotalDays=30, got %+v", pol)
	}

	// Estimate endpoint
	req = httptest.NewRequest(http.MethodGet, "/api/v1/storage/estimate", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("estimate status=%d body=%s", rec.Code, rec.Body.String())
	}
	var est StorageEstimate
	if err := json.Unmarshal(rec.Body.Bytes(), &est); err != nil {
		t.Fatalf("unmarshal estimate: %v", err)
	}
	if len(est.Cameras) != 1 || est.Cameras[0].BitrateKbps != 4000 || est.TotalGBPerDay <= 0 {
		t.Fatalf("unexpected estimate: %+v", est)
	}
}
