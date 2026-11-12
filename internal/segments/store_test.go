package segments

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/retention"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

// write creates a finalized 1 GB (logical) segment with a real tiny file.
func write(t *testing.T, s *Store, root, vol, cam string, i int) int64 {
	t.Helper()
	ctx := context.Background()
	rel := fmt.Sprintf("%s/main/2026/11/01/%04d.ts", cam, i)
	if root != "" {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("ts"), 0o644)
	}
	start := t0.Add(time.Duration(i) * time.Minute)
	id, err := s.Begin(ctx, Segment{CameraID: cam, Start: start, VolumeID: vol, RelativePath: rel, Codec: "h264"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finalize(ctx, id, start.Add(time.Minute), volumes.GB, "abc", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRangeAcrossVolumesIsChronological(t *testing.T) {
	// ST-07: a period split across D:, E:, F: comes back in order.
	s := newStore(t)
	vols := []string{"d", "e", "f"}
	for i := 0; i < 9; i++ {
		write(t, s, "", vols[i%3], "cam1", i)
	}
	got, err := s.Range(context.Background(), "cam1", "main", t0.Add(2*time.Minute+30*time.Second), t0.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d segments", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].Start.After(got[i-1].Start) {
			t.Fatal("not chronological")
		}
	}
	if got[0].VolumeID != "f" || got[1].VolumeID != "d" {
		t.Fatalf("volumes %s %s", got[0].VolumeID, got[1].VolumeID)
	}
}

func TestRecorderBytesPerVolume(t *testing.T) {
	s := newStore(t)
	write(t, s, "", "d", "c", 0)
	write(t, s, "", "d", "c", 1)
	write(t, s, "", "e", "c", 2)
	ctx := context.Background()
	if n, _ := s.RecorderBytes(ctx, "d"); n != 2*volumes.GB {
		t.Fatalf("d=%d", n)
	}
	if n, _ := s.RecorderBytes(ctx, "e"); n != volumes.GB {
		t.Fatalf("e=%d", n)
	}
}

func TestRetentionEndToEnd(t *testing.T) {
	// Plan on real index data, execute against real files, verify protection
	// and holds win even when added after planning.
	ctx := context.Background()
	s := newStore(t)
	root := t.TempDir()
	var segIDs []int64
	for i := 0; i < 10; i++ {
		segIDs = append(segIDs, write(t, s, root, "d", "cam1", i))
	}
	// Seg 0 is evidence from the start.
	if err := s.Protect(ctx, segIDs[0], t0.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Hour)
	limits := volumes.Compute(volumes.Policy{MaxPercent: 100, DefaultMaxPercent: 90, CleanupStartPercent: 87, CleanupTargetPercent: 84},
		volumes.Usage{CapacityBytes: 10 * volumes.GB, FreeBytes: 10 * volumes.GB})
	used, _ := s.RecorderBytes(ctx, "d")
	cands, err := s.RetentionCandidates(ctx, "d", now)
	if err != nil {
		t.Fatal(err)
	}
	plan := retention.PlanVolume(retention.Input{
		Now: now, Limits: limits, RecorderBytes: used, Segments: cands,
		Policy: func(string, string) retention.CameraPolicy { return retention.CameraPolicy{} },
	})
	// 10 GB used, target 9.33 GB → one deletion: seg 1 (seg 0 protected).
	if len(plan.Deletions) != 1 || plan.Deletions[0].SegmentID != segIDs[1] {
		t.Fatalf("plan=%+v", plan.Deletions)
	}
	// Race: an export grabs seg 1 between planning and execution.
	s.Hold(ctx, segIDs[1], "export:x", now.Add(time.Hour))
	res := retention.Execute(ctx, plan, root, s, now)
	if res.Deleted != 0 || res.Skipped != 1 {
		t.Fatalf("res=%+v", res)
	}
	s.Release(ctx, segIDs[1], "export:x")
	res = retention.Execute(ctx, plan, root, s, now)
	if res.Deleted != 1 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "cam1/main/2026/11/01/0001.ts")); !os.IsNotExist(err) {
		t.Fatal("file not removed")
	}
	if _, err := os.Stat(filepath.Join(root, "cam1/main/2026/11/01/0000.ts")); err != nil {
		t.Fatal("protected file removed")
	}
	if n, _ := s.RecorderBytes(ctx, "d"); n != 9*volumes.GB {
		t.Fatalf("bytes after=%d", n)
	}
}

func TestDeleteToleratesMissingFileAndKeepsOtherVolumes(t *testing.T) {
	// A missing volume's files are gone, but deleting there must not touch
	// other volumes' index rows.
	ctx := context.Background()
	s := newStore(t)
	a := write(t, s, "", "e", "cam1", 0) // no file on disk
	write(t, s, "", "d", "cam1", 1)
	plan := retention.Plan{Deletions: []retention.Deletion{{SegmentID: a, SizeBytes: volumes.GB}}}
	res := retention.Execute(ctx, plan, t.TempDir(), s, t0.Add(time.Hour))
	if res.Deleted != 1 {
		t.Fatalf("%+v", res)
	}
	if n, _ := s.RecorderBytes(ctx, "d"); n != volumes.GB {
		t.Fatal("other volume affected")
	}
}

func TestProtectNeverShortens(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id := write(t, s, "", "d", "c", 0)
	s.Protect(ctx, id, t0.Add(48*time.Hour))
	s.Protect(ctx, id, t0.Add(1*time.Hour))
	c, _ := s.RetentionCandidates(ctx, "d", t0)
	if !c[0].ProtectedUntil.Equal(t0.Add(48 * time.Hour)) {
		t.Fatalf("protection shortened to %v", c[0].ProtectedUntil)
	}
}
