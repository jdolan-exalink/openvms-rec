package volumes

import (
	"math"
	"testing"
)

const TB = 1000 * GB

func defaultPolicy() Policy {
	return Policy{
		MaxPercent:           90,
		MinFreeBytes:         100 * GB,
		DefaultMaxPercent:    90,
		CleanupStartPercent:  87,
		CleanupTargetPercent: 84,
	}
}

func TestComputeDedicatedVolumeUsesNinetyPercent(t *testing.T) {
	// 4 TB dedicated disk, empty: 90 % (3.6 TB) is stricter than capacity-reserve (3.9 TB).
	u := Usage{CapacityBytes: 4 * TB, FreeBytes: 4 * TB}
	l := Compute(defaultPolicy(), u)
	if l.Effective != 3600*GB || l.Binding != ConstraintPercent {
		t.Fatalf("effective=%d binding=%s", l.Effective, l.Binding)
	}
	if l.CleanupStart != 3480*GB {
		t.Errorf("cleanup start = %d, want 87%% of capacity", l.CleanupStart)
	}
	if l.CleanupTarget != 3360*GB {
		t.Errorf("cleanup target = %d, want 84%% of capacity", l.CleanupTarget)
	}
	if l.Headroom != l.Effective {
		t.Errorf("headroom = %d", l.Headroom)
	}
}

func TestComputeSmallVolumeReserveWins(t *testing.T) {
	// 500 GB disk: 90 % = 450 GB, but capacity - 100 GB reserve = 400 GB.
	u := Usage{CapacityBytes: 500 * GB, FreeBytes: 500 * GB}
	l := Compute(defaultPolicy(), u)
	if l.Effective != 400*GB || l.Binding != ConstraintReserve {
		t.Fatalf("effective=%d binding=%s", l.Effective, l.Binding)
	}
}

func TestComputeSharedVolumeOtherDataShrinksLimit(t *testing.T) {
	// 2 TB shared disk: 1.2 TB of foreign data, 200 GB of ours, 600 GB free.
	// Available = 200 + 600 - 100 = 700 GB < 90 % (1.8 TB).
	u := Usage{CapacityBytes: 2 * TB, FreeBytes: 600 * GB, RecorderBytes: 200 * GB}
	l := Compute(defaultPolicy(), u)
	if l.Effective != 700*GB || l.Binding != ConstraintAvailable {
		t.Fatalf("effective=%d binding=%s", l.Effective, l.Binding)
	}
	if l.Headroom != 500*GB {
		t.Errorf("headroom=%d", l.Headroom)
	}
	// Thresholds scale with the reduced limit.
	eff := float64(l.Effective)
	if got, want := l.CleanupStart, uint64(eff*87/90); got != want {
		t.Errorf("start=%d want %d", got, want)
	}
}

func TestComputeForeignDataAboveReserveBlocksWrites(t *testing.T) {
	// Other software filled the disk below the reserve: limit collapses to what
	// we already hold minus the deficit, so ingest stops and cleanup runs.
	u := Usage{CapacityBytes: 1 * TB, FreeBytes: 50 * GB, RecorderBytes: 300 * GB}
	l := Compute(defaultPolicy(), u)
	if l.Effective != 250*GB {
		t.Fatalf("effective=%d", l.Effective)
	}
	if !l.Full(u) || !l.NeedsCleanup(u) {
		t.Fatal("expected full and needing cleanup")
	}
	if l.Headroom != 0 {
		t.Errorf("headroom=%d", l.Headroom)
	}
}

func TestComputePerVolumePercent(t *testing.T) {
	// ST-03: D: at 90 %, E: at 80 % must yield different limits.
	p := defaultPolicy()
	p.MinFreeBytes = 0
	u := Usage{CapacityBytes: 1 * TB, FreeBytes: 1 * TB}
	d := Compute(p, u)
	p.MaxPercent = 80
	e := Compute(p, u)
	if d.Effective != 900*GB || e.Effective != 800*GB {
		t.Fatalf("d=%d e=%d", d.Effective, e.Effective)
	}
	if !(e.CleanupTarget < e.CleanupStart && e.CleanupStart < e.Effective) {
		t.Fatalf("thresholds out of order: %+v", e)
	}
}

func TestComputeAbsoluteAndFolderQuota(t *testing.T) {
	p := defaultPolicy()
	p.QuotaBytes = 1 * TB
	u := Usage{CapacityBytes: 10 * TB, FreeBytes: 10 * TB}
	if l := Compute(p, u); l.Effective != 1*TB || l.Binding != ConstraintAbsolute {
		t.Fatalf("%+v", l)
	}
	p.FolderQuotaBytes = 500 * GB
	if l := Compute(p, u); l.Effective != 500*GB || l.Binding != ConstraintFolderQuota {
		t.Fatalf("%+v", l)
	}
}

func TestComputeZeroCapacity(t *testing.T) {
	l := Compute(defaultPolicy(), Usage{})
	if l.Effective != 0 || l.Headroom != 0 || !l.Full(Usage{}) {
		t.Fatalf("%+v", l)
	}
}

func TestHysteresis(t *testing.T) {
	p := defaultPolicy()
	p.MinFreeBytes = 0
	capB := uint64(1 * TB)
	cases := []struct {
		used    uint64
		cleanup bool
		free    uint64
	}{
		{860 * GB, false, 0},
		{870 * GB, true, 30 * GB},
		{895 * GB, true, 55 * GB},
	}
	for _, c := range cases {
		u := Usage{CapacityBytes: capB, FreeBytes: capB - c.used, RecorderBytes: c.used}
		l := Compute(p, u)
		if l.NeedsCleanup(u) != c.cleanup || l.BytesToFree(u) != c.free {
			t.Errorf("used=%dGB cleanup=%v free=%dGB", c.used/GB, l.NeedsCleanup(u), l.BytesToFree(u)/GB)
		}
	}
}

func TestCapacityFormulas(t *testing.T) {
	// PRD §7 worked examples.
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.05 }
	if !near(GBPerDay(2), 21.6) {
		t.Errorf("2 Mbps = %.2f GB/day", GBPerDay(2))
	}
	if !near(3*GBPerDay(2), 64.8) {
		t.Errorf("3 cams")
	}
	if !near(16*GBPerDay(2), 345.6) {
		t.Errorf("16 cams")
	}
	if d := RetentionDays(256*GB, 2); !near(d, 11.85) {
		t.Errorf("256 GB at 2 Mbps = %.2f days, want ~11.9", d)
	}
	if !math.IsInf(RetentionDays(GB, 0), 1) {
		t.Error("zero bitrate should be infinite")
	}
}
