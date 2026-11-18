package events

import (
	"bytes"
	"context"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func TestDetectFrameChange(t *testing.T) {
	a := make([]byte, motionFrameBytes)
	b := make([]byte, motionFrameBytes)
	if r := DetectFrameChange(a, b, motionPixelThresh); r != 0 {
		t.Fatalf("expected 0 change for identical frames, got %v", r)
	}
	// Change 10% of pixels above threshold
	for i := 0; i < motionFrameBytes/10; i++ {
		b[i] = 100
	}
	if r := DetectFrameChange(a, b, motionPixelThresh); r < 0.09 {
		t.Fatalf("expected ~0.10 change, got %v", r)
	}
}

func TestMotionDetectorRecordsEvent(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(tmp, "rec.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	reg, err := cameras.Open(ctx, cameras.Options{
		DB:        database,
		CredsFile: filepath.Join(tmp, "creds.json"),
	})
	if err != nil {
		t.Fatalf("cameras.Open: %v", err)
	}
	if _, err := reg.Create(ctx, cameras.Input{
		ID:        "cam-1",
		Name:      "Cam 1",
		MainURL:   "rtsp://192.0.2.10:554/stream1",
		NVRMotion: true,
	}); err != nil {
		t.Fatalf("reg.Create: %v", err)
	}

	store := NewStore(database)
	det := NewMotionDetector(store, reg, "", slog.Default())

	f1 := make([]byte, motionFrameBytes)
	f2 := make([]byte, motionFrameBytes)
	for i := 0; i < motionFrameBytes/5; i++ {
		f2[i] = 180
	}
	stream := append(f1, f2...)
	_ = det.processFrameStream(ctx, "cam-1", bytes.NewReader(stream))

	evs, _, err := store.List(ctx, Query{CameraID: "cam-1"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("expected 1 motion event, got %d err=%v", len(evs), err)
	}
	if evs[0].Type != "motion" || evs[0].Source != "nvr_motion" {
		t.Fatalf("unexpected event: %+v", evs[0])
	}
	caps, err := store.ListCapabilities(ctx, "cam-1")
	if err != nil || len(caps) != 1 || caps[0].Capability != "analytics.motion" {
		t.Fatalf("expected analytics.motion confirmed, got %+v err=%v", caps, err)
	}
}

func TestMotionDetectorColorStreamAnnotatesAmberBox(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(tmp, "rec.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	reg, err := cameras.Open(ctx, cameras.Options{
		DB:        database,
		CredsFile: filepath.Join(tmp, "creds.json"),
	})
	if err != nil {
		t.Fatalf("cameras.Open: %v", err)
	}
	if _, err := reg.Create(ctx, cameras.Input{
		ID:        "cam-1",
		Name:      "Cam 1",
		MainURL:   "rtsp://192.0.2.10:554/stream1",
		NVRMotion: true,
	}); err != nil {
		t.Fatalf("reg.Create: %v", err)
	}

	snapDir := filepath.Join(tmp, "snapshots")
	store := NewStore(database)
	snap := &Snapshotter{DB: database, Dir: snapDir}
	store.OnInsert = func(ev Event) {
		if _, err := snap.Capture(ctx, ev); err != nil {
			t.Errorf("snap.Capture: %v", err)
		}
	}
	det := NewMotionDetector(store, reg, "", slog.Default())

	// Frame 1: uniform dark gray; Frame 2: bright block in center-left [x: 160..320, y: 90..210].
	f1 := bytes.Repeat([]byte{25, 25, 25}, motionColorWidth*motionColorHeight)
	f2 := append([]byte(nil), f1...)
	for y := 90; y < 210; y++ {
		for x := 160; x < 320; x++ {
			off := (y*motionColorWidth + x) * 3
			f2[off], f2[off+1], f2[off+2] = 220, 220, 220
		}
	}
	_ = det.processColorFrameStream(ctx, "cam-1", bytes.NewReader(append(f1, f2...)))

	evs, _, err := store.List(ctx, Query{CameraID: "cam-1"})
	if err != nil || len(evs) != 1 || evs[0].Snapshot == "" || len(evs[0].Detections) == 0 {
		t.Fatalf("expected 1 motion event with snapshot & ROI bbox, got %+v err=%v", evs, err)
	}
	rawSnap, err := os.ReadFile(filepath.Join(snapDir, filepath.FromSlash(evs[0].Snapshot)))
	if err != nil {
		t.Fatalf("read motion snapshot: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(rawSnap))
	if err != nil {
		t.Fatalf("decode motion snapshot: %v", err)
	}
	// Verify amber (#f59e0b) motion box border pixel exists near top edge of ROI [160..320, 90..210].
	foundAmber := false
	for y := 82; y <= 105 && !foundAmber; y++ {
		for x := 152; x <= 200; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if (r>>8) > 190 && (g>>8) > 110 && (b>>8) < 60 {
				foundAmber = true
				break
			}
		}
	}
	if !foundAmber {
		t.Fatalf("expected amber Movimiento bbox stroke around ROI on saved snapshot")
	}
}
