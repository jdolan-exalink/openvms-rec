// Package retention decides which segments to delete on a volume (PRD M04).
//
// The planner is pure: it receives the volume's limits and its segments and
// returns a plan. Executing the plan (file removal + index update) is done by
// Execute against small interfaces, so the rules are testable without disks.
//
// Rules, in order:
//
//  1. Never delete a segment that is protected (protection_until in the
//     future: evidence, user pin, event policy), held (export in progress) or
//     still being written.
//  2. Age: delete segments older than their camera/representation max age.
//  3. Space: when recorder bytes cross the hysteresis high mark, delete the
//     oldest eligible segments until the low mark is reached. Segments still
//     inside their minimum retention are only touched after all older data,
//     and only under the overwrite pressure policy; lower-priority cameras go
//     first. Every such deletion raises an alert.
//  4. If protected data alone keeps the volume above target, report it: the
//     volume will fill and stop accepting segments. Evidence is never deleted
//     silently.
package retention

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

// Pressure policies (mirror config).
const (
	OverwriteOldest = "overwrite_oldest_unprotected"
	ProtectMinimum  = "protect_minimum_retention"
)

// Segment is the subset of the segment index retention needs.
type Segment struct {
	ID             int64
	CameraID       string
	Representation string // "main", "sub"
	Start, End     time.Time
	SizeBytes      uint64
	ProtectedUntil time.Time // zero = not protected
	Held           bool      // export or migration in progress
	Writing        bool      // not finalized yet
}

// Protected reports whether rule 1 forbids deleting s at now.
func (s Segment) Protected(now time.Time) bool {
	return s.Held || s.Writing || s.ProtectedUntil.After(now)
}

// CameraPolicy is the retention policy for one camera/representation.
type CameraPolicy struct {
	// MaxAge deletes data older than this regardless of space (0 = keep until space is needed).
	MaxAge time.Duration
	// MinAge is the guaranteed retention under normal operation.
	MinAge time.Duration
	// Priority: higher value = more important; lower priority loses data first under pressure.
	Priority int
}

// PolicyFunc returns the policy for a camera/representation.
type PolicyFunc func(cameraID, representation string) CameraPolicy

// Reason explains a deletion.
type Reason string

const (
	ReasonAge           Reason = "max_age"
	ReasonSpace         Reason = "space"
	ReasonSpaceUnderMin Reason = "space_below_min_retention"
)

// Deletion is one planned removal.
type Deletion struct {
	SegmentID int64
	CameraID  string
	SizeBytes uint64
	Reason    Reason
}

// Alert codes.
const (
	AlertMinRetentionViolated = "min_retention_violated"
	AlertRetentionPressure    = "retention_pressure"
	AlertProtectedExceeds     = "protected_data_exceeds_target"
)

// Alert is surfaced to operators and the event log.
type Alert struct {
	Code     string
	CameraID string
	Message  string
}

// Plan is the planner output.
type Plan struct {
	Deletions  []Deletion
	FreedBytes uint64
	// BytesAfter is recorder bytes on the volume once the plan executes.
	BytesAfter uint64
	// ShortfallBytes is how far above the low mark the volume remains.
	ShortfallBytes uint64
	// StopWrites asks ingest to stop placing segments on this volume because
	// it could not get below its effective limit.
	StopWrites bool
	Alerts     []Alert
}

// Input bundles planner parameters.
type Input struct {
	Now            time.Time
	Limits         volumes.Limits
	RecorderBytes  uint64
	Segments       []Segment
	Policy         PolicyFunc
	PressurePolicy string
}

// PlanVolume computes the deletions for one volume.
func PlanVolume(in Input) Plan {
	var p Plan
	used := in.RecorderBytes
	deleted := map[int64]bool{}
	del := func(s Segment, r Reason) {
		deleted[s.ID] = true
		p.Deletions = append(p.Deletions, Deletion{SegmentID: s.ID, CameraID: s.CameraID, SizeBytes: s.SizeBytes, Reason: r})
		p.FreedBytes += s.SizeBytes
		used -= min(used, s.SizeBytes)
	}

	segs := slices.Clone(in.Segments)
	slices.SortFunc(segs, func(a, b Segment) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.ID, b.ID))
	})

	// Rule 2: age.
	for _, s := range segs {
		pol := in.Policy(s.CameraID, s.Representation)
		if pol.MaxAge > 0 && s.End.Before(in.Now.Add(-pol.MaxAge)) && !s.Protected(in.Now) {
			del(s, ReasonAge)
		}
	}

	// Rule 3: space with hysteresis.
	usage := volumes.Usage{RecorderBytes: used}
	if in.Limits.NeedsCleanup(usage) {
		target := in.Limits.CleanupTarget
		// 3a: oldest first among segments past their minimum retention.
		var underMin []Segment
		for _, s := range segs {
			if used <= target {
				break
			}
			if deleted[s.ID] || s.Protected(in.Now) {
				continue
			}
			pol := in.Policy(s.CameraID, s.Representation)
			if pol.MinAge > 0 && s.End.After(in.Now.Add(-pol.MinAge)) {
				underMin = append(underMin, s)
				continue
			}
			del(s, ReasonSpace)
		}
		// 3b: inside minimum retention, lowest priority camera first, then oldest.
		if used > target && len(underMin) > 0 {
			if in.PressurePolicy == ProtectMinimum {
				p.Alerts = append(p.Alerts, Alert{
					Code:    AlertRetentionPressure,
					Message: fmt.Sprintf("volume cannot reach cleanup target without breaking minimum retention; %d bytes over target", used-target),
				})
			} else {
				slices.SortStableFunc(underMin, func(a, b Segment) int {
					pa := in.Policy(a.CameraID, a.Representation).Priority
					pb := in.Policy(b.CameraID, b.Representation).Priority
					return cmp.Compare(pa, pb) // Start order preserved by stable sort
				})
				violated := map[string]bool{}
				for _, s := range underMin {
					if used <= target {
						break
					}
					del(s, ReasonSpaceUnderMin)
					violated[s.CameraID] = true
				}
				for _, cam := range sortedKeys(violated) {
					p.Alerts = append(p.Alerts, Alert{
						Code:     AlertMinRetentionViolated,
						CameraID: cam,
						Message:  "minimum retention not met: storage pressure forced deletion of recent footage",
					})
				}
			}
		}
		// Rule 4.
		if used > target {
			p.ShortfallBytes = used - target
			if onlyProtectedLeft(segs, deleted, in.Now) {
				p.Alerts = append(p.Alerts, Alert{
					Code:    AlertProtectedExceeds,
					Message: fmt.Sprintf("protected or in-use segments keep the volume %d bytes above its cleanup target", p.ShortfallBytes),
				})
			}
		}
	}
	p.BytesAfter = used
	p.StopWrites = used >= in.Limits.Effective
	return p
}

func onlyProtectedLeft(segs []Segment, deleted map[int64]bool, now time.Time) bool {
	for _, s := range segs {
		if !deleted[s.ID] && !s.Protected(now) {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
