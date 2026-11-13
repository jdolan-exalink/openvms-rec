// Package volumes models recording volumes: their identity, quota policy and
// the effective write limit derived from real disk usage (PRD M04).
package volumes

import "math"

// Policy is the quota configuration of one volume. Percentages are of the
// filesystem capacity reported by the OS.
type Policy struct {
	// MaxPercent caps occupancy as a share of capacity (PRD default 90).
	MaxPercent float64
	// QuotaBytes is an optional absolute cap for this recorder (0 = none).
	QuotaBytes uint64
	// MinFreeBytes is the reserve that must always stay free on the volume.
	MinFreeBytes uint64
	// FolderQuotaBytes is a verified folder/user quota (0 = unknown/none).
	FolderQuotaBytes uint64
	// DefaultMaxPercent, CleanupStartPercent and CleanupTargetPercent are the
	// global thresholds (90/87/84 by default). Cleanup thresholds are applied
	// proportionally to the effective limit, see Compute.
	DefaultMaxPercent    float64
	CleanupStartPercent  float64
	CleanupTargetPercent float64
}

// Usage is the observed state of a volume.
type Usage struct {
	// CapacityBytes is the filesystem size.
	CapacityBytes uint64
	// FreeBytes is space the service account can allocate right now.
	FreeBytes uint64
	// RecorderBytes is the size of this recorder's segments on the volume
	// (from the index, not a directory walk).
	RecorderBytes uint64
}

// Constraint names which rule produced the effective limit.
type Constraint string

const (
	ConstraintPercent     Constraint = "quota_percent"
	ConstraintAbsolute    Constraint = "quota_absolute"
	ConstraintReserve     Constraint = "min_free_reserve"
	ConstraintAvailable   Constraint = "available_space"
	ConstraintFolderQuota Constraint = "folder_quota"
)

// Limits is the result of Compute. All values are bytes of recorder data on
// the volume, so they compare directly with Usage.RecorderBytes.
type Limits struct {
	// Effective is the most conservative cap; ingest must never exceed it.
	Effective uint64
	// Binding is the constraint that set Effective.
	Binding Constraint
	// CleanupStart triggers retention cleanup (hysteresis high mark).
	CleanupStart uint64
	// CleanupTarget is where cleanup stops (hysteresis low mark).
	CleanupTarget uint64
	// Headroom is how many more bytes may be written before reaching Effective.
	Headroom uint64
}

// Compute derives the effective limit as the minimum of:
//
//   - capacity × max_percent
//   - absolute quota (when configured)
//   - capacity − min_free (the reserve)
//   - recorder_bytes + free − min_free (space really available: other data on
//     a shared volume shrinks the limit instead of being overrun)
//   - verified folder quota (when known)
//
// Cleanup thresholds keep the configured proportions of the default maximum:
// with 90/87/84 and a 90 % limit the marks land exactly at 87 % and 84 % of
// capacity; a volume set to 80 % gets 77.3 % / 74.7 %; a shared volume whose
// limit was cut by other data gets proportionally lower marks. Scaling (rather
// than fixed offsets) guarantees target < start ≤ effective for any limit.
func Compute(p Policy, u Usage) Limits {
	type cand struct {
		v uint64
		c Constraint
	}
	maxPct := p.MaxPercent
	if maxPct <= 0 {
		maxPct = p.DefaultMaxPercent
	}
	cands := []cand{{pct(u.CapacityBytes, maxPct), ConstraintPercent}}
	if p.QuotaBytes > 0 {
		cands = append(cands, cand{p.QuotaBytes, ConstraintAbsolute})
	}
	cands = append(cands, cand{subSat(u.CapacityBytes, p.MinFreeBytes), ConstraintReserve})
	cands = append(cands, cand{subSat(u.RecorderBytes+u.FreeBytes, p.MinFreeBytes), ConstraintAvailable})
	if p.FolderQuotaBytes > 0 {
		cands = append(cands, cand{p.FolderQuotaBytes, ConstraintFolderQuota})
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.v < best.v {
			best = c
		}
	}
	l := Limits{Effective: best.v, Binding: best.c}

	defMax := p.DefaultMaxPercent
	if defMax <= 0 {
		defMax = maxPct
	}
	startRatio, targetRatio := 1.0, 1.0
	if p.CleanupStartPercent > 0 {
		startRatio = math.Min(p.CleanupStartPercent/defMax, 1)
	}
	if p.CleanupTargetPercent > 0 {
		targetRatio = math.Min(p.CleanupTargetPercent/defMax, startRatio)
	}
	l.CleanupStart = uint64(float64(l.Effective) * startRatio)
	l.CleanupTarget = uint64(float64(l.Effective) * targetRatio)
	l.Headroom = subSat(l.Effective, u.RecorderBytes)
	return l
}

// Full reports whether the volume must stop accepting new segments.
func (l Limits) Full(u Usage) bool { return u.RecorderBytes >= l.Effective }

// NeedsCleanup reports whether retention must run on space grounds.
func (l Limits) NeedsCleanup(u Usage) bool { return u.RecorderBytes >= l.CleanupStart }

// BytesToFree is how much must be deleted to reach the hysteresis low mark,
// or 0 when the high mark has not been crossed.
func (l Limits) BytesToFree(u Usage) uint64 {
	if !l.NeedsCleanup(u) {
		return 0
	}
	return subSat(u.RecorderBytes, l.CleanupTarget)
}

func pct(total uint64, percent float64) uint64 {
	if percent >= 100 {
		return total
	}
	return uint64(float64(total) * percent / 100)
}

func subSat(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

// GB is a decimal gigabyte, matching how disks are sold and the PRD formulas.
const GB = 1_000_000_000

// GBPerDay estimates storage per camera per day for a constant bitrate:
// Mbps × 10.8 (86400 s × 1e6 bit / 8 / 1e9).
func GBPerDay(mbps float64) float64 { return mbps * 86400 / 8 / 1000 }

// RetentionDays estimates how many days fit in limitBytes at the aggregate
// bitrate of all cameras writing to the volume. It ignores container overhead
// and bitrate variation and must be presented as an estimate.
func RetentionDays(limitBytes uint64, aggregateMbps float64) float64 {
	if aggregateMbps <= 0 {
		return math.Inf(1)
	}
	return float64(limitBytes) / (GBPerDay(aggregateMbps) * GB)
}
