package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

func TestVolumeAdmin(t *testing.T) {
	base := t.TempDir()
	cfg := config.Default()
	yamlRoot := filepath.Join(base, "d", "rec")
	os.MkdirAll(yamlRoot, 0o755)
	cfg.Storage.Volumes = []config.Volume{{ID: "yaml", Path: yamlRoot, PathWindows: yamlRoot}}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := segments.New(d)
	mgr, err := storage.NewManager(cfg, runtime.GOOS, fakeProber{}, store, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.LoadVolumes(ctx, d); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: cfg, DB: d, Prober: fakeProber{}, Storage: mgr, Log: log}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	api := ts.URL + "/api/v1/storage/volumes"
	js := func(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

	newRoot := filepath.Join(base, "f", "grabaciones")
	var e struct {
		Error struct {
			Code    string
			Details []string
		}
	}
	// Missing folder without initialize is refused; relative path too.
	if c := send(t, "POST", api, `{"id":"nuevo","root_path":"`+js(newRoot)+`"}`, &e); c != 422 {
		t.Fatalf("missing folder %d", c)
	}
	if c := send(t, "POST", api, `{"id":"rel","root_path":"grabaciones","initialize":true}`, &e); c != 422 {
		t.Fatalf("relative %d %+v", c, e)
	}
	// Nested inside the YAML volume is refused.
	if c := send(t, "POST", api, `{"id":"nest","root_path":"`+js(filepath.Join(yamlRoot, "x"))+`","initialize":true}`, &e); c != 422 {
		t.Fatalf("nested %d", c)
	}
	// Duplicate of a config volume.
	if c := send(t, "POST", api, `{"id":"yaml","root_path":"`+js(newRoot)+`","initialize":true}`, &e); c != 409 {
		t.Fatalf("dup %d", c)
	}

	var v VolumeDTO
	if c := send(t, "POST", api, `{"id":"nuevo","root_path":"`+js(newRoot)+`","initialize":true,"max_percent":80,"min_free_bytes":5000000000,"priority":2}`, &v); c != 201 {
		t.Fatalf("create %d", c)
	}
	if v.Source != "api" || !v.Editable || v.Quota.MaxPercent != 80 || v.Priority != 2 || v.State != string(volumes.StateHealthy) {
		t.Fatalf("%+v", v)
	}
	if _, err := volumes.ReadMarker(newRoot); err != nil {
		t.Fatalf("marker: %v", err)
	}

	// A non-empty folder without marker is never initialized.
	dirty := filepath.Join(base, "g", "datos")
	os.MkdirAll(dirty, 0o755)
	os.WriteFile(filepath.Join(dirty, "foto.jpg"), []byte("x"), 0o644)
	if c := send(t, "POST", api, `{"id":"sucio","root_path":"`+js(dirty)+`","initialize":true}`, &e); c != 409 || e.Error.Code != "volume_not_empty" {
		t.Fatalf("dirty %d %+v", c, e)
	}
	if mgr.Source("sucio") != "" {
		t.Fatal("dirty volume saved")
	}

	if c := send(t, "PATCH", api+"/nuevo", `{"max_percent":70,"enabled":false}`, &v); c != 200 || v.Quota.MaxPercent != 70 || v.Enabled {
		t.Fatalf("update %d %+v", c, v)
	}
	if c := send(t, "PUT", api+"/nuevo", `{"root_path":"`+js(filepath.Join(base, "z"))+`"}`, &e); c != 422 {
		t.Fatalf("path change %d", c)
	}
	if c := send(t, "PUT", api+"/yaml", `{"max_percent":70}`, &e); c != 409 || e.Error.Code != "volume_read_only" {
		t.Fatalf("yaml update %d", c)
	}

	// Reload from the database keeps the edit.
	mgr2, _ := storage.NewManager(cfg, runtime.GOOS, fakeProber{}, store, log)
	if err := mgr2.LoadVolumes(ctx, d); err != nil {
		t.Fatal(err)
	}
	sp := mgr2.Specs()
	if len(sp) != 2 || sp[1].ID != "nuevo" || sp[1].Enabled || sp[1].Policy.MaxPercent != 70 || sp[1].Policy.MinFreeBytes != 5*volumes.GB {
		t.Fatalf("%+v", sp)
	}

	// Recordings block deletion.
	id, err := store.Begin(ctx, segments.Segment{CameraID: "c", VolumeID: "nuevo", RelativePath: "c/a.ts", Start: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if c := send(t, "DELETE", api+"/nuevo", "", &e); c != 409 || e.Error.Code != "volume_has_recordings" {
		t.Fatalf("delete with data %d", c)
	}
	d.Exec(`DELETE FROM segments WHERE id = ?`, id)
	if c := send(t, "DELETE", api+"/nuevo", "", nil); c != 204 {
		t.Fatalf("delete %d", c)
	}
	if c := send(t, "DELETE", api+"/yaml", "", &e); c != 409 {
		t.Fatalf("yaml delete %d", c)
	}
	if _, err := os.Stat(filepath.Join(newRoot, volumes.MarkerFile)); err != nil {
		t.Fatal("delete must leave files on disk")
	}
}
