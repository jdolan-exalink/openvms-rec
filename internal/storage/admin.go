package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// Volume sources: the YAML file (read-only at runtime) or the API (SQLite).
const (
	SourceConfig = "config"
	SourceAPI    = "api"
)

var (
	ErrVolumeNotFound   = errors.New("volume not found")
	ErrVolumeExists     = errors.New("a volume with that id already exists")
	ErrVolumeReadOnly   = errors.New("volume comes from the configuration file; edit recorder.yaml instead")
	ErrVolumeHasData    = errors.New("volume still has recordings indexed; disable it instead of deleting")
	ErrVolumesDisabled  = errors.New("volume changes need the recorder database")
	errVolumeNotPrepped = errors.New("volume root does not exist; tick initialize to create it")
)

// InvalidVolumeError lists every validation problem of a volume change.
type InvalidVolumeError struct{ Issues []string }

func (e *InvalidVolumeError) Error() string { return "invalid volume: " + strings.Join(e.Issues, "; ") }

// VolumeInput is a volume created or edited through the API. Pointers keep
// the stored value when nil (on update).
type VolumeInput struct {
	ID         string   `json:"id"`
	Path       string   `json:"root_path"`
	Enabled    *bool    `json:"enabled"`
	MaxPercent *float64 `json:"max_percent"`
	MinFree    *uint64  `json:"min_free_bytes"`
	Quota      *uint64  `json:"quota_bytes"`
	Priority   *int     `json:"priority"`
	Weight     *int     `json:"weight"`
	// Initialize creates the folder if missing and writes the volume marker.
	// It refuses a non-empty folder without a marker and never formats.
	Initialize bool `json:"initialize"`
}

// Specs returns the current volume specs (config first, then API).
func (m *Manager) Specs() []volumes.Spec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]volumes.Spec(nil), m.specs...)
}

// Source reports where a volume is defined, or "" if unknown.
func (m *Manager) Source(id string) string {
	for _, v := range m.Cfg.Storage.Volumes {
		if v.ID == id {
			return SourceConfig
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.extra {
		if v.ID == id {
			return SourceAPI
		}
	}
	return ""
}

// LoadVolumes reads API volumes from the database and enables mutations.
// Config volumes win on an id clash (the API row is ignored and logged).
func (m *Manager) LoadVolumes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT id, root_path, enabled, priority, weight, max_percent, quota_bytes, min_free_bytes FROM volumes ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var extra []config.Volume
	for rows.Next() {
		var v config.Volume
		var enabled bool
		var maxPct sql.NullFloat64
		var quota, minFree sql.NullInt64
		if err := rows.Scan(&v.ID, &v.Path, &enabled, &v.Priority, &v.Weight, &maxPct, &quota, &minFree); err != nil {
			return err
		}
		if m.Source(v.ID) == SourceConfig {
			m.Log.Warn("volume defined in config and database; using config", "volume", v.ID)
			continue
		}
		v.Enabled = &enabled
		v.MaxPercent = maxPct.Float64
		if quota.Valid {
			v.QuotaGB = float64(quota.Int64) / volumes.GB
		}
		if minFree.Valid {
			gb := float64(minFree.Int64) / volumes.GB
			v.MinFreeGB = &gb
		}
		extra = append(extra, v)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.chg.Lock()
	defer m.chg.Unlock()
	m.db = db
	m.setExtra(extra)
	return nil
}

// setExtra swaps the API volume list and rebuilds specs. Caller holds chg.
func (m *Manager) setExtra(extra []config.Volume) {
	all := m.Cfg
	all.Storage.Volumes = append(append([]config.Volume(nil), m.Cfg.Storage.Volumes...), extra...)
	specs := SpecsFromConfig(all, m.goos)
	m.mu.Lock()
	m.extra, m.specs = extra, specs
	m.mu.Unlock()
}

// validate checks the full volume list as the configuration would, plus
// runtime rules for API volumes (absolute paths, no nesting).
func (m *Manager) validate(list []config.Volume, changed config.Volume) error {
	cfg := m.Cfg
	cfg.Storage.Volumes = list
	var issues []string
	if err := cfg.Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if strings.Contains(line, "storage.volumes") || strings.Contains(line, "placement manual") {
				issues = append(issues, strings.TrimPrefix(strings.TrimSpace(line), "- "))
			}
		}
	}
	root := changed.ResolvedPath(m.goos)
	if !filepath.IsAbs(root) {
		issues = append(issues, "path: must be absolute (e.g. D:\\Grabaciones or /srv/recordings)")
	} else {
		clean := filepath.Clean(root)
		for _, v := range list {
			if v.ID == changed.ID {
				continue
			}
			other := filepath.Clean(v.ResolvedPath(m.goos))
			if nested(clean, other) || nested(other, clean) {
				issues = append(issues, fmt.Sprintf("path: overlaps volume %q (%s)", v.ID, other))
			}
		}
	}
	if len(issues) > 0 {
		return &InvalidVolumeError{Issues: issues}
	}
	return nil
}

// nested reports whether p is a or lies inside a.
func nested(a, p string) bool {
	if strings.EqualFold(a, p) {
		return true
	}
	rel, err := filepath.Rel(a, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func apply(v *config.Volume, in VolumeInput) {
	if in.Enabled != nil {
		e := *in.Enabled
		v.Enabled = &e
	}
	if in.MaxPercent != nil {
		v.MaxPercent = *in.MaxPercent
	}
	if in.MinFree != nil {
		g := float64(*in.MinFree) / volumes.GB
		v.MinFreeGB = &g
	}
	if in.Quota != nil {
		v.QuotaGB = float64(*in.Quota) / volumes.GB
	}
	if in.Priority != nil {
		v.Priority = *in.Priority
	}
	if in.Weight != nil {
		v.Weight = *in.Weight
	}
}

// prepare creates the folder and writes the marker when asked. It never
// touches a folder that already holds unrelated data.
func (m *Manager) prepare(v config.Volume, initialize bool) error {
	sp := SpecsFromConfig(config.Config{Storage: config.Storage{Volumes: []config.Volume{v}}}, m.goos)[0]
	if !initialize {
		if _, err := os.Stat(sp.Root); err != nil {
			return errVolumeNotPrepped
		}
		return nil
	}
	if err := os.MkdirAll(sp.Root, 0o750); err != nil {
		return fmt.Errorf("create folder: %w", err)
	}
	_, err := volumes.Initialize(m.Prober, sp, m.Now())
	return err
}

// CreateVolume validates, optionally initializes and stores a new volume.
func (m *Manager) CreateVolume(ctx context.Context, in VolumeInput) (config.Volume, error) {
	m.chg.Lock()
	defer m.chg.Unlock()
	if m.db == nil {
		return config.Volume{}, ErrVolumesDisabled
	}
	if m.Source(in.ID) != "" {
		return config.Volume{}, ErrVolumeExists
	}
	v := config.Volume{ID: in.ID, Path: strings.TrimSpace(in.Path)}
	apply(&v, in)
	list := append(append([]config.Volume(nil), m.Cfg.Storage.Volumes...), m.extra...)
	list = append(list, v)
	if err := m.validate(list, v); err != nil {
		return v, err
	}
	if err := m.prepare(v, in.Initialize); err != nil {
		if errors.Is(err, errVolumeNotPrepped) {
			return v, &InvalidVolumeError{Issues: []string{"path: " + err.Error()}}
		}
		return v, err
	}
	if err := m.save(ctx, v, true); err != nil {
		return v, err
	}
	m.setExtra(append(append([]config.Volume(nil), m.extra...), v))
	m.Log.Info("volume added", "volume", v.ID, "path", v.Path)
	m.Refresh(ctx)
	return v, nil
}

// UpdateVolume edits limits and flags of an API volume. The path is fixed:
// moving recordings is a separate, explicit operation.
func (m *Manager) UpdateVolume(ctx context.Context, id string, in VolumeInput) (config.Volume, error) {
	m.chg.Lock()
	defer m.chg.Unlock()
	if m.db == nil {
		return config.Volume{}, ErrVolumesDisabled
	}
	switch m.Source(id) {
	case "":
		return config.Volume{}, ErrVolumeNotFound
	case SourceConfig:
		return config.Volume{}, ErrVolumeReadOnly
	}
	extra := append([]config.Volume(nil), m.extra...)
	var idx int
	for i := range extra {
		if extra[i].ID == id {
			idx = i
		}
	}
	v := extra[idx]
	if p := strings.TrimSpace(in.Path); p != "" && filepath.Clean(p) != filepath.Clean(v.Path) {
		return v, &InvalidVolumeError{Issues: []string{"path: cannot be changed; add a new volume instead"}}
	}
	apply(&v, in)
	extra[idx] = v
	list := append(append([]config.Volume(nil), m.Cfg.Storage.Volumes...), extra...)
	if err := m.validate(list, v); err != nil {
		return v, err
	}
	if in.Initialize {
		if err := m.prepare(v, true); err != nil {
			return v, err
		}
	}
	if err := m.save(ctx, v, false); err != nil {
		return v, err
	}
	m.setExtra(extra)
	m.Log.Info("volume updated", "volume", v.ID)
	m.Refresh(ctx)
	return v, nil
}

// DeleteVolume removes an API volume that has no indexed recordings. Files
// on disk (including the marker) are left untouched.
func (m *Manager) DeleteVolume(ctx context.Context, id string) error {
	m.chg.Lock()
	defer m.chg.Unlock()
	if m.db == nil {
		return ErrVolumesDisabled
	}
	switch m.Source(id) {
	case "":
		return ErrVolumeNotFound
	case SourceConfig:
		return ErrVolumeReadOnly
	}
	var n int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE volume_id = ? AND status <> 'deleted'`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrVolumeHasData
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM volumes WHERE id = ?`, id); err != nil {
		return err
	}
	var extra []config.Volume
	for _, v := range m.extra {
		if v.ID != id {
			extra = append(extra, v)
		}
	}
	m.setExtra(extra)
	m.Log.Info("volume removed", "volume", id)
	m.Refresh(ctx)
	return nil
}

func (m *Manager) save(ctx context.Context, v config.Volume, insert bool) error {
	now := m.Now().Unix()
	var maxPct, quota, minFree any
	if v.MaxPercent > 0 {
		maxPct = v.MaxPercent
	}
	if v.QuotaGB > 0 {
		quota = int64(v.QuotaGB * volumes.GB)
	}
	if v.MinFreeGB != nil {
		minFree = int64(*v.MinFreeGB * volumes.GB)
	}
	if insert {
		_, err := m.db.ExecContext(ctx, `INSERT INTO volumes(id, root_path, enabled, priority, weight, max_percent, quota_bytes, min_free_bytes, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, v.ID, v.Path, v.IsEnabled(), v.Priority, v.EffectiveWeight(), maxPct, quota, minFree, now, now)
		return err
	}
	_, err := m.db.ExecContext(ctx, `UPDATE volumes SET enabled = ?, priority = ?, weight = ?, max_percent = ?, quota_bytes = ?, min_free_bytes = ?, updated_at = ? WHERE id = ?`,
		v.IsEnabled(), v.Priority, v.EffectiveWeight(), maxPct, quota, minFree, now, v.ID)
	return err
}
