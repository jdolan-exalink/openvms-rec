// Package core implements the optional Phase 4 OpenVMS Core federation
// adapter. OpenVMS Recorder stays completely autonomous when the adapter is
// disabled or when OpenVMS Core is unreachable.
package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
)

// Config holds persisted/runtime OpenVMS Core bridge settings.
type Config struct {
	Enabled               bool   `json:"enabled"`
	CoreURL               string `json:"core_url"`
	NodeID                string `json:"node_id"`
	SiteID                string `json:"site_id"`
	APIToken              string `json:"api_token,omitempty"` // write-only; never returned on GET
	SyncIntervalSeconds   int    `json:"sync_interval_seconds"`
	PushEvents            bool   `json:"push_events"`
	PushHealth            bool   `json:"push_health"`
	TLSInsecureSkipVerify bool   `json:"tls_insecure_skip_verify"`
}

// ConfigPatch mutates adapter settings cleanly without forcing re-entry of unchanged fields.
type ConfigPatch struct {
	Enabled               *bool   `json:"enabled"`
	CoreURL               *string `json:"core_url"`
	NodeID                *string `json:"node_id"`
	SiteID                *string `json:"site_id"`
	APIToken              *string `json:"api_token"`
	SyncIntervalSeconds   *int    `json:"sync_interval_seconds"`
	PushEvents            *bool   `json:"push_events"`
	PushHealth            *bool   `json:"push_health"`
	TLSInsecureSkipVerify *bool   `json:"tls_insecure_skip_verify"`
}

// Status is the redacted status returned by GET /api/v1/core/status.
type Status struct {
	Enabled               bool   `json:"enabled"`
	Status                string `json:"status"` // disabled | connected | degraded | offline
	CoreURL               string `json:"core_url"`
	NodeID                string `json:"node_id"`
	SiteID                string `json:"site_id"`
	HasToken              bool   `json:"has_token"`
	SyncIntervalSeconds   int    `json:"sync_interval_seconds"`
	PushEvents            bool   `json:"push_events"`
	PushHealth            bool   `json:"push_health"`
	TLSInsecureSkipVerify bool   `json:"tls_insecure_skip_verify"`
	LastSyncUTC           string `json:"last_sync_utc,omitempty"`
	EventsSynced          int64  `json:"events_synced"`
	LastEventID           string `json:"last_event_id,omitempty"`
	PendingEvents         int64  `json:"pending_events"`
	LastError             string `json:"last_error,omitempty"`
}

// Manifest captures an OpenVMS Core federation snapshot returned by
// GET /api/v1/core/manifest and sent in outbound heartbeats.
type Manifest struct {
	SchemaVersion string         `json:"schema_version"`
	NodeID        string         `json:"node_id"`
	SiteID        string         `json:"site_id"`
	Version       string         `json:"version"`
	OS            string         `json:"os"`
	Arch          string         `json:"arch"`
	GeneratedAt   string         `json:"generated_at"`
	Capabilities  map[string]any `json:"capabilities"`
	Cameras       []any          `json:"cameras"`
	Storage       any            `json:"storage"`
	Streams       []any          `json:"streams"`
	EventCursor   CursorInfo     `json:"event_cursor"`
}

type CursorInfo struct {
	LastEventMS   int64  `json:"last_event_ms"`
	LastEventID   string `json:"last_event_id,omitempty"`
	EventsSynced  int64  `json:"events_synced"`
	PendingEvents int64  `json:"pending_events"`
}

type OutboundEvent struct {
	EventID      string   `json:"event_id"`
	CameraID     string   `json:"camera_id"`
	TimestampUTC string   `json:"timestamp_utc"`
	TimestampMS  int64    `json:"timestamp_ms"`
	Type         string   `json:"type"`
	Class        string   `json:"class,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
	Zone         string   `json:"zone,omitempty"`
	Source       string   `json:"source"`
	SnapshotPath string   `json:"snapshot_path,omitempty"`
}

// Adapter coordinates OpenVMS Core registration, periodic heartbeats, and
// cursor-based event streaming while keeping Recorder autonomous on failures.
type Adapter struct {
	DB            *sql.DB
	SecretsKey    []byte
	DefaultConfig config.CoreConfig
	Version       string
	BuildManifest func(ctx context.Context) Manifest
	HTTPClient    *http.Client
	Log           *slog.Logger
	Now           func() time.Time

	mu          sync.Mutex
	cfg         Config
	status      string
	lastSync    time.Time
	lastError   string
	lastEventMS int64
	lastEventID string
	eventsSync  int64
	wakeup      chan struct{}
}

// New builds a Core adapter from DB state + YAML defaults.
func New(ctx context.Context, db *sql.DB, secretsKey []byte, def config.CoreConfig, version string, log *slog.Logger) (*Adapter, error) {
	if log == nil {
		log = slog.Default()
	}
	a := &Adapter{
		DB:            db,
		SecretsKey:    append([]byte(nil), secretsKey...),
		DefaultConfig: def,
		Version:       version,
		Log:           log,
		Now:           time.Now,
		wakeup:        make(chan struct{}, 1),
		status:        "disabled",
	}
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Adapter) defaultCfg() Config {
	node := strings.TrimSpace(a.DefaultConfig.NodeID)
	if node == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			node = h
		} else {
			node = "openvms-recorder"
		}
	}
	iv := a.DefaultConfig.SyncIntervalSeconds
	if iv <= 0 {
		iv = 30
	}
	pe := a.DefaultConfig.PushEvents == nil || *a.DefaultConfig.PushEvents
	ph := a.DefaultConfig.PushHealth == nil || *a.DefaultConfig.PushHealth
	return Config{
		Enabled:               a.DefaultConfig.Enabled,
		CoreURL:               strings.TrimRight(strings.TrimSpace(a.DefaultConfig.CoreURL), "/"),
		NodeID:                node,
		SiteID:                strings.TrimSpace(a.DefaultConfig.SiteID),
		SyncIntervalSeconds:   iv,
		PushEvents:            pe,
		PushHealth:            ph,
		TLSInsecureSkipVerify: a.DefaultConfig.TLSInsecureSkipVerify,
	}
}

func (a *Adapter) load(ctx context.Context) error {
	cfg := a.defaultCfg()
	if a.DB == nil {
		a.mu.Lock()
		a.cfg = cfg
		if cfg.Enabled {
			a.status = "offline"
		}
		a.mu.Unlock()
		return nil
	}

	var enabled, pushEv, pushHealth, tlsSkip bool
	var coreURL, nodeID, siteID, tokenEnc string
	var interval int
	err := a.DB.QueryRowContext(ctx, `SELECT enabled, core_url, node_id, site_id, token_enc, sync_interval_seconds, push_events, push_health, tls_insecure_skip_verify
		FROM core_adapter_config WHERE id = 1`).Scan(&enabled, &coreURL, &nodeID, &siteID, &tokenEnc, &interval, &pushEv, &pushHealth, &tlsSkip)
	if err == nil {
		cfg.Enabled = enabled
		cfg.CoreURL = strings.TrimRight(strings.TrimSpace(coreURL), "/")
		if strings.TrimSpace(nodeID) != "" {
			cfg.NodeID = strings.TrimSpace(nodeID)
		}
		cfg.SiteID = strings.TrimSpace(siteID)
		if interval > 0 {
			cfg.SyncIntervalSeconds = interval
		}
		cfg.PushEvents = pushEv
		cfg.PushHealth = pushHealth
		cfg.TLSInsecureSkipVerify = tlsSkip
		if tokenEnc != "" {
			if dec, dErr := config.DecryptBytes(a.SecretsKey, tokenEnc); dErr == nil {
				cfg.APIToken = strings.TrimSpace(string(dec))
			}
		}
	}

	var lastMS, synced, syncUTC int64
	var lastID, st, lastErr string
	if sErr := a.DB.QueryRowContext(ctx, `SELECT last_event_ms, last_event_id, events_synced, last_sync_utc, status, last_error
		FROM core_adapter_state WHERE id = 1`).Scan(&lastMS, &lastID, &synced, &syncUTC, &st, &lastErr); sErr == nil {
		a.lastEventMS = lastMS
		a.lastEventID = lastID
		a.eventsSync = synced
		if syncUTC > 0 {
			a.lastSync = time.UnixMilli(syncUTC).UTC()
		}
		if st != "" {
			a.status = st
		}
		a.lastError = lastErr
	}

	a.mu.Lock()
	a.cfg = cfg
	if !cfg.Enabled {
		a.status = "disabled"
	} else if a.status == "disabled" {
		a.status = "offline"
	}
	a.mu.Unlock()
	return nil
}

// UpdateConfig validates, encrypts APIToken at rest, saves, and wakes the loop.
func (a *Adapter) UpdateConfig(ctx context.Context, p ConfigPatch) (Status, error) {
	a.mu.Lock()
	next := a.cfg
	a.mu.Unlock()

	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	if p.CoreURL != nil {
		next.CoreURL = strings.TrimRight(strings.TrimSpace(*p.CoreURL), "/")
	}
	if p.NodeID != nil && strings.TrimSpace(*p.NodeID) != "" {
		next.NodeID = strings.TrimSpace(*p.NodeID)
	}
	if p.SiteID != nil {
		next.SiteID = strings.TrimSpace(*p.SiteID)
	}
	if p.APIToken != nil {
		next.APIToken = strings.TrimSpace(*p.APIToken)
	}
	if p.SyncIntervalSeconds != nil {
		next.SyncIntervalSeconds = *p.SyncIntervalSeconds
	}
	if next.SyncIntervalSeconds <= 0 {
		next.SyncIntervalSeconds = 30
	}
	if next.SyncIntervalSeconds > 3600 {
		return Status{}, errors.New("sync_interval_seconds must be in [5, 3600]")
	}
	if p.PushEvents != nil {
		next.PushEvents = *p.PushEvents
	}
	if p.PushHealth != nil {
		next.PushHealth = *p.PushHealth
	}
	if p.TLSInsecureSkipVerify != nil {
		next.TLSInsecureSkipVerify = *p.TLSInsecureSkipVerify
	}
	if next.Enabled {
		u, err := url.Parse(next.CoreURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Status{}, errors.New("core_url must be a valid http:// or https:// URL when enabled")
		}
	}

	var tokenEnc string
	if next.APIToken != "" {
		if len(a.SecretsKey) == 32 {
			enc, err := config.EncryptBytes(a.SecretsKey, []byte(next.APIToken))
			if err != nil {
				return Status{}, fmt.Errorf("encrypt core token: %w", err)
			}
			tokenEnc = enc
		} else {
			tokenEnc = next.APIToken
		}
	}
	if a.DB != nil {
		now := a.Now().UTC().UnixMilli()
		if _, err := a.DB.ExecContext(ctx, `INSERT INTO core_adapter_config
			(id, enabled, core_url, node_id, site_id, token_enc, sync_interval_seconds, push_events, push_health, tls_insecure_skip_verify, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				enabled=excluded.enabled, core_url=excluded.core_url, node_id=excluded.node_id, site_id=excluded.site_id,
				token_enc=excluded.token_enc, sync_interval_seconds=excluded.sync_interval_seconds,
				push_events=excluded.push_events, push_health=excluded.push_health,
				tls_insecure_skip_verify=excluded.tls_insecure_skip_verify, updated_at=excluded.updated_at`,
			next.Enabled, next.CoreURL, next.NodeID, next.SiteID, tokenEnc, next.SyncIntervalSeconds,
			next.PushEvents, next.PushHealth, next.TLSInsecureSkipVerify, now); err != nil {
			return Status{}, err
		}
	}

	a.mu.Lock()
	a.cfg = next
	if !next.Enabled {
		a.status = "disabled"
		a.lastError = ""
	} else {
		a.status = "offline"
	}
	a.mu.Unlock()
	a.persistState(ctx)

	select {
	case a.wakeup <- struct{}{}:
	default:
	}
	return a.Status(ctx), nil
}

// Status returns a sanitized view of the adapter config and sync metrics.
func (a *Adapter) Status(ctx context.Context) Status {
	a.mu.Lock()
	st := Status{
		Enabled:               a.cfg.Enabled,
		Status:                a.status,
		CoreURL:               a.cfg.CoreURL,
		NodeID:                a.cfg.NodeID,
		SiteID:                a.cfg.SiteID,
		HasToken:              a.cfg.APIToken != "",
		SyncIntervalSeconds:   a.cfg.SyncIntervalSeconds,
		PushEvents:            a.cfg.PushEvents,
		PushHealth:            a.cfg.PushHealth,
		TLSInsecureSkipVerify: a.cfg.TLSInsecureSkipVerify,
		EventsSynced:          a.eventsSync,
		LastEventID:           a.lastEventID,
		LastError:             a.lastError,
	}
	if !a.lastSync.IsZero() {
		st.LastSyncUTC = a.lastSync.UTC().Format(time.RFC3339)
	}
	lastMS := a.lastEventMS
	lastID := a.lastEventID
	a.mu.Unlock()

	if a.DB != nil {
		st.PendingEvents = a.countPendingEvents(ctx, lastMS, lastID)
	}
	return st
}

// Manifest builds the complete federation payload for Core (used by both
// GET /api/v1/core/manifest and outbound heartbeat pushes).
func (a *Adapter) Manifest(ctx context.Context) Manifest {
	var m Manifest
	if a.BuildManifest != nil {
		m = a.BuildManifest(ctx)
	}
	a.mu.Lock()
	m.SchemaVersion = "openvms-rec.v1"
	if m.NodeID == "" {
		m.NodeID = a.cfg.NodeID
	}
	if m.SiteID == "" {
		m.SiteID = a.cfg.SiteID
	}
	m.Version = a.Version
	m.OS = runtime.GOOS
	m.Arch = runtime.GOARCH
	m.GeneratedAt = a.Now().UTC().Format(time.RFC3339)
	m.EventCursor = CursorInfo{
		LastEventMS:  a.lastEventMS,
		LastEventID:  a.lastEventID,
		EventsSynced: a.eventsSync,
	}
	a.mu.Unlock()
	if a.DB != nil {
		m.EventCursor.PendingEvents = a.countPendingEvents(ctx, m.EventCursor.LastEventMS, m.EventCursor.LastEventID)
	}
	return m
}

// Start runs periodic Core heartbeat + event sync until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context) {
	for {
		a.mu.Lock()
		enabled := a.cfg.Enabled
		iv := time.Duration(a.cfg.SyncIntervalSeconds) * time.Second
		a.mu.Unlock()
		if iv < 5*time.Second {
			iv = 30 * time.Second
		}
		if enabled {
			_ = a.SyncOnce(ctx)
		}
		timer := time.NewTimer(iv)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.wakeup:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// SyncOnce executes one heartbeat + event-batch sync with OpenVMS Core.
func (a *Adapter) SyncOnce(ctx context.Context) error {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	if !cfg.Enabled || cfg.CoreURL == "" {
		a.mu.Lock()
		a.status = "disabled"
		a.lastError = ""
		a.mu.Unlock()
		return nil
	}

	syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var errs []string
	if cfg.PushHealth {
		m := a.Manifest(syncCtx)
		if err := a.postJSON(syncCtx, cfg, "/api/v1/recorders/heartbeat", m); err != nil {
			errs = append(errs, "heartbeat: "+err.Error())
		}
	}
	if cfg.PushEvents && a.DB != nil {
		if err := a.syncEventBatch(syncCtx, cfg); err != nil {
			errs = append(errs, "events: "+err.Error())
		}
	}

	now := a.Now().UTC()
	a.mu.Lock()
	if len(errs) == 0 {
		a.status = "connected"
		a.lastError = ""
		a.lastSync = now
	} else {
		msg := strings.Join(errs, "; ")
		if strings.Contains(msg, "HTTP 4") || strings.Contains(msg, "HTTP 5") {
			a.status = "degraded"
		} else {
			a.status = "offline"
		}
		a.lastError = msg
	}
	a.mu.Unlock()
	a.persistState(ctx)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (a *Adapter) syncEventBatch(ctx context.Context, cfg Config) error {
	a.mu.Lock()
	lastMS := a.lastEventMS
	lastID := a.lastEventID
	a.mu.Unlock()

	rows, err := a.DB.QueryContext(ctx, `SELECT id, camera_id, timestamp_utc, type, class, confidence, zone, source, snapshot_ref
		FROM events
		WHERE (timestamp_utc > ? OR (timestamp_utc = ? AND id > ?))
		ORDER BY timestamp_utc ASC, id ASC
		LIMIT 100`, lastMS, lastMS, lastID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var batch []OutboundEvent
	for rows.Next() {
		var ev OutboundEvent
		var cls, zone, snap sql.NullString
		var conf sql.NullFloat64
		if err := rows.Scan(&ev.EventID, &ev.CameraID, &ev.TimestampMS, &ev.Type, &cls, &conf, &zone, &ev.Source, &snap); err != nil {
			return err
		}
		ev.TimestampUTC = time.UnixMilli(ev.TimestampMS).UTC().Format(time.RFC3339)
		ev.Class = cls.String
		if conf.Valid {
			v := conf.Float64
			ev.Confidence = &v
		}
		ev.Zone = zone.String
		if snap.Valid && snap.String != "" {
			ev.SnapshotPath = "/api/v1/events/" + ev.EventID + "/snapshot"
		}
		batch = append(batch, ev)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}

	payload := map[string]any{
		"node_id": cfg.NodeID,
		"site_id": cfg.SiteID,
		"events":  batch,
	}
	if err := a.postJSON(ctx, cfg, "/api/v1/recorders/events", payload); err != nil {
		return err
	}
	last := batch[len(batch)-1]
	a.mu.Lock()
	a.lastEventMS = last.TimestampMS
	a.lastEventID = last.EventID
	a.eventsSync += int64(len(batch))
	a.mu.Unlock()
	return nil
}

func (a *Adapter) countPendingEvents(ctx context.Context, lastMS int64, lastID string) int64 {
	var n int64
	_ = a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE (timestamp_utc > ? OR (timestamp_utc = ? AND id > ?))`,
		lastMS, lastMS, lastID).Scan(&n)
	return n
}

func (a *Adapter) postJSON(ctx context.Context, cfg Config, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CoreURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenVMS-Node", cfg.NodeID)
	if cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
		mac := hmac.New(sha256.New, []byte(cfg.APIToken))
		mac.Write(raw)
		req.Header.Set("X-OpenVMS-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	cli := a.httpClient(cfg.TLSInsecureSkipVerify)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (a *Adapter) httpClient(skipVerify bool) *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if skipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // explicit user opt-in for lab Core certs
	}
	return &http.Client{Transport: tr, Timeout: 8 * time.Second}
}

func (a *Adapter) persistState(ctx context.Context) {
	if a.DB == nil {
		return
	}
	a.mu.Lock()
	ms, id, synced, st, errStr := a.lastEventMS, a.lastEventID, a.eventsSync, a.status, a.lastError
	var syncUTC int64
	if !a.lastSync.IsZero() {
		syncUTC = a.lastSync.UnixMilli()
	}
	a.mu.Unlock()

	_, _ = a.DB.ExecContext(ctx, `INSERT INTO core_adapter_state(id, last_event_ms, last_event_id, events_synced, last_sync_utc, status, last_error)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			last_event_ms=excluded.last_event_ms, last_event_id=excluded.last_event_id,
			events_synced=excluded.events_synced, last_sync_utc=excluded.last_sync_utc,
			status=excluded.status, last_error=excluded.last_error`,
		ms, id, synced, syncUTC, st, errStr)
}
