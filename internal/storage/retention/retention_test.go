package retention

import (
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

const GB = volumes.GB

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// seg builds a 1 GB, 1-hour segment that ended ageH hours ago.
func seg(id int64, cam string, ageH int) Segment {
	end := now.Add(-time.Duration(ageH) * time.Hour)
	return Segment{ID: id, CameraID: cam, Representation: "main", Start: end.Add(-time.Hour), End: end, SizeBytes: GB}
}

func flatPolicy(maxDays, minDays int) PolicyFunc {
	return func(string, string) CameraPolicy {
		return CameraPolicy{MaxAge: time.Duration(maxDays) * 24 * time.Hour, MinAge: time.Duration(minDays) * 24 * time.Hour}
	}
}

// limits100 is a volume whose effective limit is 100 GB (start 96.67, target 93.33).
func limits100() volumes.Limits {
	return volumes.Compute(volumes.Policy{
		MaxPercent: 100, DefaultMaxPercent: 90, CleanupStartPercent: 87, CleanupTargetPercent: 84,
	}, volumes.Usage{CapacityBytes: 100 * GB, FreeBytes: 100 * GB})
}

func ids(p Plan) []int64 {
	var out []int64
	for _, d := range p.Deletions {
		out = append(out, d.SegmentID)
	}
	return out
}

func TestNoCleanupBelowHighMark(t *testing.T) {
	segs := []Segment{seg(1, "c", 10), seg(2, "c", 5)}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 50 * GB, Segments: segs, Policy: flatPolicy(0, 0)})
	if len(p.Deletions) != 0 || p.StopWrites {
		t.Fatalf("%+v", p)
	}
}

func TestAgeDeletionRespectsProtection(t *testing.T) {
	old := seg(1, "c", 24*10)
	pinned := seg(2, "c", 24*10)
	pinned.ProtectedUntil = now.Add(24 * time.Hour)
	held := seg(3, "c", 24*10)
	held.Held = true
	fresh := seg(4, "c", 1)
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 4 * GB,
		Segments: []Segment{old, pinned, held, fresh}, Policy: flatPolicy(7, 1)})
	if got := ids(p); len(got) != 1 || got[0] != 1 || p.Deletions[0].Reason != ReasonAge {
		t.Fatalf("deletions=%v", p.Deletions)
	}
}

func TestSpaceCleanupOldestFirstToLowMark(t *testing.T) {
	// 97 GB used, oldest segment first; must reach <= 93.33 GB → delete 4.
	var segs []Segment
	for i := 0; i < 97; i++ {
		segs = append(segs, seg(int64(i+1), "c", 200-i))
	}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 97 * GB, Segments: segs, Policy: flatPolicy(0, 0)})
	got := ids(p)
	if len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("deleted %v", got)
	}
	if p.BytesAfter != 93*GB || p.StopWrites || len(p.Alerts) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestSpaceCleanupSkipsProtectedEvidence(t *testing.T) {
	// ST-06: the oldest segments are evidence; they survive, newer ones go.
	var segs []Segment
	for i := 0; i < 97; i++ {
		s := seg(int64(i+1), "c", 200-i)
		if i < 3 {
			s.ProtectedUntil = now.Add(30 * 24 * time.Hour)
		}
		segs = append(segs, s)
	}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 97 * GB, Segments: segs, Policy: flatPolicy(0, 0)})
	for _, id := range ids(p) {
		if id <= 3 {
			t.Fatalf("protected segment %d deleted", id)
		}
	}
	if len(p.Deletions) != 4 {
		t.Fatalf("deleted %v", ids(p))
	}
}

func TestMinimumRetentionPressureOverwrite(t *testing.T) {
	// Everything is inside a 30-day minimum; overwrite policy deletes the
	// low-priority camera first and alerts.
	var segs []Segment
	for i := 0; i < 48; i++ {
		segs = append(segs, seg(int64(i+1), "vip", 48-i))
		segs = append(segs, seg(int64(100+i), "lobby", 48-i))
	}
	pol := func(cam, _ string) CameraPolicy {
		pr := 1
		if cam == "vip" {
			pr = 10
		}
		return CameraPolicy{MinAge: 30 * 24 * time.Hour, Priority: pr}
	}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 97 * GB, Segments: segs, Policy: pol, PressurePolicy: OverwriteOldest})
	if len(p.Deletions) != 4 {
		t.Fatalf("deleted %v", ids(p))
	}
	for _, d := range p.Deletions {
		if d.CameraID != "lobby" || d.Reason != ReasonSpaceUnderMin {
			t.Fatalf("unexpected %+v", d)
		}
	}
	if p.Deletions[0].SegmentID != 100 {
		t.Fatalf("not oldest-first within camera: %v", ids(p))
	}
	if len(p.Alerts) != 1 || p.Alerts[0].Code != AlertMinRetentionViolated || p.Alerts[0].CameraID != "lobby" {
		t.Fatalf("alerts=%+v", p.Alerts)
	}
}

func TestMinimumRetentionPressureProtect(t *testing.T) {
	var segs []Segment
	for i := 0; i < 100; i++ {
		segs = append(segs, seg(int64(i+1), "c", 100-i))
	}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 100 * GB, Segments: segs, Policy: flatPolicy(0, 30), PressurePolicy: ProtectMinimum})
	if len(p.Deletions) != 0 {
		t.Fatalf("protect policy deleted %v", ids(p))
	}
	if !p.StopWrites || len(p.Alerts) != 1 || p.Alerts[0].Code != AlertRetentionPressure {
		t.Fatalf("%+v", p)
	}
}

func TestProtectedDataExceedsTarget(t *testing.T) {
	var segs []Segment
	for i := 0; i < 100; i++ {
		s := seg(int64(i+1), "c", 100-i)
		s.ProtectedUntil = now.Add(time.Hour)
		segs = append(segs, s)
	}
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 100 * GB, Segments: segs, Policy: flatPolicy(0, 0)})
	if len(p.Deletions) != 0 || !p.StopWrites {
		t.Fatalf("%+v", p)
	}
	if len(p.Alerts) != 1 || p.Alerts[0].Code != AlertProtectedExceeds {
		t.Fatalf("alerts=%+v", p.Alerts)
	}
}

func TestWritingSegmentsNeverDeleted(t *testing.T) {
	s := seg(1, "c", 24*30)
	s.Writing = true
	p := PlanVolume(Input{Now: now, Limits: limits100(), RecorderBytes: 99 * GB, Segments: []Segment{s}, Policy: flatPolicy(7, 0)})
	if len(p.Deletions) != 0 {
		t.Fatal("deleted a segment being written")
	}
}
