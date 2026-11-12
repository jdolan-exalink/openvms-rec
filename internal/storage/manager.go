package storage

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/health"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/placement"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/retention"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// Manager keeps a live view of all volumes, decides placement for ingest,
// reacts to write failures and runs retention. It never formats or repairs.
type Manager struct {
	Cfg    config.Config
	Specs  []volumes.Spec
	Prober volumes.Prober
	Store  *segments.Store
	Placer *placement.Placer
	Log    *slog.Logger
	Now    func() time.Time
	// Policy returns retention policy per camera/representation.
	Policy retention.PolicyFunc

	mu       sync.RWMutex
	statuses []volumes.Status
	failed   map[string]string // volume → reason, set by ingest on IO errors
	alerts   []retention.Alert
	lastAff  map[string]string
}

// NewManager builds a manager from configuration.
func NewManager(cfg config.Config, goos string, prober volumes.Prober, store *segments.Store, log *slog.Logger) (*Manager, error) {
	p, err := placement.New(cfg.Storage.Placement)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		Cfg: cfg, Specs: SpecsFromConfig(cfg, goos), Prober: prober, Store: store, Placer: p, Log: log,
		Now: time.Now, failed: map[string]string{},
	}
	m.Policy = DefaultPolicy(cfg)
	if aff, err := store.LoadAffinity(context.Background()); err == nil {
		p.Restore(aff)
		m.lastAff = aff
	}
	return m, nil
}

// DefaultPolicy derives per-representation retention from config. Camera
// overrides are applied by the caller when cameras are configured.
func DefaultPolicy(cfg config.Config) retention.PolicyFunc {
	day := 24 * time.Hour
	return func(_, rep string) retention.CameraPolicy {
		maxDays := cfg.Retention.MainDays
		if rep == "sub" {
			maxDays = cfg.Retention.LowDays
		}
		return retention.CameraPolicy{
			MaxAge:   time.Duration(maxDays) * day,
			MinAge:   time.Duration(cfg.Retention.MinDays) * day,
			Priority: 5,
		}
	}
}

// Refresh re-inspects every volume and probes the writable ones.
func (m *Manager) Refresh(ctx context.Context) []volumes.Status {
	in := volumes.Inspector{
		Prober: m.Prober,
		Now:    m.Now,
		RecorderBytes: func(id string) uint64 {
			n, err := m.Store.RecorderBytes(ctx, id)
			if err != nil {
				m.Log.Warn("recorder bytes", "volume", id, "err", err)
			}
			return n
		},
	}
	sts := in.InspectAll(m.Specs)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range sts {
		st := &sts[i]
		if st.State != volumes.StateHealthy && st.State != volumes.StateFull {
			continue
		}
		if r := health.Probe(st.Spec.Root); !r.OK {
			st.State, st.Reason = volumes.StateDegraded, "write probe failed: "+r.Err.Error()
		} else if reason, bad := m.failed[st.Spec.ID]; bad {
			// Probe recovered: clear the ingest failure but log it.
			m.Log.Info("volume recovered after write failure", "volume", st.Spec.ID, "previous", reason)
			delete(m.failed, st.Spec.ID)
		}
		if prev := m.find(st.Spec.ID); prev != nil && prev.State != st.State {
			m.Log.Warn("volume state changed", "volume", st.Spec.ID, "from", prev.State, "to", st.State, "reason", st.Reason)
		}
	}
	m.statuses = sts
	return sts
}

func (m *Manager) find(id string) *volumes.Status {
	for i := range m.statuses {
		if m.statuses[i].Spec.ID == id {
			return &m.statuses[i]
		}
	}
	return nil
}

// Statuses returns the last inspection results.
func (m *Manager) Statuses() []volumes.Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]volumes.Status(nil), m.statuses...)
}

// Alerts returns alerts from the last retention pass.
func (m *Manager) Alerts() []retention.Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]retention.Alert(nil), m.alerts...)
}

// Choose implements the ingest volume selection.
func (m *Manager) Choose(cameraID string, sizeBytes uint64) (placement.Decision, string, error) {
	m.mu.RLock()
	cands := Candidates(m.statuses, m.Cfg.Storage)
	for i := range cands {
		if _, bad := m.failed[cands[i].ID]; bad {
			cands[i].Writable = false
		}
	}
	roots := map[string]string{}
	for _, st := range m.statuses {
		roots[st.Spec.ID] = st.Spec.Root
	}
	m.mu.RUnlock()

	d, err := m.Placer.Choose(cameraID, sizeBytes, cands)
	if err != nil {
		return d, "", err
	}
	// Reserve the headroom locally so concurrent cameras don't overshoot
	// before the next refresh.
	m.mu.Lock()
	if st := m.find(d.VolumeID); st != nil {
		if st.Limits.Headroom >= sizeBytes {
			st.Limits.Headroom -= sizeBytes
		} else {
			st.Limits.Headroom = 0
		}
	}
	m.mu.Unlock()
	return d, roots[d.VolumeID], nil
}

// MarkFailed stops placement on a volume after an IO error during ingest.
// It stays excluded until a later write probe succeeds.
func (m *Manager) MarkFailed(volumeID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, already := m.failed[volumeID]; !already {
		m.Log.Error("volume write failure; switching away", "volume", volumeID, "err", err)
	}
	m.failed[volumeID] = err.Error()
	if st := m.find(volumeID); st != nil {
		st.State, st.Reason = volumes.StateDegraded, "write failure: "+err.Error()
	}
}

// RunRetention plans and executes retention on every reachable volume.
func (m *Manager) RunRetention(ctx context.Context) {
	now := m.Now()
	var alerts []retention.Alert
	for _, st := range m.Statuses() {
		switch st.State {
		case volumes.StateMissing, volumes.StateUnready:
			continue // keep metadata; nothing to delete on an absent disk
		}
		if st.Usage.CapacityBytes == 0 {
			continue
		}
		cands, err := m.Store.RetentionCandidates(ctx, st.Spec.ID, now)
		if err != nil {
			m.Log.Error("retention candidates", "volume", st.Spec.ID, "err", err)
			continue
		}
		plan := retention.PlanVolume(retention.Input{
			Now: now, Limits: st.Limits, RecorderBytes: st.Usage.RecorderBytes, Segments: cands,
			Policy: m.Policy, PressurePolicy: m.Cfg.Storage.PressurePolicy,
		})
		for _, a := range plan.Alerts {
			m.Log.Warn("retention alert", "volume", st.Spec.ID, "code", a.Code, "camera", a.CameraID, "msg", a.Message)
		}
		alerts = append(alerts, plan.Alerts...)
		if len(plan.Deletions) == 0 {
			continue
		}
		res := retention.Execute(ctx, plan, st.Spec.Root, m.Store, now)
		m.Log.Info("retention", "volume", st.Spec.ID, "deleted", res.Deleted, "freed_bytes", res.FreedBytes,
			"skipped", res.Skipped, "errors", len(res.Errors))
		for _, e := range res.Errors {
			m.Log.Error("retention delete", "volume", st.Spec.ID, "err", e)
		}
	}
	m.mu.Lock()
	m.alerts = alerts
	m.mu.Unlock()
}

// PersistAffinity saves the placer's sticky map when it changed.
func (m *Manager) PersistAffinity(ctx context.Context) {
	snap := m.Placer.Snapshot()
	if maps.Equal(snap, m.lastAff) {
		return
	}
	if err := m.Store.SaveAffinity(ctx, snap); err != nil {
		m.Log.Error("save affinity", "err", err)
		return
	}
	m.lastAff = snap
}

// Run refreshes volumes and runs retention every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Refresh(ctx)
			m.RunRetention(ctx)
			m.Refresh(ctx) // reflect freed space immediately
			m.PersistAffinity(ctx)
		}
	}
}
