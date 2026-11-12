// Package app wires configuration, storage, ingest and the API into the
// running service.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/api"
	"github.com/jdolan-exalink/openvms-rec/internal/auth"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/exports"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/playback"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/retention"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// App is the running recorder.
type App struct {
	Cfg       config.Config
	DB        *sql.DB
	Store     *segments.Store
	Storage   *storage.Manager
	Recorders map[string]*ingest.Recorder // key: camera/representation
	Exports   *exports.Service
	Signer    *playback.Signer
	Log       *slog.Logger
	Version   string

	// StorageInterval is how often volumes are inspected and retention runs.
	StorageInterval time.Duration
	Auth            *auth.Authenticator
}

// New opens the database and builds all components. Nothing runs yet.
func New(ctx context.Context, cfg config.Config, prober volumes.Prober, log *slog.Logger, version string) (*App, error) {
	if err := os.MkdirAll(cfg.Data.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	d, err := db.Open(ctx, filepath.Join(cfg.Data.Dir, "recorder.db"))
	if err != nil {
		return nil, err
	}
	store := segments.New(d)
	mgr, err := storage.NewManager(cfg, runtime.GOOS, prober, store, log)
	if err != nil {
		d.Close()
		return nil, err
	}
	mgr.Policy = CameraPolicy(cfg)
	a := &App{Cfg: cfg, DB: d, Store: store, Storage: mgr, Recorders: map[string]*ingest.Recorder{},
		Log: log, Version: version, StorageInterval: 30 * time.Second}

	keys, err := auth.LoadKeys(cfg.Server.APIKeysFile)
	if err != nil {
		d.Close()
		return nil, err
	}
	if len(keys) == 0 && !auth.IsLoopback(cfg.Server.Listen) {
		d.Close()
		return nil, fmt.Errorf("refusing to listen on %s without API keys: create one with `openvms-recorder apikey create NAME` and set server.api_keys_file", cfg.Server.Listen)
	}
	a.Auth = &auth.Authenticator{Keys: keys, Public: func(p string) bool {
		return p == "/" || strings.HasPrefix(p, "/health/") || strings.HasPrefix(p, "/api/v1/playback/")
	}}
	a.Signer, err = playback.NewSigner()
	if err != nil {
		d.Close()
		return nil, err
	}
	a.Exports = &exports.Service{DB: d, Store: store, Dir: filepath.Join(cfg.Data.Dir, "exports"),
		FFmpeg: cfg.Recording.FFmpegPath, Version: version, Log: log, Root: a.volumeRoot}

	secrets, err := config.LoadSecrets(cfg.Data.SecretsFile)
	if err != nil {
		d.Close()
		return nil, err
	}
	for _, cam := range cfg.Cameras {
		if !cam.IsEnabled() {
			continue
		}
		streams := []struct{ rep, url string }{{"main", cam.MainURL}}
		if cfg.Recording.DefaultStream == "sub" && cam.SubURL != "" {
			streams[0] = struct{ rep, url string }{"sub", cam.SubURL}
		}
		if (cam.DualRecording || cfg.Recording.DualRecording) && cam.SubURL != "" && streams[0].rep == "main" {
			streams = append(streams, struct{ rep, url string }{"sub", cam.SubURL})
		}
		for _, st := range streams {
			u, err := secrets.StreamURL(st.url, cam.CredentialsRef)
			if err != nil {
				d.Close()
				return nil, fmt.Errorf("camera %s: %w", cam.ID, err)
			}
			sink := &ingest.Sink{CameraID: cam.ID, Representation: st.rep, Volumes: mgr, Index: store, Log: log}
			a.Recorders[cam.ID+"/"+st.rep] = &ingest.Recorder{
				Source:  ingest.Source{URL: u, Transport: cam.Transport, Audio: cam.Audio},
				FFmpeg:  cfg.Recording.FFmpegPath,
				Segment: time.Duration(cfg.Recording.SegmentSeconds) * time.Second,
				Sink:    sink,
				Log:     log,
			}
		}
	}
	return a, nil
}

// CameraPolicy applies per-camera retention overrides on top of globals.
func CameraPolicy(cfg config.Config) retention.PolicyFunc {
	base := storage.DefaultPolicy(cfg)
	cams := map[string]config.Camera{}
	for _, c := range cfg.Cameras {
		cams[c.ID] = c
	}
	day := 24 * time.Hour
	return func(cameraID, rep string) retention.CameraPolicy {
		p := base(cameraID, rep)
		c, ok := cams[cameraID]
		if !ok {
			return p
		}
		if c.Priority != 0 {
			p.Priority = c.Priority
		}
		if r := c.Retention; r != nil {
			if rep == "main" && r.MainDays != nil {
				p.MaxAge = time.Duration(*r.MainDays) * day
			}
			if rep == "sub" && r.SubDays != nil {
				p.MaxAge = time.Duration(*r.SubDays) * day
			}
			if r.MinDays != nil {
				p.MinAge = time.Duration(*r.MinDays) * day
			}
		}
		return p
	}
}

// Recover reconciles every reachable volume and records the outage while the
// recorder was down as gaps. Call before starting recorders.
func (a *App) Recover(ctx context.Context) {
	if err := a.Exports.RecoverJobs(ctx); err != nil {
		a.Log.Error("recover export jobs", "err", err)
	}
	for _, st := range a.Storage.Refresh(ctx) {
		if st.State == volumes.StateMissing || st.State == volumes.StateUnready {
			a.Log.Warn("volume not available at startup; its segments stay indexed", "volume", st.Spec.ID, "state", st.State, "reason", st.Reason)
			continue
		}
		res, err := ingest.Reconcile(ctx, a.Store, st.Spec.ID, st.Spec.Root, a.Log)
		if err != nil {
			a.Log.Error("reconcile", "volume", st.Spec.ID, "err", err)
			continue
		}
		if res != (ingest.ReconcileResult{}) {
			a.Log.Info("reconciled volume", "volume", st.Spec.ID, "recovered", res.Recovered, "corrupt", res.Corrupt, "deletes", res.Deletes)
		}
	}
	now := time.Now()
	for _, r := range a.Recorders {
		cam, rep := r.Sink.CameraID, r.Sink.Representation
		closed, err := a.Store.CloseOpenGaps(ctx, cam, rep, now)
		if err != nil {
			a.Log.Error("close stale gaps", "camera", cam, "err", err)
			continue
		}
		if closed > 0 {
			continue // previous run already recorded why it stopped
		}
		// Unclean stop (crash, power loss): record the outage explicitly.
		last, err := a.Store.LastSegmentEnd(ctx, cam, rep)
		if err == nil && !last.IsZero() && now.Sub(last) > 2*r.Segment {
			if id, err := a.Store.OpenGap(ctx, cam, rep, last, "recorder_down"); err == nil {
				a.Store.CloseGap(ctx, id, now)
			}
		}
	}
	a.Storage.Refresh(ctx)
}

// Run starts storage monitoring, all recorders and the HTTP server, and
// blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	a.Recover(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.Storage.Run(ctx, a.StorageInterval) }()
	for key, r := range a.Recorders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Log.Info("recording", "stream", key, "source", ingest.Redact(r.Source.URL))
			r.Run(ctx)
		}()
	}

	srv := &http.Server{
		Addr:              a.Cfg.Server.Listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.Log.Info("recorder started", "listen", a.Cfg.Server.Listen, "version", a.Version,
		"volumes", len(a.Storage.Specs), "streams", len(a.Recorders))

	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	wg.Wait() // recorders close their open segments cleanly
	a.Storage.PersistAffinity(context.Background())
	a.Log.Info("recorder stopped")
	return err
}

// Handler returns the API router.
func (a *App) Handler() http.Handler {
	h := (&api.Server{Cfg: a.Cfg, DB: a.DB, Prober: a.Storage.Prober, Version: a.Version, Log: a.Log,
		Storage: a.Storage, Streams: a.streams, Exports: a.Exports, Signer: a.Signer}).Handler()
	return api.SecurityHeaders(a.Auth.Middleware(h))
}

func (a *App) volumeRoot(id string) (string, bool) {
	for _, st := range a.Storage.Statuses() {
		if st.Spec.ID == id {
			return st.Spec.Root, st.State != volumes.StateMissing && st.State != volumes.StateUnready
		}
	}
	return "", false
}

func (a *App) streams() []api.StreamStatus {
	out := make([]api.StreamStatus, 0, len(a.Recorders))
	for _, r := range a.Recorders {
		m := r.Metrics.Snapshot()
		out = append(out, api.StreamStatus{
			CameraID: r.Sink.CameraID, Representation: r.Sink.Representation, Source: ingest.Redact(r.Source.URL),
			State: string(m.State), LastError: m.LastError, Reconnects: m.Reconnects, Segments: m.Segments,
			Bytes: m.Bytes, LastWrite: m.LastWrite, BitrateKbps: m.BitrateKbps, Audio: m.Audio,
		})
	}
	return out
}

// Close releases resources.
func (a *App) Close() error { return a.DB.Close() }
