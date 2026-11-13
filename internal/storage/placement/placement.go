// Package placement chooses the volume that receives the next complete,
// decodable segment of a camera (PRD M04, ADR-002). Frames are never split
// across disks: the unit of placement is always a whole segment file.
package placement

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Policy names.
const (
	Balanced       = "balanced"
	CameraAffinity = "camera_affinity"
	Manual         = "manual"
)

// ErrNoEligibleVolume means no volume can take the segment. The caller must
// record a recording gap; it must not write elsewhere (e.g. the OS disk).
var ErrNoEligibleVolume = errors.New("placement: no eligible volume")

// Candidate is a volume as seen by the placer. Build it from volumes.Status.
type Candidate struct {
	ID       string
	Writable bool // healthy, enabled, not draining
	// Priority groups volumes into tiers: lower value is preferred. Lower tiers
	// only receive data when every volume of a better tier is ineligible.
	Priority int
	// Weight is the relative share of segments within a tier (>= 1).
	Weight int
	// HeadroomBytes is the space left below the effective limit.
	HeadroomBytes uint64
	// Cameras lists cameras assigned to this volume (camera_affinity / manual).
	Cameras []string
}

// Decision explains a choice so it can be logged and shown in the UI.
type Decision struct {
	VolumeID string
	Policy   string
	// Fallback is true when the camera's assigned volumes were all ineligible
	// and the segment went elsewhere (camera_affinity only). Raise an alert.
	Fallback bool
	Reason   string
}

// Placer is safe for concurrent use. Its sticky camera→volume map should be
// persisted (Snapshot/Restore) so choices survive restarts.
type Placer struct {
	policy string

	mu      sync.Mutex
	current map[string]int    // smooth weighted round-robin state per volume
	sticky  map[string]string // camera → last volume (camera_affinity)
}

// New returns a placer for one of the policy names.
func New(policy string) (*Placer, error) {
	switch policy {
	case Balanced, CameraAffinity, Manual:
	default:
		return nil, fmt.Errorf("placement: unknown policy %q", policy)
	}
	return &Placer{policy: policy, current: map[string]int{}, sticky: map[string]string{}}, nil
}

// Policy returns the configured policy.
func (p *Placer) Policy() string { return p.policy }

// Choose picks a volume for a segment of cameraID of roughly sizeBytes.
func (p *Placer) Choose(cameraID string, sizeBytes uint64, cands []Candidate) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	eligible := filter(cands, func(c Candidate) bool { return c.Writable && c.HeadroomBytes >= sizeBytes })
	assigned := filter(cands, func(c Candidate) bool { return slices.Contains(c.Cameras, cameraID) })
	assignedEligible := filter(eligible, func(c Candidate) bool { return slices.Contains(c.Cameras, cameraID) })

	switch p.policy {
	case Manual:
		if len(assigned) == 0 {
			return Decision{}, fmt.Errorf("%w: camera %q has no volume assigned (manual policy)", ErrNoEligibleVolume, cameraID)
		}
		if len(assignedEligible) == 0 {
			return Decision{}, fmt.Errorf("%w: all volumes assigned to camera %q are unavailable or full", ErrNoEligibleVolume, cameraID)
		}
		id := p.swrr(assignedEligible)
		return Decision{VolumeID: id, Policy: Manual, Reason: "assigned volume"}, nil

	case CameraAffinity:
		pool, fallback, reason := assignedEligible, false, "assigned volume"
		if len(assigned) == 0 {
			pool, reason = eligible, "no assignment; sticky balanced"
		} else if len(assignedEligible) == 0 {
			pool, fallback, reason = eligible, true, "assigned volumes unavailable; fell back to balanced"
		}
		if len(pool) == 0 {
			return Decision{}, fmt.Errorf("%w for camera %q", ErrNoEligibleVolume, cameraID)
		}
		if last, ok := p.sticky[cameraID]; ok && !fallback {
			if c, ok := find(pool, last); ok && c.Priority == bestPriority(pool) {
				return Decision{VolumeID: last, Policy: CameraAffinity, Reason: reason}, nil
			}
		}
		id := p.swrr(pool)
		if !fallback {
			p.sticky[cameraID] = id
		}
		return Decision{VolumeID: id, Policy: CameraAffinity, Fallback: fallback, Reason: reason}, nil

	default: // Balanced
		if len(eligible) == 0 {
			return Decision{}, fmt.Errorf("%w for camera %q", ErrNoEligibleVolume, cameraID)
		}
		id := p.swrr(eligible)
		return Decision{VolumeID: id, Policy: Balanced, Reason: "weighted round-robin"}, nil
	}
}

// swrr runs nginx-style smooth weighted round-robin over the best-priority
// tier of pool. It spreads writes evenly (no bursts on one disk) while
// honouring weights exactly over a cycle.
func (p *Placer) swrr(pool []Candidate) string {
	best := bestPriority(pool)
	total := 0
	var chosen *Candidate
	for i := range pool {
		c := &pool[i]
		if c.Priority != best {
			continue
		}
		w := max(c.Weight, 1)
		total += w
		p.current[c.ID] += w
		if chosen == nil || p.current[c.ID] > p.current[chosen.ID] ||
			(p.current[c.ID] == p.current[chosen.ID] && c.ID < chosen.ID) {
			chosen = c
		}
	}
	p.current[chosen.ID] -= total
	return chosen.ID
}

// Snapshot returns the sticky camera→volume map for persistence.
func (p *Placer) Snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.sticky))
	for k, v := range p.sticky {
		out[k] = v
	}
	return out
}

// Restore loads a map produced by Snapshot.
func (p *Placer) Restore(m map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range m {
		p.sticky[k] = v
	}
}

func bestPriority(pool []Candidate) int {
	best := pool[0].Priority
	for _, c := range pool[1:] {
		best = min(best, c.Priority)
	}
	return best
}

func filter(in []Candidate, keep func(Candidate) bool) []Candidate {
	var out []Candidate
	for _, c := range in {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func find(pool []Candidate, id string) (Candidate, bool) {
	for _, c := range pool {
		if c.ID == id {
			return c, true
		}
	}
	return Candidate{}, false
}
