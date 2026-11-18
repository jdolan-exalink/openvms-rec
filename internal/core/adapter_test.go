package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func TestAdapterConfigEncryptionSyncAndOfflineFallback(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	// Seed 2 local events to verify cursor-based batch push.
	now := time.Now().UTC().UnixMilli()
	_, err = d.ExecContext(ctx, `INSERT INTO events(id, camera_id, timestamp_utc, type, source, dedupe_key, created_at)
		VALUES ('ev-1', 'cam1', ?, 'person', 'tvt', 'k1', ?), ('ev-2', 'cam1', ?, 'vehicle', 'tvt', 'k2', ?)`,
		now-2000, now-2000, now-1000, now-1000)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var heartbeats int
	var gotSigOK bool
	var syncedEventIDs []string
	var failCore bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shouldFail := failCore
		mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("core-secret-tok"))
		mac.Write(raw)
		wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if r.Header.Get("X-OpenVMS-Signature") == wantSig {
			mu.Lock()
			gotSigOK = true
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/api/v1/recorders/heartbeat":
			mu.Lock()
			heartbeats++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case "/api/v1/recorders/events":
			var body struct {
				Events []OutboundEvent `json:"events"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			for _, e := range body.Events {
				syncedEventIDs = append(syncedEventIDs, e.EventID)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	secKey := config.DeriveSecretsKey([]byte("0123456789abcdef0123456789abcdef"))
	ad, err := New(ctx, d, secKey, config.CoreConfig{SyncIntervalSeconds: 30}, "vtest", nil)
	if err != nil {
		t.Fatal(err)
	}
	ad.BuildManifest = func(context.Context) Manifest {
		return Manifest{Capabilities: map[string]any{"sd_auto_sync": true}}
	}

	en := true
	urlStr := srv.URL
	nodeID := "nvr-test"
	siteID := "site-a"
	tok := "core-secret-tok"
	st, err := ad.UpdateConfig(ctx, ConfigPatch{
		Enabled:  &en,
		CoreURL:  &urlStr,
		NodeID:   &nodeID,
		SiteID:   &siteID,
		APIToken: &tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !st.HasToken || st.PendingEvents != 2 {
		t.Fatalf("unexpected initial status: %+v", st)
	}

	// Verify token stored encrypted at rest in SQLite, never plaintext.
	var storedEnc string
	if err := d.QueryRowContext(ctx, `SELECT token_enc FROM core_adapter_config WHERE id = 1`).Scan(&storedEnc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedEnc, tok) || !strings.HasPrefix(storedEnc, "enc:v1:") {
		t.Fatalf("core token must be encrypted at rest, got %q", storedEnc)
	}

	// Reload adapter from DB to confirm persistence and decryption work.
	ad2, err := New(ctx, d, secKey, config.CoreConfig{}, "vtest", nil)
	if err != nil {
		t.Fatal(err)
	}
	ad2.BuildManifest = ad.BuildManifest
	if err := ad2.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	st2 := ad2.Status(ctx)
	if st2.Status != "connected" || st2.EventsSynced != 2 || st2.PendingEvents != 0 || st2.LastEventID != "ev-2" {
		t.Fatalf("unexpected status after sync: %+v", st2)
	}

	mu.Lock()
	if heartbeats != 1 || !gotSigOK || len(syncedEventIDs) != 2 {
		t.Fatalf("unexpected server observations: hb=%d sig=%v evs=%v", heartbeats, gotSigOK, syncedEventIDs)
	}
	failCore = true
	mu.Unlock()

	// Offline/degraded fallback never drops cursor or crashes.
	if err := ad2.SyncOnce(ctx); err == nil {
		t.Fatal("expected error when Core returns 503")
	}
	st3 := ad2.Status(ctx)
	if st3.Status != "degraded" || st3.EventsSynced != 2 {
		t.Fatalf("expected degraded keeping synced=2, got %+v", st3)
	}
}
