package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/placement"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

var (
	ErrPolicyNotFound = errors.New("storage policy not found")
	ErrInvalidPolicy  = errors.New("invalid storage policy")
)

// StoragePolicy mirrors components.schemas.StoragePolicy in api/openapi.yaml.
type StoragePolicy struct {
	ID                   string  `json:"id"`
	DefaultMaxPercent    float64 `json:"default_max_percent"`
	CleanupStartPercent  float64 `json:"cleanup_start_percent"`
	CleanupTargetPercent float64 `json:"cleanup_target_percent"`
	MinFreeBytes         uint64  `json:"min_free_bytes"`
	Placement            string  `json:"placement"`
	PressurePolicy       string  `json:"pressure_policy"`
}

// StoragePolicyPatch updates one or more fields of StoragePolicy.
type StoragePolicyPatch struct {
	DefaultMaxPercent    *float64 `json:"default_max_percent"`
	CleanupStartPercent  *float64 `json:"cleanup_start_percent"`
	CleanupTargetPercent *float64 `json:"cleanup_target_percent"`
	MinFreeBytes         *uint64  `json:"min_free_bytes"`
	Placement            *string  `json:"placement"`
	PressurePolicy       *string  `json:"pressure_policy"`
}

// StorageConfig returns a snapshot of the live Storage configuration.
func (m *Manager) StorageConfig() config.Storage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Cfg.Storage
}

// CurrentPolicy returns the active default storage policy.
func (m *Manager) CurrentPolicy() StoragePolicy {
	s := m.StorageConfig()
	return StoragePolicy{
		ID:                   "default",
		DefaultMaxPercent:    s.DefaultMaxPercent,
		CleanupStartPercent:  s.CleanupStartPercent,
		CleanupTargetPercent: s.CleanupTargetPercent,
		MinFreeBytes:         uint64(s.MinFreeGB * volumes.GB),
		Placement:            s.Placement,
		PressurePolicy:       s.PressurePolicy,
	}
}

func (m *Manager) loadPolicyLocked(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	var p StoragePolicy
	var minFree int64
	err := db.QueryRowContext(ctx, `SELECT id, default_max_percent, cleanup_start_percent, cleanup_target_percent, min_free_bytes, placement, pressure_policy
		FROM storage_policies WHERE id = 'default'`).Scan(
		&p.ID, &p.DefaultMaxPercent, &p.CleanupStartPercent, &p.CleanupTargetPercent, &minFree, &p.Placement, &p.PressurePolicy,
	)
	if err != nil {
		return
	}
	if minFree >= 0 {
		p.MinFreeBytes = uint64(minFree)
	}
	m.applyPolicyLocked(p)
}

func (m *Manager) applyPolicyLocked(p StoragePolicy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Cfg.Storage.DefaultMaxPercent = p.DefaultMaxPercent
	m.Cfg.Storage.CleanupStartPercent = p.CleanupStartPercent
	m.Cfg.Storage.CleanupTargetPercent = p.CleanupTargetPercent
	m.Cfg.Storage.MinFreeGB = float64(p.MinFreeBytes) / volumes.GB
	m.Cfg.Storage.Placement = p.Placement
	m.Cfg.Storage.PressurePolicy = p.PressurePolicy
	if pl, err := placement.New(p.Placement); err == nil {
		pl.Restore(m.Placer.Snapshot())
		m.Placer = pl
	}
}

// UpdatePolicy validates, persists, and hot-applies global storage policy changes.
func (m *Manager) UpdatePolicy(ctx context.Context, id string, patch StoragePolicyPatch) (StoragePolicy, error) {
	if id != "default" {
		return StoragePolicy{}, ErrPolicyNotFound
	}
	m.chg.Lock()
	defer m.chg.Unlock()
	if m.db == nil {
		return StoragePolicy{}, ErrVolumesDisabled
	}

	m.mu.RLock()
	cfg := m.Cfg.Storage
	m.mu.RUnlock()

	if patch.DefaultMaxPercent != nil {
		cfg.DefaultMaxPercent = *patch.DefaultMaxPercent
	}
	if patch.CleanupStartPercent != nil {
		cfg.CleanupStartPercent = *patch.CleanupStartPercent
	}
	if patch.CleanupTargetPercent != nil {
		cfg.CleanupTargetPercent = *patch.CleanupTargetPercent
	}
	if patch.MinFreeBytes != nil {
		cfg.MinFreeGB = float64(*patch.MinFreeBytes) / volumes.GB
	}
	if patch.Placement != nil {
		cfg.Placement = *patch.Placement
	}
	if patch.PressurePolicy != nil {
		cfg.PressurePolicy = *patch.PressurePolicy
	}

	if issues := config.ValidateStoragePolicy(cfg); len(issues) > 0 {
		return StoragePolicy{}, &InvalidVolumeError{Issues: issues}
	}

	now := m.Now().UnixMilli()
	minBytes := int64(cfg.MinFreeGB * volumes.GB)
	if _, err := m.db.ExecContext(ctx, `INSERT INTO storage_policies
		(id, default_max_percent, cleanup_start_percent, cleanup_target_percent, min_free_bytes, placement, pressure_policy, updated_at)
		VALUES ('default', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			default_max_percent = excluded.default_max_percent,
			cleanup_start_percent = excluded.cleanup_start_percent,
			cleanup_target_percent = excluded.cleanup_target_percent,
			min_free_bytes = excluded.min_free_bytes,
			placement = excluded.placement,
			pressure_policy = excluded.pressure_policy,
			updated_at = excluded.updated_at`,
		cfg.DefaultMaxPercent, cfg.CleanupStartPercent, cfg.CleanupTargetPercent, minBytes, cfg.Placement, cfg.PressurePolicy, now); err != nil {
		return StoragePolicy{}, err
	}

	out := StoragePolicy{
		ID:                   "default",
		DefaultMaxPercent:    cfg.DefaultMaxPercent,
		CleanupStartPercent:  cfg.CleanupStartPercent,
		CleanupTargetPercent: cfg.CleanupTargetPercent,
		MinFreeBytes:         uint64(minBytes),
		Placement:            cfg.Placement,
		PressurePolicy:       cfg.PressurePolicy,
	}
	m.applyPolicyLocked(out)
	m.setExtra(m.extra)
	m.Refresh(ctx)
	return out, nil
}

// SetVolumeRuntime persists runtime overrides (draining state and/or camera assignments)
// for either a config-defined or API-defined volume without mutating recorder.yaml.
func (m *Manager) SetVolumeRuntime(ctx context.Context, id string, draining *bool, cameras *[]string) error {
	m.chg.Lock()
	defer m.chg.Unlock()
	if m.db == nil {
		return ErrVolumesDisabled
	}
	if m.Source(id) == "" {
		return ErrVolumeNotFound
	}
	if m.runtimeOverrides == nil {
		m.runtimeOverrides = map[string]VolumeRuntime{}
	}
	ov := m.runtimeOverrides[id]
	if draining != nil {
		b := *draining
		ov.Draining = &b
	}
	var camsJSON any
	if cameras != nil {
		ov.HasCameras = true
		ov.Cameras = append([]string(nil), (*cameras)...)
		b, _ := json.Marshal(ov.Cameras)
		camsJSON = string(b)
	}
	m.runtimeOverrides[id] = ov

	now := m.Now().UnixMilli()
	var drVal any
	if ov.Draining != nil {
		drVal = *ov.Draining
	}
	if _, err := m.db.ExecContext(ctx, `INSERT INTO volume_runtime(volume_id, draining, cameras_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(volume_id) DO UPDATE SET
			draining = COALESCE(excluded.draining, volume_runtime.draining),
			cameras_json = COALESCE(excluded.cameras_json, volume_runtime.cameras_json),
			updated_at = excluded.updated_at`,
		id, drVal, camsJSON, now); err != nil {
		return err
	}
	if m.Source(id) == SourceAPI && draining != nil {
		_, _ = m.db.ExecContext(ctx, `UPDATE volumes SET draining = ?, updated_at = ? WHERE id = ?`, *draining, now/1000, id)
	}
	if m.Source(id) == SourceAPI && cameras != nil && camsJSON != nil {
		for i := range m.extra {
			if m.extra[i].ID == id {
				m.extra[i].Cameras = append([]string(nil), (*cameras)...)
			}
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE volumes SET cameras_json = ?, updated_at = ? WHERE id = ?`, camsJSON, now/1000, id)
	}
	m.setExtra(m.extra)
	m.Refresh(ctx)
	return nil
}

// DrainVolume places id into StateDraining so no new segments are written to it.
// When migrateSegments is true, a checksum-verified migration job is queued to
// move existing final segments to healthy writable volumes.
func (m *Manager) DrainVolume(ctx context.Context, id string, migrateSegments bool) (string, error) {
	dr := true
	if err := m.SetVolumeRuntime(ctx, id, &dr, nil); err != nil {
		return "", err
	}
	m.Log.Info("volume set to draining", "volume", id, "migrate_segments", migrateSegments)
	if !migrateSegments || m.Store == nil {
		return "", nil
	}

	var raw [8]byte
	_, _ = rand.Read(raw[:])
	jobID := "mig_" + hex.EncodeToString(raw[:])
	now := m.Now().UnixMilli()
	payload, _ := json.Marshal(map[string]any{
		"source_volume": id,
		"moved":         0,
		"total":         0,
	})
	if _, err := m.db.ExecContext(ctx, `INSERT INTO jobs(id, kind, state, payload, progress, error, created_at, updated_at)
		VALUES (?, 'migrate_segments', 'running', ?, 0, '', ?, ?)`, jobID, string(payload), now, now); err != nil {
		return "", err
	}
	go m.runMigrateJob(jobID, id)
	return jobID, nil
}

func (m *Manager) runMigrateJob(jobID, sourceVolID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	finish := func(state string, moved, total int, errMsg string) {
		p, _ := json.Marshal(map[string]any{
			"source_volume": sourceVolID,
			"moved":         moved,
			"total":         total,
		})
		prog := 1.0
		if total > 0 && state == "running" {
			prog = float64(moved) / float64(total)
		} else if state == "failed" {
			prog = 0
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE jobs SET state = ?, payload = ?, progress = ?, error = ?, updated_at = ? WHERE id = ?`,
			state, string(p), prog, errMsg, m.Now().UnixMilli(), jobID)
	}

	srcRoot := ""
	for _, sp := range m.Specs() {
		if sp.ID == sourceVolID {
			srcRoot = sp.Root
			break
		}
	}
	if srcRoot == "" {
		finish("failed", 0, 0, "source volume root not found")
		return
	}

	segs, err := m.Store.FinalOnVolume(ctx, sourceVolID)
	if err != nil {
		finish("failed", 0, 0, err.Error())
		return
	}
	total := len(segs)
	moved := 0
	for _, seg := range segs {
		if ctx.Err() != nil {
			finish("failed", moved, total, ctx.Err().Error())
			return
		}
		dec, dstRoot, err := m.Choose(seg.CameraID, seg.SizeBytes)
		if err != nil {
			finish("failed", moved, total, fmt.Sprintf("no healthy destination volume: %v", err))
			return
		}
		if dec.VolumeID == sourceVolID {
			finish("failed", moved, total, "no alternate writable destination volume")
			return
		}
		srcFile := filepath.Join(srcRoot, filepath.FromSlash(seg.RelativePath))
		dstFile := filepath.Join(dstRoot, filepath.FromSlash(seg.RelativePath))
		sum, size, err := copyVerifySHA256(srcFile, dstFile)
		if err != nil {
			finish("failed", moved, total, fmt.Sprintf("migrate segment %d: %v", seg.ID, err))
			return
		}
		if seg.Checksum != "" && seg.Checksum != sum {
			_ = os.Remove(dstFile)
			finish("failed", moved, total, fmt.Sprintf("checksum mismatch on segment %d", seg.ID))
			return
		}
		if err := m.Store.Relocate(ctx, seg.ID, sourceVolID, dec.VolumeID, seg.RelativePath, sum, size); err != nil {
			_ = os.Remove(dstFile)
			finish("failed", moved, total, err.Error())
			return
		}
		_ = os.Remove(srcFile)
		moved++
		finish("running", moved, total, "")
	}
	finish("succeeded", moved, total, "")
	m.Refresh(ctx)
	m.Log.Info("volume segment migration completed", "volume", sourceVolID, "moved", moved, "total", total)
}

func copyVerifySHA256(src, dst string) (string, uint64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, err
	}
	tmp := dst + ".mig.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}
