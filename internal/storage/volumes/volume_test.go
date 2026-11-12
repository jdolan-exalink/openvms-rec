package volumes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/platform"
)

// fakeProber maps root paths to canned disk facts.
type fakeProber struct {
	vols  map[string]platform.VolumeInfo
	usage map[string]platform.DiskUsage
}

func (f fakeProber) Usage(p string) (platform.DiskUsage, error) {
	u, ok := f.usage[p]
	if !ok {
		return platform.DiskUsage{}, errors.New("no usage")
	}
	return u, nil
}

func (f fakeProber) Volume(p string) (platform.VolumeInfo, error) {
	v, ok := f.vols[p]
	if !ok {
		return platform.VolumeInfo{}, errors.New("no volume")
	}
	return v, nil
}

func newEnv(t *testing.T, names ...string) (map[string]string, fakeProber) {
	t.Helper()
	base := t.TempDir()
	roots := map[string]string{}
	fp := fakeProber{vols: map[string]platform.VolumeInfo{}, usage: map[string]platform.DiskUsage{}}
	for _, n := range names {
		r := filepath.Join(base, n, "Recordings con espacio ñ") // spaces + Unicode
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
		roots[n] = r
		fp.vols[r] = platform.VolumeInfo{StableID: "uuid:" + n, FSType: "ext4", MountPoint: filepath.Join(base, n)}
		fp.usage[r] = platform.DiskUsage{TotalBytes: 1000 * GB, FreeBytes: 1000 * GB}
	}
	return roots, fp
}

func spec(id, root string) Spec {
	return Spec{ID: id, Root: root, Enabled: true, Weight: 1, Policy: defaultPolicy()}
}

func TestInspectRequiresInitialization(t *testing.T) {
	roots, fp := newEnv(t, "d")
	in := Inspector{Prober: fp}
	st := in.Inspect(spec("cctv-d", roots["d"]))
	if st.State != StateUnready {
		t.Fatalf("state=%s reason=%s", st.State, st.Reason)
	}
	if _, err := Initialize(fp, spec("cctv-d", roots["d"]), time.Now()); err != nil {
		t.Fatal(err)
	}
	st = in.Inspect(spec("cctv-d", roots["d"]))
	if st.State != StateHealthy || !st.Writable() {
		t.Fatalf("state=%s reason=%s", st.State, st.Reason)
	}
	if st.Limits.Effective != 900*GB {
		t.Fatalf("limit=%d", st.Limits.Effective)
	}
}

func TestInitializeRefusesForeignData(t *testing.T) {
	roots, fp := newEnv(t, "d")
	os.WriteFile(filepath.Join(roots["d"], "someone-elses.mp4"), []byte("x"), 0o644)
	if _, err := Initialize(fp, spec("cctv-d", roots["d"]), time.Now()); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("err=%v", err)
	}
}

func TestInspectMissingAndWrongDisk(t *testing.T) {
	roots, fp := newEnv(t, "d", "e")
	in := Inspector{Prober: fp}
	if st := in.Inspect(spec("gone", filepath.Join(roots["d"], "nope"))); st.State != StateMissing {
		t.Fatalf("state=%s", st.State)
	}
	// Disk of volume "cctv-e" is now mounted where "cctv-d" is expected.
	Initialize(fp, spec("cctv-e", roots["d"]), time.Now())
	if st := in.Inspect(spec("cctv-d", roots["d"])); st.State != StateMissing {
		t.Fatalf("state=%s reason=%s", st.State, st.Reason)
	}
}

func TestInspectDriveLetterChangeKeepsIdentity(t *testing.T) {
	// ST-05: the disk moves to another path; the marker identifies it.
	roots, fp := newEnv(t, "d")
	Initialize(fp, spec("cctv-d", roots["d"]), time.Now())
	moved := filepath.Join(filepath.Dir(roots["d"]), "moved")
	if err := os.Rename(roots["d"], moved); err != nil {
		t.Fatal(err)
	}
	fp.vols[moved] = fp.vols[roots["d"]]
	fp.usage[moved] = fp.usage[roots["d"]]
	st := Inspector{Prober: fp}.Inspect(spec("cctv-d", moved))
	if st.State != StateHealthy {
		t.Fatalf("state=%s reason=%s", st.State, st.Reason)
	}
}

func TestInspectAllDetectsSameVolumeTwice(t *testing.T) {
	// ST-02: two paths on one filesystem must not double count capacity.
	roots, fp := newEnv(t, "d")
	other := filepath.Join(roots["d"], "..", "second")
	os.MkdirAll(other, 0o755)
	fp.vols[other] = fp.vols[roots["d"]]
	fp.usage[other] = fp.usage[roots["d"]]
	Initialize(fp, spec("a", roots["d"]), time.Now())
	Initialize(fp, spec("b", other), time.Now())
	sts := Inspector{Prober: fp}.InspectAll([]Spec{spec("a", roots["d"]), spec("b", other)})
	if sts[0].State != StateHealthy || sts[1].State != StateDegraded {
		t.Fatalf("a=%s b=%s (%s)", sts[0].State, sts[1].State, sts[1].Reason)
	}
}

func TestInspectStates(t *testing.T) {
	roots, fp := newEnv(t, "d")
	Initialize(fp, spec("v", roots["d"]), time.Now())

	sp := spec("v", roots["d"])
	sp.Enabled = false
	if st := (Inspector{Prober: fp}).Inspect(sp); st.State != StateDisabled {
		t.Errorf("disabled: %s", st.State)
	}
	sp = spec("v", roots["d"])
	sp.Draining = true
	if st := (Inspector{Prober: fp}).Inspect(sp); st.State != StateDraining {
		t.Errorf("draining: %s", st.State)
	}
	in := Inspector{Prober: fp, RecorderBytes: func(string) uint64 { return 900 * GB }}
	u := fp.usage[roots["d"]]
	u.FreeBytes = 100 * GB
	fp.usage[roots["d"]] = u
	if st := in.Inspect(spec("v", roots["d"])); st.State != StateFull {
		t.Errorf("full: %s", st.State)
	}
	u.ReadOnly = true
	fp.usage[roots["d"]] = u
	if st := in.Inspect(spec("v", roots["d"])); st.State != StateDegraded {
		t.Errorf("read-only: %s", st.State)
	}
}

func TestInspectWarnings(t *testing.T) {
	roots, fp := newEnv(t, "d")
	Initialize(fp, spec("v", roots["d"]), time.Now())
	vi := fp.vols[roots["d"]]
	vi.IsSystem, vi.IsNetwork = true, true
	fp.vols[roots["d"]] = vi
	st := Inspector{Prober: fp}.Inspect(spec("v", roots["d"]))
	has := map[string]bool{}
	for _, w := range st.Warnings {
		has[w] = true
	}
	if !has[WarnSystemVolume] || !has[WarnNetworkVolume] || !has[WarnNoRedundancy] {
		t.Fatalf("warnings=%v", st.Warnings)
	}
}

func TestRealPlatformOnTempDir(t *testing.T) {
	// Smoke test against the real OS implementation.
	root := t.TempDir()
	sp := spec("real", root)
	if _, err := Initialize(OSProber{}, sp, time.Now()); err != nil {
		if errors.Is(err, platform.ErrUnsupported) {
			t.Skip("platform not supported")
		}
		t.Fatal(err)
	}
	st := Inspector{Prober: OSProber{}}.Inspect(sp)
	if st.Info.StableID == "" || st.Usage.CapacityBytes == 0 {
		t.Fatalf("info=%+v usage=%+v", st.Info, st.Usage)
	}
	t.Logf("state=%s fs=%s id=%s mount=%s cap=%dGB", st.State, st.Info.FSType, st.Info.StableID, st.Info.MountPoint, st.Usage.CapacityBytes/GB)
}
