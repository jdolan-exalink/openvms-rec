package events

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeSolidJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestAnnotateDetectionJPEG_PersonAndVehicle(t *testing.T) {
	raw := makeSolidJPEG(t, 640, 360, color.RGBA{24, 24, 28, 255})
	conf := 0.95

	personEv := Event{
		EventID:    "evt_person_1",
		CameraID:   "cam-1",
		Type:       "person",
		Class:      "person",
		Confidence: &conf,
		BBox:       []float64{0.25, 0.20, 0.30, 0.50}, // x=160..352, y=72..252
	}
	outBytes, err := AnnotateDetectionJPEG(raw, personEv)
	if err != nil {
		t.Fatalf("AnnotateDetectionJPEG(person): %v", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(outBytes))
	if err != nil {
		t.Fatalf("decode annotated person jpeg: %v", err)
	}
	if decoded.Bounds().Dx() != 640 || decoded.Bounds().Dy() != 360 {
		t.Fatalf("unexpected dimensions: %v", decoded.Bounds())
	}

	// Box border at (200, 72) should be blue (#3b82f6).
	r, g, b, _ := decoded.At(200, 72).RGBA()
	if (b>>8) < 180 || (b>>8) <= (r>>8)+60 {
		t.Fatalf("expected blue border pixel at (200,72), got rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
	}

	// Label bar just above y=72 (e.g. y=62) should contain white text pixels and blue bg pixels.
	hasWhite, hasBlue := false, false
	for y := 50; y < 72; y++ {
		for x := 160; x < 300; x++ {
			pr, pg, pb, _ := decoded.At(x, y).RGBA()
			R, G, B := pr>>8, pg>>8, pb>>8
			if R > 210 && G > 210 && B > 210 {
				hasWhite = true
			}
			if B > 180 && B > R+60 {
				hasBlue = true
			}
		}
	}
	if !hasWhite || !hasBlue {
		t.Fatalf("expected white text (%v) on blue label bar (%v)", hasWhite, hasBlue)
	}

	// Vehicle detection should draw green (#10b981).
	vConf := 0.88
	vehicleEv := Event{
		EventID:    "evt_veh_1",
		CameraID:   "cam-1",
		Type:       "vehicle",
		Class:      "vehicle",
		Confidence: &vConf,
		BBox:       []float64{0.40, 0.30, 0.40, 0.40}, // x=256..512, y=108..252
	}
	vehBytes, err := AnnotateDetectionJPEG(raw, vehicleEv)
	if err != nil {
		t.Fatalf("AnnotateDetectionJPEG(vehicle): %v", err)
	}
	vehImg, _, err := image.Decode(bytes.NewReader(vehBytes))
	if err != nil {
		t.Fatalf("decode annotated vehicle jpeg: %v", err)
	}
	vr, vg, vb, _ := vehImg.At(300, 108).RGBA()
	if (vg>>8) < 140 || (vg>>8) <= (vr>>8)+60 {
		t.Fatalf("expected green border pixel at (300,108), got rgb(%d,%d,%d)", vr>>8, vg>>8, vb>>8)
	}
}

func TestAnnotateDetectionJPEG_TopAndRightEdgeClamp(t *testing.T) {
	raw := makeSolidJPEG(t, 640, 360, color.RGBA{20, 20, 20, 255})
	conf := 0.91
	ev := Event{
		Type:       "person",
		Confidence: &conf,
		BBox:       []float64{0.92, 0.0, 0.08, 0.40},
	}
	outBytes, err := AnnotateDetectionJPEG(raw, ev)
	if err != nil {
		t.Fatalf("AnnotateDetectionJPEG edge clamp: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(outBytes))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Label bar should be clamped inside top-right area with white text pixels.
	hasWhite := false
	for y := 0; y < 30; y++ {
		for x := 450; x < 640; x++ {
			pr, pg, pb, _ := img.At(x, y).RGBA()
			if (pr>>8) > 210 && (pg>>8) > 210 && (pb>>8) > 210 {
				hasWhite = true
				break
			}
		}
	}
	if !hasWhite {
		t.Fatalf("expected clamped white label text in top-right region")
	}
}

func TestAnnotateDetectionJPEG_NoBBoxReturnsUnchanged(t *testing.T) {
	raw := makeSolidJPEG(t, 320, 180, color.RGBA{10, 10, 10, 255})
	for _, bbox := range [][]float64{nil, {0.1, 0.2}, {0.1, 0.2, 0, 0.4}, {1.2, 0.2, 0.2, 0.2}} {
		out, err := AnnotateDetectionJPEG(raw, Event{Type: "person", BBox: bbox})
		if err != nil {
			t.Fatalf("bbox=%v err=%v", bbox, err)
		}
		if !bytes.Equal(out, raw) {
			t.Fatalf("expected raw bytes unchanged for bbox=%v", bbox)
		}
	}
}

func TestSnapshotter_SavesAnnotatedJPEG(t *testing.T) {
	store, _ := openTestEnv(t)
	ctx := context.Background()
	snapDir := t.TempDir()
	snap := &Snapshotter{
		DB:  store.db,
		Dir: snapDir,
	}

	raw := makeSolidJPEG(t, 640, 360, color.RGBA{18, 20, 24, 255})
	conf := 0.93
	ev := Event{
		EventID:      "evt_box_saved",
		CameraID:     "cam-1",
		TimestampUTC: time.Now().UTC(),
		Type:         "person",
		Class:        "person",
		Confidence:   &conf,
		BBox:         []float64{0.2, 0.25, 0.25, 0.5},
		Source:       "provisionISR",
		SnapshotData: raw,
	}
	ok, err := store.Insert(ctx, ev, "snap-box-1")
	if err != nil || !ok {
		t.Fatalf("store.Insert: ok=%v err=%v", ok, err)
	}
	rel, err := snap.Capture(ctx, ev)
	if err != nil {
		t.Fatalf("snap.Capture: %v", err)
	}
	if rel != "cam-1/evt_box_saved.jpg" {
		t.Fatalf("unexpected rel: %q", rel)
	}
	saved, err := os.ReadFile(filepath.Join(snapDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read saved snapshot: %v", err)
	}
	if bytes.Equal(saved, raw) {
		t.Fatalf("expected saved snapshot to include bounding box annotation, got identical raw bytes")
	}
	img, _, err := image.Decode(bytes.NewReader(saved))
	if err != nil {
		t.Fatalf("decode saved snapshot: %v", err)
	}
	// Verify blue box top border at x=150, y=90 (0.25 * 360 = 90).
	r, _, b, _ := img.At(150, 90).RGBA()
	if (b>>8) < 180 || (b>>8) <= (r>>8)+60 {
		t.Fatalf("expected blue detection box in saved snapshot at (150,90), got r=%d b=%d", r>>8, b>>8)
	}
	dbRef, err := store.GetSnapshotRef(ctx, "evt_box_saved")
	if err != nil || dbRef != rel {
		t.Fatalf("expected DB snapshot_ref=%q, got %q err=%v", rel, dbRef, err)
	}
}

func TestAnnotateDetectionJPEG_MultipleBoxes(t *testing.T) {
	raw := makeSolidJPEG(t, 640, 360, color.RGBA{24, 24, 24, 255})
	c1, c2 := 0.94, 0.87
	ev := Event{
		EventID: "evt_multi",
		Type:    "person",
		Detections: []Detection{
			{Type: "person", Class: "person", Confidence: &c1, BBox: []float64{0.10, 0.20, 0.15, 0.50}},
			{Type: "vehicle", Class: "motorcycle", Confidence: &c2, BBox: []float64{0.50, 0.30, 0.30, 0.40}},
		},
	}
	out, err := AnnotateDetectionJPEG(raw, ev)
	if err != nil {
		t.Fatalf("AnnotateDetectionJPEG(multi): %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Person box blue top border near (90, 72) and Vehicle box green top border near (360, 108).
	pr, _, pb, _ := img.At(90, 72).RGBA()
	if (pb>>8) < 180 || (pb>>8) <= (pr>>8)+60 {
		t.Fatalf("expected blue border for person at (90,72), got r=%d b=%d", pr>>8, pb>>8)
	}
	vr, vg, _, _ := img.At(360, 108).RGBA()
	if (vg>>8) < 140 || (vg>>8) <= (vr>>8)+60 {
		t.Fatalf("expected green border for motorcycle at (360,108), got r=%d g=%d", vr>>8, vg>>8)
	}
}
