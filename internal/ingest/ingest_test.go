package ingest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/platform"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeProber struct{}

func (fakeProber) Usage(string) (platform.DiskUsage, error) {
	return platform.DiskUsage{TotalBytes: 1000e9, FreeBytes: 1000e9}, nil
}
func (fakeProber) Volume(p string) (platform.VolumeInfo, error) {
	return platform.VolumeInfo{StableID: "uuid:" + filepath.Base(filepath.Dir(p)), FSType: "ext4"}, nil
}

type env struct {
	store *segments.Store
	mgr   *storage.Manager
	roots map[string]string
}

func newEnv(t *testing.T, ids ...string) env {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default()
	cfg.Storage.MinFreeGB = 0
	roots := map[string]string{}
	for _, id := range ids {
		root := filepath.Join(base, id, "Grabaciones ñ")
		os.MkdirAll(root, 0o755)
		roots[id] = root
		cfg.Storage.Volumes = append(cfg.Storage.Volumes, config.Volume{ID: id, Path: root, PathWindows: root})
	}
	d, err := db.Open(context.Background(), filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store := segments.New(d)
	mgr, err := storage.NewManager(cfg, "linux", fakeProber{}, store, quiet)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range mgr.Specs() {
		if _, err := volumes.Initialize(fakeProber{}, sp, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	mgr.Refresh(context.Background())
	return env{store: store, mgr: mgr, roots: roots}
}

func needFFmpeg(t *testing.T) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return ff
}

// syntheticCamera emulates a camera: real-time H.264 + AAC MPEG-TS for d.
func syntheticCamera(d time.Duration) func(string) []string {
	return func(string) []string {
		return []string{"-hide_banner", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
			"-f", "lavfi", "-i", "sine=frequency=440",
			"-t", d.String(), "-c:v", "libx264", "-g", "12", "-pix_fmt", "yuv420p", "-c:a", "aac",
			"-f", "mpegts", "pipe:1"}
	}
}

func runFor(t *testing.T, r *Recorder, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.Run(ctx) }()
	wg.Wait()
}

func TestRecordAcrossVolumes(t *testing.T) {
	ff := needFFmpeg(t)
	e := newEnv(t, "vol-a", "vol-b")
	sink := &Sink{CameraID: "cam1", Representation: "main", Volumes: e.mgr, Index: e.store, Log: quiet}
	r := &Recorder{FFmpeg: ff, Segment: 500 * time.Millisecond, Sink: sink, Log: quiet,
		Args: syntheticCamera(4 * time.Second), MinBackoff: time.Hour, MaxBackoff: time.Hour}
	runFor(t, r, 6*time.Second)

	ctx := context.Background()
	segs, err := e.store.Range(ctx, "cam1", "main", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 4 {
		t.Fatalf("only %d segments", len(segs))
	}
	perVol := map[string]int{}
	for i, s := range segs {
		if s.Status != segments.StatusFinal || s.Checksum == "" || s.SizeBytes == 0 {
			t.Fatalf("segment %d: %+v", i, s)
		}
		if !strings.HasPrefix(s.Codec, "h264+aac") {
			t.Fatalf("codec %q", s.Codec)
		}
		p := filepath.Join(e.roots[s.VolumeID], filepath.FromSlash(s.RelativePath))
		sum, err := fileSHA256(p)
		if err != nil || sum != s.Checksum {
			t.Fatalf("segment %d file %s: %v checksum match=%v", i, p, err, sum == s.Checksum)
		}
		if i > 0 && !s.Start.After(segs[i-1].Start) {
			t.Fatal("timeline not chronological")
		}
		perVol[s.VolumeID]++
	}
	if perVol["vol-a"] == 0 || perVol["vol-b"] == 0 {
		t.Fatalf("balanced placement not spread: %v", perVol)
	}
	for _, root := range e.roots {
		tmp, _ := os.ReadDir(filepath.Join(root, TmpDir))
		if len(tmp) != 0 {
			t.Fatalf("temp files left: %v", tmp)
		}
	}
	m := r.Metrics.Snapshot()
	if m.Segments != int64(len(segs)) || m.BitrateKbps <= 0 || m.State != StateStopped || m.Reconnects != 1 {
		t.Fatalf("metrics %+v", m)
	}
	// Stream ended after 4 s: an explicit gap must exist.
	gaps, _ := e.store.Gaps(ctx, "cam1", "main", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if len(gaps) == 0 || gaps[0].Reason != "stream_lost" {
		t.Fatalf("gaps %+v", gaps)
	}
}

func TestVolumeFailureSwitchesToHealthyVolume(t *testing.T) {
	// ST-04: a volume disappears while recording; new segments go elsewhere
	// and the other volume's index is untouched.
	ff := needFFmpeg(t)
	e := newEnv(t, "vol-a", "vol-b")
	// Replace vol-a's root with a plain file after inspection said it was healthy.
	os.RemoveAll(e.roots["vol-a"])
	os.WriteFile(e.roots["vol-a"], []byte("not a dir"), 0o644)

	sink := &Sink{CameraID: "cam1", Representation: "main", Volumes: e.mgr, Index: e.store, Log: quiet}
	r := &Recorder{FFmpeg: ff, Segment: 500 * time.Millisecond, Sink: sink, Log: quiet,
		Args: syntheticCamera(3 * time.Second), MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	runFor(t, r, 5*time.Second)

	segs, _ := e.store.Range(context.Background(), "cam1", "main", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if len(segs) < 2 {
		t.Fatalf("only %d segments", len(segs))
	}
	for _, s := range segs {
		if s.VolumeID != "vol-b" {
			t.Fatalf("segment written to failed volume: %+v", s)
		}
	}
	for _, st := range e.mgr.Statuses() {
		if st.Spec.ID == "vol-a" && st.State != volumes.StateDegraded {
			t.Fatalf("vol-a state %s", st.State)
		}
	}
}

func TestNoVolumeRecordsGapInsteadOfWriting(t *testing.T) {
	ff := needFFmpeg(t)
	e := newEnv(t, "vol-a")
	e.mgr.MarkFailed("vol-a", os.ErrPermission)
	sink := &Sink{CameraID: "cam1", Representation: "main", Volumes: e.mgr, Index: e.store, Log: quiet}
	r := &Recorder{FFmpeg: ff, Segment: 500 * time.Millisecond, Sink: sink, Log: quiet,
		Args: syntheticCamera(2 * time.Second), MinBackoff: time.Hour, MaxBackoff: time.Hour}
	runFor(t, r, 3*time.Second)
	ctx := context.Background()
	segs, _ := e.store.Range(ctx, "cam1", "main", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	gaps, _ := e.store.Gaps(ctx, "cam1", "main", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if len(segs) != 0 || len(gaps) == 0 || gaps[0].Reason != "no_volume" {
		t.Fatalf("segs=%d gaps=%+v", len(segs), gaps)
	}
}

func TestReconcileAfterCrash(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "vol-a")
	root := e.roots["vol-a"]
	begin := func(i int) segments.Segment {
		start := time.Date(2026, 11, 12, 10, i, 0, 0, time.UTC)
		s := segments.Segment{CameraID: "cam1", Start: start, VolumeID: "vol-a", RelativePath: RelativePath("cam1", "main", start)}
		id, err := e.store.Begin(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		s.ID = id
		return s
	}
	os.MkdirAll(filepath.Join(root, TmpDir), 0o755)
	// 1: power cut mid-write → temp with 10.5 packets.
	a := begin(0)
	os.WriteFile(TempPath(root, a.ID), make([]byte, 188*10+94), 0o644)
	// 2: crash after rename, before index update.
	b := begin(1)
	final := filepath.Join(root, filepath.FromSlash(b.RelativePath))
	os.MkdirAll(filepath.Dir(final), 0o755)
	os.WriteFile(final, make([]byte, 188*3), 0o644)
	// 3: crash right after Begin, nothing on disk.
	begin(2)
	// 4: interrupted deletion.
	d := begin(3)
	df := filepath.Join(root, filepath.FromSlash(d.RelativePath))
	os.MkdirAll(filepath.Dir(df), 0o755)
	os.WriteFile(df, make([]byte, 188), 0o644)
	e.store.Finalize(ctx, d.ID, d.Start.Add(time.Minute), 188, "x", "")
	if _, err := e.store.BeginDelete(ctx, d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(ctx, e.store, "vol-a", root, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recovered != 2 || res.Corrupt != 1 || res.Deletes != 1 {
		t.Fatalf("%+v", res)
	}
	segs, _ := e.store.Range(ctx, "cam1", "main", a.Start.Add(-time.Hour), a.Start.Add(time.Hour))
	if len(segs) != 2 || segs[0].SizeBytes != 188*10 || segs[1].SizeBytes != 188*3 {
		t.Fatalf("%+v", segs)
	}
	if exists(df) || exists(TempPath(root, a.ID)) {
		t.Fatal("leftover files")
	}
	if w, _ := e.store.Writing(ctx, "vol-a"); len(w) != 0 {
		t.Fatal("rows still writing")
	}
}

func TestRedactAndArgs(t *testing.T) {
	src := Source{URL: "rtsp://admin:S3cr3t!@10.0.0.5:554/Streaming/101?token=abc"}
	if got := Redact(src.URL); strings.Contains(got, "S3cr3t") || strings.Contains(got, "token") || strings.Contains(got, "admin") {
		t.Fatalf("leak: %s", got)
	}
	r := &Recorder{Source: src}
	if got := r.redact("Error opening rtsp://admin:S3cr3t!@10.0.0.5:554/x: 401 S3cr3t!"); strings.Contains(got, "S3cr3t") {
		t.Fatalf("leak: %s", got)
	}
	args := strings.Join(FFmpegArgs(src, AudioCopy), " ")
	for _, want := range []string{"-rtsp_transport tcp", "-c:v copy", "-c:a copy", "-f mpegts", "pipe:1"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q: %s", want, args)
		}
	}
	if a := strings.Join(FFmpegArgs(src, AudioAAC), " "); !strings.Contains(a, "-c:a aac") || !strings.Contains(a, "-c:v copy") {
		t.Fatal(a)
	}
	for i := 0; i < 20; i++ {
		if d := backoff(i, time.Second, time.Minute); d < 500*time.Millisecond || d > 90*time.Second {
			t.Fatalf("backoff(%d)=%s", i, d)
		}
	}
}
