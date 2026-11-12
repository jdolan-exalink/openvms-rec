package placement

import (
	"errors"
	"testing"
)

const GB = 1_000_000_000

func vols() []Candidate {
	return []Candidate{
		{ID: "d", Writable: true, Weight: 1, HeadroomBytes: 100 * GB},
		{ID: "e", Writable: true, Weight: 1, HeadroomBytes: 100 * GB},
		{ID: "f", Writable: true, Weight: 2, HeadroomBytes: 100 * GB},
	}
}

func TestBalancedHonoursWeightsSmoothly(t *testing.T) {
	p, _ := New(Balanced)
	counts := map[string]int{}
	var seq []string
	for i := 0; i < 400; i++ {
		d, err := p.Choose("cam1", GB, vols())
		if err != nil {
			t.Fatal(err)
		}
		counts[d.VolumeID]++
		seq = append(seq, d.VolumeID)
	}
	if counts["d"] != 100 || counts["e"] != 100 || counts["f"] != 200 {
		t.Fatalf("counts=%v", counts)
	}
	// Smoothness: never 3 consecutive segments on the same disk with these weights.
	for i := 2; i < len(seq); i++ {
		if seq[i] == seq[i-1] && seq[i] == seq[i-2] {
			t.Fatalf("hotspot at %d: %v", i, seq[i-2:i+1])
		}
	}
}

func TestBalancedSkipsUnhealthyAndFull(t *testing.T) {
	p, _ := New(Balanced)
	c := vols()
	c[0].Writable = false       // ST-04: d disconnected
	c[2].HeadroomBytes = GB / 2 // f nearly at its limit
	for i := 0; i < 10; i++ {
		d, err := p.Choose("cam1", GB, c)
		if err != nil || d.VolumeID != "e" {
			t.Fatalf("got %v %v", d, err)
		}
	}
}

func TestNoEligibleVolumeIsAnError(t *testing.T) {
	p, _ := New(Balanced)
	c := vols()
	for i := range c {
		c[i].Writable = false
	}
	if _, err := p.Choose("cam1", GB, c); !errors.Is(err, ErrNoEligibleVolume) {
		t.Fatalf("err=%v", err)
	}
}

func TestPriorityTiersOverflow(t *testing.T) {
	p, _ := New(Balanced)
	c := vols()
	c[2].Priority = 1 // f only used when d and e cannot take data
	for i := 0; i < 20; i++ {
		if d, _ := p.Choose("cam1", GB, c); d.VolumeID == "f" {
			t.Fatal("lower tier used while higher tier available")
		}
	}
	c[0].Writable, c[1].Writable = false, false
	if d, _ := p.Choose("cam1", GB, c); d.VolumeID != "f" {
		t.Fatalf("overflow to tier 1 failed: %v", d)
	}
}

func TestCameraAffinityStickyAndFallback(t *testing.T) {
	p, _ := New(CameraAffinity)
	c := vols()
	c[1].Cameras = []string{"cam1"}
	c[2].Cameras = []string{"cam1"}
	first, _ := p.Choose("cam1", GB, c)
	for i := 0; i < 10; i++ {
		d, _ := p.Choose("cam1", GB, c)
		if d.VolumeID != first.VolumeID {
			t.Fatalf("affinity not sticky: %s then %s", first.VolumeID, d.VolumeID)
		}
	}
	// Sticky volume fails: move to the other assigned one, not to d.
	for i := range c {
		if c[i].ID == first.VolumeID {
			c[i].Writable = false
		}
	}
	d, _ := p.Choose("cam1", GB, c)
	if d.VolumeID == first.VolumeID || d.VolumeID == "d" || d.Fallback {
		t.Fatalf("got %+v", d)
	}
	// All assigned fail: fall back to any healthy volume, flagged.
	c[1].Writable, c[2].Writable = false, false
	d, err := p.Choose("cam1", GB, c)
	if err != nil || d.VolumeID != "d" || !d.Fallback {
		t.Fatalf("got %+v %v", d, err)
	}
}

func TestCameraAffinityUnassignedIsStickyAndPersisted(t *testing.T) {
	p, _ := New(CameraAffinity)
	a, _ := p.Choose("cam9", GB, vols())
	snap := p.Snapshot()
	q, _ := New(CameraAffinity)
	q.Restore(snap)
	b, _ := q.Choose("cam9", GB, vols())
	if a.VolumeID != b.VolumeID {
		t.Fatalf("restart lost affinity: %s vs %s", a.VolumeID, b.VolumeID)
	}
}

func TestManualNeverFallsBack(t *testing.T) {
	p, _ := New(Manual)
	c := vols()
	c[0].Cameras = []string{"cam1"}
	if d, err := p.Choose("cam1", GB, c); err != nil || d.VolumeID != "d" {
		t.Fatalf("%+v %v", d, err)
	}
	c[0].Writable = false
	if _, err := p.Choose("cam1", GB, c); !errors.Is(err, ErrNoEligibleVolume) {
		t.Fatalf("manual must not fall back, err=%v", err)
	}
	if _, err := p.Choose("cam2", GB, c); !errors.Is(err, ErrNoEligibleVolume) {
		t.Fatalf("unassigned camera, err=%v", err)
	}
}

func TestUnknownPolicy(t *testing.T) {
	if _, err := New("raid0"); err == nil {
		t.Fatal("expected error")
	}
}
