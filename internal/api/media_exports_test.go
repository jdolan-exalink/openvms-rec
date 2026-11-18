package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/exports"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

func TestExportListVideoAndDeleteAPI(t *testing.T) {
	base := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	expDir := filepath.Join(base, "exports")
	id := "exp_test01"
	if err := os.MkdirAll(filepath.Join(expDir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(expDir, id, "export.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	vw, _ := zw.CreateHeader(&zip.FileHeader{Name: "video.mp4", Method: zip.Store, Modified: time.Now()})
	wantBytes := []byte("ftypisom-fake-mp4-stream-payload")
	_, _ = vw.Write(wantBytes)
	_ = zw.Close()
	_ = f.Close()

	now := time.Now().UTC().Truncate(time.Second)
	expObj := exports.Export{
		ID:        id,
		State:     exports.StateSucceeded,
		Progress:  1,
		Request:   exports.Request{CameraID: "cam1", Representation: "main", From: now.Add(-time.Minute), To: now, Name: "Prueba Porton"},
		SHA256:    "abc123",
		SizeBytes: int64(len(wantBytes)),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload, _ := json.Marshal(expObj)
	if _, err := d.Exec(`INSERT INTO jobs(id, kind, state, progress, error, payload, created_at, updated_at) VALUES (?, 'export', 'succeeded', 1, '', ?, ?, ?)`,
		id, string(payload), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	svc := &exports.Service{DB: d, Store: segments.New(d), Dir: expDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Server{Cfg: config.Default(), DB: d, Exports: svc, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	var list struct{ Items []exports.Export }
	if c := getJSON(t, "GET", ts.URL+"/api/v1/exports", &list); c != 200 || len(list.Items) != 1 || list.Items[0].Request.Name != "Prueba Porton" {
		t.Fatalf("GET /api/v1/exports status=%d items=%+v", c, list.Items)
	}

	res, err := http.Get(ts.URL + "/api/v1/exports/" + id + "/video")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "video/mp4" || string(got) != string(wantBytes) {
		t.Fatalf("GET /video status=%d ct=%q got=%q", res.StatusCode, res.Header.Get("Content-Type"), string(got))
	}

	resDL, err := http.Get(ts.URL + "/api/v1/exports/" + id + "/download?format=mp4")
	if err != nil {
		t.Fatal(err)
	}
	resDL.Body.Close()
	if resDL.StatusCode != 200 || resDL.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("GET /download?format=mp4 status=%d ct=%q", resDL.StatusCode, resDL.Header.Get("Content-Type"))
	}

	if c := getJSON(t, "DELETE", ts.URL+"/api/v1/exports/"+id, nil); c != 204 {
		t.Fatalf("DELETE expected 204, got %d", c)
	}
	if c := getJSON(t, "GET", ts.URL+"/api/v1/exports/"+id, nil); c != 404 {
		t.Fatalf("GET after DELETE expected 404, got %d", c)
	}
}
