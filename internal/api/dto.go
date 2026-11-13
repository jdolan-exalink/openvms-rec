package api

import (
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// VolumeDTO is the Volume schema of openapi.yaml.
type VolumeDTO struct {
	ID          string   `json:"id"`
	RootPath    string   `json:"root_path"`
	StableID    string   `json:"stable_id"`
	MountPoint  string   `json:"mount_point"`
	FSType      string   `json:"fs_type"`
	Enabled     bool     `json:"enabled"`
	State       string   `json:"state"`
	Reason      string   `json:"state_reason,omitempty"`
	Writable    bool     `json:"writable"`
	Priority    int      `json:"priority"`
	Weight      int      `json:"weight"`
	Cameras     []string `json:"cameras"`
	IsSystem    bool     `json:"is_system"`
	IsNetwork   bool     `json:"is_network"`
	IsRemovable bool     `json:"is_removable"`
	Warnings    []string `json:"warnings"`

	CapacityBytes uint64 `json:"capacity_bytes"`
	FreeBytes     uint64 `json:"free_bytes"`
	RecorderBytes uint64 `json:"recorder_bytes"`

	Quota  QuotaDTO  `json:"quota"`
	Limits LimitsDTO `json:"limits"`
	Probe  *ProbeDTO `json:"probe,omitempty"`

	InspectedAt time.Time `json:"inspected_at"`
}

type QuotaDTO struct {
	MaxPercent   float64 `json:"max_percent"`
	QuotaBytes   uint64  `json:"quota_bytes,omitempty"`
	MinFreeBytes uint64  `json:"min_free_bytes"`
}

type LimitsDTO struct {
	EffectiveBytes     uint64 `json:"effective_bytes"`
	Binding            string `json:"binding_constraint"`
	CleanupStartBytes  uint64 `json:"cleanup_start_bytes"`
	CleanupTargetBytes uint64 `json:"cleanup_target_bytes"`
	HeadroomBytes      uint64 `json:"headroom_bytes"`
}

type ProbeDTO struct {
	OK        bool    `json:"ok"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

// SummaryDTO is the StorageSummary schema.
type SummaryDTO struct {
	Volumes             int            `json:"volumes"`
	WritableVolumes     int            `json:"writable_volumes"`
	ByState             map[string]int `json:"by_state"`
	EffectiveLimitBytes uint64         `json:"effective_limit_bytes"`
	HeadroomBytes       uint64         `json:"headroom_bytes"`
	RecorderBytes       uint64         `json:"recorder_bytes"`
	Placement           string         `json:"placement"`
	PressurePolicy      string         `json:"pressure_policy"`
	Warnings            []string       `json:"warnings"`
	Alerts              []AlertDTO     `json:"alerts"`
}

// AlertDTO is a storage/retention alert.
type AlertDTO struct {
	Code     string `json:"code"`
	CameraID string `json:"camera_id,omitempty"`
	Message  string `json:"message"`
}

// StreamStatus is the ingest state of one camera stream.
type StreamStatus struct {
	CameraID       string    `json:"camera_id"`
	Representation string    `json:"representation"`
	Source         string    `json:"source"` // credentials redacted
	State          string    `json:"state"`
	LastError      string    `json:"last_error,omitempty"`
	Reconnects     int64     `json:"reconnects"`
	Segments       int64     `json:"segments"`
	Bytes          int64     `json:"bytes"`
	LastWrite      time.Time `json:"last_write,omitzero"`
	BitrateKbps    float64   `json:"bitrate_kbps"`
	Audio          string    `json:"audio"`
	PID            int       `json:"pid,omitempty"` // FFmpeg process
}

// ToVolumeDTO converts an inspected volume to the API representation (also
// used by `storage check -json`).
func ToVolumeDTO(st volumes.Status, writable bool) VolumeDTO { return toDTO(st, writable) }

func toDTO(st volumes.Status, writable bool) VolumeDTO {
	cams := st.Spec.Cameras
	if cams == nil {
		cams = []string{}
	}
	warn := st.Warnings
	if warn == nil {
		warn = []string{}
	}
	return VolumeDTO{
		ID: st.Spec.ID, RootPath: st.Spec.Root, StableID: st.Info.StableID, MountPoint: st.Info.MountPoint,
		FSType: st.Info.FSType, Enabled: st.Spec.Enabled, State: string(st.State), Reason: st.Reason,
		Writable: writable, Priority: st.Spec.Priority, Weight: st.Spec.Weight, Cameras: cams,
		IsSystem: st.Info.IsSystem, IsNetwork: st.Info.IsNetwork, IsRemovable: st.Info.IsRemovable,
		Warnings: warn, CapacityBytes: st.Usage.CapacityBytes, FreeBytes: st.Usage.FreeBytes,
		RecorderBytes: st.Usage.RecorderBytes,
		Quota:         QuotaDTO{MaxPercent: st.Spec.Policy.MaxPercent, QuotaBytes: st.Spec.Policy.QuotaBytes, MinFreeBytes: st.Spec.Policy.MinFreeBytes},
		Limits: LimitsDTO{
			EffectiveBytes: st.Limits.Effective, Binding: string(st.Limits.Binding),
			CleanupStartBytes: st.Limits.CleanupStart, CleanupTargetBytes: st.Limits.CleanupTarget,
			HeadroomBytes: st.Limits.Headroom,
		},
		InspectedAt: st.InspectedAt,
	}
}
