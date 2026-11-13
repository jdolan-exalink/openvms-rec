package exports

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest/tsseg"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// fileSink writes segments as real files and indexes them.
type fileSink struct {
	t     *testing.T
	store *segments.Store
	root  string
	clock *time.Time
}

type fileSeg struct {
	sink *fileSink
	id   int64
	f    *os.File
	n    int64
}

func (s *fileSink) Open(si tsseg.StartInfo) (tsseg.Writer, error) {
	rel := fmt.Sprintf("cam1/main/%d.ts", si.Start.UnixMilli())
	id, err := s.store.Begin(context.Background(), segments.Segment{CameraID: "cam1", Start: si.Start, Codec: si.VideoCodec, VolumeID: "v1", RelativePath: rel})
	if err != nil {
		return nil, err
	}
	os.MkdirAll(filepath.Join(s.root, "cam1/main"), 0o755)
	f, err := os.Create(filepath.Join(s.root, rel))
	return &fileSeg{sink: s, id: id, f: f}, err
}

func (w *fileSeg) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}
func (w *fileSeg) Abort(error) { w.f.Close() }
func (w *fileSeg) Close(e tsseg.EndInfo) error {
	w.f.Close()
	sum, _ := fileSHA256(w.f.Name())
	return w.sink.store.Finalize(context.Background(), w.id, e.End, uint64(w.n), sum, e.KeyframesJSON())
}

func setup(t *testing.T) (*Service, *segments.Store, string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	base := t.TempDir()
	in := filepath.Join(base, "in.ts")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-t", "12", "-c:v", "libx264", "-g", "25", "-pix_fmt", "yuv420p", "-f", "mpegts", "-y", in).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg: %v %s", err, b)
	}
	d, err := db.Open(context.Background(), filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store := segments.New(d)
	root := filepath.Join(base, "vol")
	// Fake clock: 12 s of media ≈ 12 s of wall time, 4 segments of ~3 s.
	data, _ := os.ReadFile(in)
	perPkt := 12 * time.Second / time.Duration(len(data)/tsseg.PacketSize)
	clk := t0
	seg := &tsseg.Segmenter{Target: 3 * time.Second, Now: func() time.Time { clk = clk.Add(perPkt); return clk }}
	f, _ := os.Open(in)
	defer f.Close()
	if err := seg.Run(context.Background(), f, &fileSink{t: t, store: store, root: root}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: d, Store: store, Dir: filepath.Join(base, "exports"), Version: "test",
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Root: func(id string) (string, bool) { return root, id == "v1" }}
	return svc, store, root
}

func wait(t *testing.T, svc *Service, id string) Export {
	t.Helper()
	for i := 0; i < 200; i++ {
		e, err := svc.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if e.State != StateQueued && e.State != StateRunning {
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("export did not finish")
	return Export{}
}

func TestExportProducesVerifiedArchive(t *testing.T) {
	svc, store, _ := setup(t)
	ctx := context.Background()
	req := Request{CameraID: "cam1", From: t0.Add(4500 * time.Millisecond), To: t0.Add(9 * time.Second)}
	e, err := svc.Create(ctx, req, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.Create(ctx, req, "key-1"); again.ID != e.ID {
		t.Fatal("idempotency key ignored")
	}
	e = wait(t, svc, e.ID)
	if e.State != StateSucceeded {
		t.Fatalf("%+v", e)
	}
	if e.ActualFrom.After(req.From) || req.From.Sub(e.ActualFrom) > 1100*time.Millisecond {
		t.Fatalf("start not snapped to the previous keyframe: %s vs %s", e.ActualFrom, req.From)
	}
	p, _, err := svc.Archive(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var man Manifest
	tmp := t.TempDir()
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		os.WriteFile(filepath.Join(tmp, f.Name), b, 0o644)
		if f.Name == "manifest.json" {
			json.Unmarshal(b, &man)
		}
	}
	if sum, _ := fileSHA256(filepath.Join(tmp, "video.mp4")); sum != man.Video.SHA256 || sum == "" {
		t.Fatal("manifest checksum mismatch")
	}
	if len(man.SourceSegments) < 2 || !man.SourceSegments[0].Verified {
		t.Fatalf("sources %+v", man.SourceSegments)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", filepath.Join(tmp, "video.mp4")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var dur float64
	fmt.Sscan(string(out), &dur)
	if dur < 4 || dur > 6.5 {
		t.Fatalf("duration %.2f for a 4.5 s request", dur)
	}
	// Holds are released after the export.
	if c, _ := store.RetentionCandidates(ctx, "v1", time.Now()); c[0].Held {
		t.Fatal("hold not released")
	}
}

func TestExportDetectsTampering(t *testing.T) {
	svc, store, root := setup(t)
	ctx := context.Background()
	segs, _ := store.Range(ctx, "cam1", "main", t0, t0.Add(time.Minute))
	f, _ := os.OpenFile(filepath.Join(root, segs[1].RelativePath), os.O_WRONLY, 0)
	f.WriteAt([]byte{0x47, 0x1f, 0xff, 0x10, 0xde, 0xad}, 188*5)
	f.Close()
	e, _ := svc.Create(ctx, Request{CameraID: "cam1", From: t0, To: t0.Add(12 * time.Second)}, "")
	e = wait(t, svc, e.ID)
	if e.State != StateFailed || e.Error == "" {
		t.Fatalf("%+v", e)
	}
	if _, err := os.Stat(filepath.Join(svc.Dir, e.ID)); !os.IsNotExist(err) {
		t.Fatal("partial export left behind")
	}
}

func TestExportValidation(t *testing.T) {
	svc := &Service{}
	if _, err := svc.Create(context.Background(), Request{CameraID: "c", From: t0, To: t0}, ""); err == nil {
		t.Fatal("empty range accepted")
	}
	if _, err := svc.Create(context.Background(), Request{CameraID: "c", From: t0, To: t0.Add(48 * time.Hour)}, ""); err == nil {
		t.Fatal("huge range accepted")
	}
}
