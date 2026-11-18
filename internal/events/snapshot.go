package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

const (
	eventSnapshotWidth = 960
	eventSnapshotSlots = 2
)

// Snapshotter captures JPEG frames from recorded segments (or attached push
// frames) whenever a new event is stored without an existing snapshot_ref,
// and burns Frigate-style bounding-box annotations into the saved JPEG when
// detection coordinates are present.
type Snapshotter struct {
	DB         *sql.DB
	Dir        string
	FFmpeg     string
	VolumeRoot func(id string) (string, bool)
	Log        *slog.Logger

	once sync.Once
	sem  chan struct{}
}

func (s *Snapshotter) slots() chan struct{} {
	s.once.Do(func() { s.sem = make(chan struct{}, eventSnapshotSlots) })
	return s.sem
}

// CaptureAsync captures a snapshot for ev in the background without blocking
// event ingestion.
func (s *Snapshotter) CaptureAsync(ev Event) {
	if s == nil || s.DB == nil || s.Dir == "" {
		return
	}
	if len(ev.SnapshotData) == 0 && (s.FFmpeg == "" || s.VolumeRoot == nil) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := s.Capture(ctx, ev); err != nil && s.Log != nil {
			s.Log.Debug("event snapshot skipped", "event_id", ev.EventID, "camera", ev.CameraID, "err", err)
		}
	}()
}

// LatestFrameJPEG extracts a JPEG keyframe from the newest recorded segment for
// cameraID without opening an additional RTSP session.
func (s *Snapshotter) LatestFrameJPEG(ctx context.Context, cameraID string) ([]byte, error) {
	if s == nil || s.DB == nil || s.FFmpeg == "" || s.VolumeRoot == nil {
		return nil, errors.New("snapshotter not configured")
	}
	segStore := segments.New(s.DB)
	var segs []segments.Segment
	for _, rep := range []string{"main", "sub"} {
		found, err := segStore.Latest(ctx, cameraID, rep, 2)
		if err == nil && len(found) > 0 {
			segs = found
			break
		}
	}
	if len(segs) == 0 {
		return nil, errors.New("no recorded segments for camera")
	}
	var lastErr error
	for _, sg := range segs {
		root, ok := s.VolumeRoot(sg.VolumeID)
		if !ok {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(sg.RelativePath))
		if sg.Status == segments.StatusWriting {
			path = ingest.TempPath(root, sg.ID)
		}
		b, err := ingest.LastKeyframe(ctx, s.FFmpeg, path, eventSnapshotWidth)
		if err != nil {
			lastErr = err
			continue
		}
		return b, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("could not decode frame from segments")
}

// Capture decodes a JPEG from ev.SnapshotData (if supplied) or the latest
// recorded segment for ev.CameraID, overlays any detection bounding box, saves
// it under Dir/<camera_id>/<event_id>.jpg, and updates events.snapshot_ref.
func (s *Snapshotter) Capture(ctx context.Context, ev Event) (string, error) {
	if s == nil || s.DB == nil || s.Dir == "" {
		return "", errors.New("snapshotter not configured")
	}
	if len(ev.SnapshotData) == 0 && (s.FFmpeg == "" || s.VolumeRoot == nil) {
		return "", errors.New("snapshotter not configured")
	}
	if ev.EventID == "" || ev.CameraID == "" {
		return "", errors.New("event_id and camera_id required")
	}
	select {
	case s.slots() <- struct{}{}:
		defer func() { <-s.slots() }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	var img []byte
	if len(ev.SnapshotData) > 0 {
		img = ev.SnapshotData
	} else {
		var err error
		img, err = s.LatestFrameJPEG(ctx, ev.CameraID)
		if err != nil {
			return "", err
		}
	}

	// Burn bounding boxes only onto synchronous frames matching the detection
	// instant; drawing push/ONVIF boxes onto a historical LatestFrameJPEG from
	// an earlier closed segment puts detection boxes over empty background.
	canAnnotateFrame := len(ev.SnapshotData) > 0 || ev.Source == "nvr_motion" || ev.Source == "nvr_classifier" || ev.Source == "http_rest"
	if canAnnotateFrame && ShouldAnnotateBBox(ev) {
		if annotated, err := AnnotateDetectionJPEG(img, ev); err == nil && len(annotated) > 0 {
			img = annotated
		} else if err != nil && s.Log != nil {
			s.Log.Warn("event snapshot bbox annotation failed; saving raw frame", "event_id", ev.EventID, "err", err)
		}
	} else if !bytes.HasPrefix(img, []byte{0xFF, 0xD8, 0xFF}) {
		// Normalize non-JPEG pushed frames (e.g. PNG) to JPEG.
		if decoded, _, err := image.Decode(bytes.NewReader(img)); err == nil {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, decoded, &jpeg.Options{Quality: 88}); err == nil {
				img = buf.Bytes()
			}
		}
	}

	camDir := filepath.Join(s.Dir, filepath.Base(ev.CameraID))
	if err := os.MkdirAll(camDir, 0o750); err != nil {
		return "", err
	}
	filePath := filepath.Join(camDir, filepath.Base(ev.EventID)+".jpg")
	if err := os.WriteFile(filePath, img, 0o640); err != nil {
		return "", err
	}
	rel := filepath.Base(ev.CameraID) + "/" + filepath.Base(ev.EventID) + ".jpg"
	if _, err := s.DB.ExecContext(ctx, `UPDATE events SET snapshot_ref = ? WHERE id = ?`, rel, ev.EventID); err != nil {
		return "", err
	}
	return rel, nil
}
