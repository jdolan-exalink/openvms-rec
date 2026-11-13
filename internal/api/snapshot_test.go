package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

func TestCameraSnapshot(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	base := t.TempDir()
	root := filepath.Join(base, "vol")
	os.MkdirAll(root, 0o755)
	if _, err := volumes.Initialize(fakeProber{}, volumes.Spec{ID: "v", Root: root}, time.Now()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "cam"), 0o755)
	// 6 s of 1440p HEVC, keyframe every second, like a real camera.
	seg := filepath.Join(root, "cam", "a.ts")
	out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=2560x1440:rate=15",
		"-t", "6", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=15:log-level=error", "-f", "mpegts", seg).CombinedOutput()
	if err != nil {
		t.Skipf("cannot encode HEVC here: %v %s", err, out)
	}
	// Simulate the segment still being written: cut it mid-packet.
	b, _ := os.ReadFile(seg)
	os.WriteFile(seg, b[:len(b)*2/3+57], 0o644)

	cfg := config.Default()
	cfg.Storage.Volumes = []config.Volume{{ID: "v", Path: root, PathWindows: root}}
	cfg.Cameras = []config.Camera{{ID: "cam", MainURL: "rtsp://10.0.0.1/a"}}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := &Server{Cfg: cfg, DB: d, Prober: fakeProber{}, FFmpeg: ff, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if c := getJSON(t, "GET", ts.URL+"/api/v1/cameras/cam/snapshot", nil); c != 404 {
		t.Fatalf("no footage %d", c)
	}
	if c := getJSON(t, "GET", ts.URL+"/api/v1/cameras/nope/snapshot", nil); c != 404 {
		t.Fatalf("unknown camera %d", c)
	}
	id, err := segments.New(d).Begin(ctx, segments.Segment{CameraID: "cam", VolumeID: "v", RelativePath: "cam/a.ts", Start: time.Now(), Codec: "h265"})
	if err != nil {
		t.Fatal(err)
	}
	// A segment being written lives in the volume's temp dir until it closes.
	tmp := ingest.TempPath(root, id)
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	if err := os.Rename(seg, tmp); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(ts.URL + "/api/v1/cameras/cam/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/jpeg" || !bytes.HasPrefix(img, []byte{0xff, 0xd8}) {
		t.Fatalf("%d %s %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(img))
	}
	if len(img) > 200<<10 {
		t.Fatalf("snapshot not scaled down: %d bytes", len(img))
	}
}
