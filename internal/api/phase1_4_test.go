package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/auth"
	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/core"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

func TestPhase1To4Endpoints(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	volA := filepath.Join(base, "a", "rec")
	volB := filepath.Join(base, "b", "rec")
	os.MkdirAll(volA, 0o755)
	os.MkdirAll(volB, 0o755)

	cfg := config.Default()
	cfg.Data.Dir = base
	cfg.Storage.Volumes = []config.Volume{
		{ID: "vol-a", Path: volA, PathWindows: volA},
		{ID: "vol-b", Path: volB, PathWindows: volB},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := segments.New(d)
	mgr, err := storage.NewManager(cfg, runtime.GOOS, fakeProber{}, store, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.LoadVolumes(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, sp := range mgr.Specs() {
		if _, err := volumes.Initialize(fakeProber{}, sp, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	mgr.Refresh(ctx)

	cams, err := cameras.Open(ctx, cameras.Options{
		DB:         d,
		CredsFile:  filepath.Join(base, "creds.yaml"),
		SecretsKey: config.DeriveSecretsKey([]byte("test-secret-key-32-bytes-long!!")),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cams.Create(ctx, cameras.Input{
		ID: "cam1", Name: "Camara 1", MainURL: "rtsp://127.0.0.1:8554/stream1",
	})
	if err != nil {
		t.Fatal(err)
	}

	users := auth.NewUserStore(d)
	_, _ = users.EnsureDefaultAdmin(ctx)
	tokens, err := auth.NewTokenSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	coreAd, err := core.New(ctx, d, config.DeriveSecretsKey([]byte("test-secret-key-32-bytes-long!!")), cfg.Core, "vtest", log)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		Cfg: cfg, DB: d, Prober: fakeProber{}, Storage: mgr, Cameras: cams,
		Users: users, Tokens: tokens, Core: coreAd, Version: "vtest", Log: log,
	}
	s.Auth = &auth.Authenticator{
		VerifyToken: func(tok string) (string, bool) {
			sess, ok := tokens.Verify(tok)
			if !ok || auth.IsTokenRevoked(ctx, d, tok) {
				return "", false
			}
			return sess.Username, true
		},
		Public: func(p string) bool { return p == "/api/v1/auth/login" || p == "/health/live" },
	}
	coreAd.BuildManifest = func(c context.Context) core.Manifest { return s.CoreManifestSnapshot(c) }

	ts := httptest.NewServer(s.Auth.Middleware(s.Handler()))
	t.Cleanup(ts.Close)

	// 1. Login and verify session token works, then logout revokes it.
	var loginResp struct {
		Token string `json:"token"`
	}
	if code := send(t, "POST", ts.URL+"/api/v1/auth/login", `{"username":"admin","password":"`+auth.DefaultAdminPass+`"}`, &loginResp); code != 200 {
		t.Fatalf("login failed %d", code)
	}
	authGet := func(path string) int {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+loginResp.Token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := authGet("/api/v1/auth/me"); c != 200 {
		t.Fatalf("auth/me expected 200, got %d", c)
	}

	// Authenticate subsequent calls using a fresh second token so logout test is isolated.
	tok2, err := tokens.Issue("admin", "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	doAuth := func(method, path, body string, dst any) int {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rdr)
		req.Header.Set("Authorization", "Bearer "+tok2)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if dst != nil {
			_ = json.NewDecoder(resp.Body).Decode(dst)
		}
		return resp.StatusCode
	}
	// Revoke first token via /api/v1/auth/logout.
	reqOut, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/logout", nil)
	reqOut.Header.Set("Authorization", "Bearer "+loginResp.Token)
	respOut, err := http.DefaultClient.Do(reqOut)
	if err != nil || respOut.StatusCode != 200 {
		t.Fatalf("logout failed: %v %v", err, respOut)
	}
	respOut.Body.Close()
	if c := authGet("/api/v1/auth/me"); c != 401 {
		t.Fatalf("revoked token expected 401, got %d", c)
	}

	// 2. Camera recording start/stop.
	var recSt map[string]any
	if c := doAuth("POST", "/api/v1/cameras/cam1/recording/stop", "", &recSt); c != 202 || recSt["recording_enabled"] != false {
		t.Fatalf("stop recording: code=%d body=%+v", c, recSt)
	}
	var camDTO CameraDTO
	if c := doAuth("GET", "/api/v1/cameras/cam1", "", &camDTO); c != 200 || camDTO.RecordingEnabled {
		t.Fatalf("camera dto recording_enabled expected false: code=%d dto=%+v", c, camDTO)
	}
	if c := doAuth("POST", "/api/v1/cameras/cam1/recording/start", "", &recSt); c != 202 || recSt["recording_enabled"] != true {
		t.Fatalf("start recording: code=%d body=%+v", c, recSt)
	}

	// 3. Storage policies GET + PATCH + validation.
	var polOut struct {
		Items []storage.StoragePolicy `json:"items"`
	}
	if c := doAuth("GET", "/api/v1/storage/policies", "", &polOut); c != 200 || len(polOut.Items) != 1 {
		t.Fatalf("list policies %d %+v", c, polOut)
	}
	if c := doAuth("PATCH", "/api/v1/storage/policies/default", `{"cleanup_target_percent":95}`, nil); c != 422 {
		t.Fatalf("invalid policy threshold expected 422, got %d", c)
	}
	var updatedPol storage.StoragePolicy
	if c := doAuth("PATCH", "/api/v1/storage/policies/default", `{"placement":"camera_affinity"}`, &updatedPol); c != 200 || updatedPol.Placement != "camera_affinity" {
		t.Fatalf("patch policy: %d %+v", c, updatedPol)
	}

	// 4. Volume drain + checksum-verified segment migration.
	rel := "cam1/main/2026/11/17/18/seg1.ts"
	srcPath := filepath.Join(volA, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(srcPath), 0o750)
	payload := []byte("fake-ts-payload-for-drain-migration-test")
	_ = os.WriteFile(srcPath, payload, 0o640)
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])

	segID, err := store.Begin(ctx, segments.Segment{
		CameraID: "cam1", RepresentationID: "main", Start: time.Now().Add(-time.Minute), VolumeID: "vol-a", RelativePath: rel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finalize(ctx, segID, time.Now(), uint64(len(payload)), hexSum, ""); err != nil {
		t.Fatal(err)
	}

	var drainVol VolumeDTO
	if c := doAuth("POST", "/api/v1/storage/volumes/vol-a/drain", `{"migrate_segments":true}`, &drainVol); c != 202 || drainVol.State != "draining" {
		t.Fatalf("drain failed: %d %+v", c, drainVol)
	}
	// Wait briefly for migration goroutine to finish moving seg1.ts from vol-a to vol-b.
	deadline := time.Now().Add(3 * time.Second)
	migrated := false
	for time.Now().Before(deadline) {
		var volID string
		var gen int
		if err := d.QueryRowContext(ctx, `SELECT volume_id, generation FROM segments WHERE id = ?`, segID).Scan(&volID, &gen); err == nil && volID == "vol-b" && gen == 2 {
			if _, statErr := os.Stat(filepath.Join(volB, filepath.FromSlash(rel))); statErr == nil {
				migrated = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !migrated {
		t.Fatal("segment was not migrated from vol-a to vol-b during drain")
	}

	// 5. Phase 4 Core adapter endpoints & manifest.
	var coreMan core.Manifest
	if c := doAuth("GET", "/api/v1/core/manifest", "", &coreMan); c != 200 || len(coreMan.Cameras) != 1 {
		t.Fatalf("core manifest: %d %+v", c, coreMan)
	}
	mockCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mockCore.Close)
	var coreSt core.Status
	if c := doAuth("PUT", "/api/v1/core/config", `{"enabled":true,"core_url":"`+mockCore.URL+`","api_token":"s3cr3t"}`, &coreSt); c != 200 || !coreSt.Enabled || !coreSt.HasToken {
		t.Fatalf("core config: %d %+v", c, coreSt)
	}
	if c := doAuth("POST", "/api/v1/core/sync", "", &coreSt); c != 200 || coreSt.Status != "connected" {
		t.Fatalf("core sync: %d %+v", c, coreSt)
	}
}
