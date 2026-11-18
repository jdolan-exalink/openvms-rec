package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

const (
	motionWidth       = 160
	motionHeight      = 90
	motionFrameBytes  = motionWidth * motionHeight
	motionColorWidth  = 640
	motionColorHeight = 360
	motionColorBytes  = motionColorWidth * motionColorHeight * 3
	motionPixelThresh = 30
	motionAreaThresh  = 0.025
	motionCooldown    = 10 * time.Second
)

// LiveStreamSource taps an active ingest hub so analytics do not consume an
// extra RTSP connection slot on the camera when ingest is already running.
type LiveStreamSource interface {
	PreferredStream(cameraID string, staleAfter time.Duration) (rep string, ok bool)
	Subscribe(ctx context.Context, cameraID, rep string) io.ReadCloser
}

// MotionDetector runs lightweight keyframe/1fps grayscale frame differencing
// via FFmpeg for cameras with NVRMotion enabled, extracting motion ROIs and
// optionally delegating keyframes to an ObjectClassifier.
type MotionDetector struct {
	Store        *Store
	Cameras      *cameras.Registry
	Hub          LiveStreamSource
	FFmpegPath   string
	Classifier   ObjectClassifier
	FrameFetcher func(ctx context.Context, cameraID string) ([]byte, error)
	Log          *slog.Logger

	mu      sync.Mutex
	runCtx  context.Context
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// NewMotionDetector constructs a local NVR motion analytics listener.
func NewMotionDetector(store *Store, cams *cameras.Registry, ffmpegPath string, log *slog.Logger) *MotionDetector {
	if log == nil {
		log = slog.Default()
	}
	return &MotionDetector{
		Store:      store,
		Cameras:    cams,
		FFmpegPath: ffmpegPath,
		Log:        log,
		workers:    make(map[string]context.CancelFunc),
	}
}

func (m *MotionDetector) Name() string { return "nvr_motion" }

// Start reconciles camera motion workers and blocks until ctx is cancelled.
func (m *MotionDetector) Start(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()

	m.Sync()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for id, cancel := range m.workers {
				cancel()
				delete(m.workers, id)
			}
			m.mu.Unlock()
			m.wg.Wait()
			return
		case <-tick.C:
			m.Sync()
		}
	}
}

// RestartCamera stops and restarts the local motion worker for id.
func (m *MotionDetector) RestartCamera(id string) {
	m.mu.Lock()
	if cancel, ok := m.workers[id]; ok {
		cancel()
		delete(m.workers, id)
	}
	ctx := m.runCtx
	m.mu.Unlock()
	if ctx != nil && ctx.Err() == nil {
		m.Sync()
	}
}

// Sync reconciles active motion workers with the camera registry.
func (m *MotionDetector) Sync() {
	if m.Cameras == nil || m.FFmpegPath == "" {
		return
	}
	m.mu.Lock()
	runCtx := m.runCtx
	m.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return
	}

	list := m.Cameras.List()
	wanted := make(map[string]cameras.Entry)
	for _, e := range list {
		if e.IsEnabled() && (e.NVRMotion || m.Classifier != nil) {
			wanted[e.ID] = e
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.workers {
		if _, ok := wanted[id]; !ok {
			cancel()
			delete(m.workers, id)
		}
	}
	for id, e := range wanted {
		if _, ok := m.workers[id]; ok {
			continue
		}
		wCtx, cancel := context.WithCancel(runCtx)
		m.workers[id] = cancel
		m.wg.Add(1)
		go func(entry cameras.Entry) {
			defer m.wg.Done()
			m.runCamera(wCtx, entry)
		}(e)
	}
}

func (m *MotionDetector) runCamera(ctx context.Context, e cameras.Entry) {
	streamURL := e.SubURL
	if streamURL == "" {
		streamURL = e.MainURL
	}
	if streamURL == "" {
		return
	}
	if c, ok := m.Cameras.Credentials(e.ID); ok && c.Username != "" {
		if u, err := url.Parse(streamURL); err == nil && u.User == nil {
			u.User = url.UserPassword(c.Username, c.Password)
			streamURL = u.String()
		}
	}
	transport := e.Transport
	if transport == "" {
		transport = "tcp"
	}

	if m.Hub != nil && e.IsEnabled() {
		deadline := time.Now().Add(10 * time.Second)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			if _, ok := m.Hub.PreferredStream(e.ID, 10*time.Second); ok {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		var err error
		if m.Hub != nil {
			if rep, ok := m.Hub.PreferredStream(e.ID, 10*time.Second); ok {
				err = m.runFFmpegFromHub(ctx, e.ID, rep)
			} else {
				err = m.runFFmpeg(ctx, e.ID, streamURL, transport)
			}
		} else {
			err = m.runFFmpeg(ctx, e.ID, streamURL, transport)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = 5 * time.Second
		}
		m.Log.Warn("nvr motion detector retry", "camera", e.ID, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (m *MotionDetector) runFFmpegFromHub(ctx context.Context, cameraID, rep string) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sub := m.Hub.Subscribe(subCtx, cameraID, rep)
	defer sub.Close()

	cmd := exec.CommandContext(subCtx, m.FFmpegPath,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-fflags", "+genpts+discardcorrupt",
		"-analyzeduration", "5000000",
		"-probesize", "2097152",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-vf", fmt.Sprintf("fps=1,scale=%d:%d,format=rgb24", motionColorWidth, motionColorHeight),
		"-f", "rawvideo",
		"pipe:1",
	)
	cmd.Stdin = sub
	idle := time.AfterFunc(45*time.Second, cancel)
	defer idle.Stop()
	return m.runFFmpegCmd(subCtx, cameraID, cmd, cancel, func() {
		idle.Reset(45 * time.Second)
	})
}

func (m *MotionDetector) runFFmpeg(ctx context.Context, cameraID, streamURL, transport string) error {
	cmd := exec.CommandContext(ctx, m.FFmpegPath,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-rtsp_transport", transport,
		"-i", streamURL,
		"-vf", fmt.Sprintf("fps=1,scale=%d:%d,format=rgb24", motionColorWidth, motionColorHeight),
		"-f", "rawvideo",
		"pipe:1",
	)
	return m.runFFmpegCmd(ctx, cameraID, cmd, nil, nil)
}

func (m *MotionDetector) runFFmpegCmd(ctx context.Context, cameraID string, cmd *exec.Cmd, cancel context.CancelFunc, onFrame func()) error {
	var errBuf tailWriter
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	procErr := m.processColorFrameStreamWithReset(ctx, cameraID, stdout, onFrame)
	if cancel != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			if len(msg) > 240 {
				msg = msg[len(msg)-240:]
			}
			return fmt.Errorf("%w: %s", waitErr, msg)
		}
		return waitErr
	}
	if procErr != nil && procErr != io.EOF {
		return procErr
	}
	return nil
}

type tailWriter struct{ buf []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	const capBytes = 1024
	w.buf = append(w.buf, p...)
	if len(w.buf) > capBytes {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-capBytes:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.buf) }

func (m *MotionDetector) processColorFrameStream(ctx context.Context, cameraID string, r io.Reader) error {
	return m.processColorFrameStreamWithReset(ctx, cameraID, r, nil)
}

func (m *MotionDetector) processColorFrameStreamWithReset(ctx context.Context, cameraID string, r io.Reader, onFrame func()) error {
	rgb := make([]byte, motionColorBytes)
	prev := make([]byte, motionFrameBytes)
	cur := make([]byte, motionFrameBytes)
	havePrev := false
	var lastEvent time.Time

	for ctx.Err() == nil {
		if _, err := io.ReadFull(r, rgb); err != nil {
			return err
		}
		if onFrame != nil {
			onFrame()
		}
		downsampleRGB24ToGray(rgb, motionColorWidth, motionColorHeight, cur, motionWidth, motionHeight)
		if !havePrev {
			copy(prev, cur)
			havePrev = true
			continue
		}
		ratio, rois := ExtractMotionROIs(prev, cur, motionWidth, motionHeight, motionPixelThresh)
		copy(prev, cur)
		if ratio < motionAreaThresh {
			continue
		}
		now := time.Now().UTC()
		if !lastEvent.IsZero() && now.Sub(lastEvent) < motionCooldown {
			continue
		}
		lastEvent = now
		jpegBytes := encodeRGB24JPEG(rgb, motionColorWidth, motionColorHeight)
		m.recordMotionEvent(ctx, cameraID, now, ratio, rois, jpegBytes)
	}
	return ctx.Err()
}

func (m *MotionDetector) processFrameStream(ctx context.Context, cameraID string, r io.Reader) error {
	prev := make([]byte, motionFrameBytes)
	cur := make([]byte, motionFrameBytes)
	havePrev := false
	var lastEvent time.Time

	for ctx.Err() == nil {
		if _, err := io.ReadFull(r, cur); err != nil {
			return err
		}
		if !havePrev {
			copy(prev, cur)
			havePrev = true
			continue
		}
		ratio, rois := ExtractMotionROIs(prev, cur, motionWidth, motionHeight, motionPixelThresh)
		copy(prev, cur)
		if ratio < motionAreaThresh {
			continue
		}
		now := time.Now().UTC()
		if !lastEvent.IsZero() && now.Sub(lastEvent) < motionCooldown {
			continue
		}
		lastEvent = now
		m.recordMotionEvent(ctx, cameraID, now, ratio, rois, nil)
	}
	return ctx.Err()
}

func (m *MotionDetector) recordMotionEvent(ctx context.Context, cameraID string, now time.Time, ratio float64, rois [][]float64, syncJPEG []byte) {
	if m.Classifier != nil {
		jpegBytes := syncJPEG
		if len(jpegBytes) == 0 && m.FrameFetcher != nil {
			fCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			jpegBytes, _ = m.FrameFetcher(fCtx, cameraID)
			cancel()
		}
		if len(jpegBytes) > 0 {
			cCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			dets, cErr := m.Classifier.ClassifyFrame(cCtx, cameraID, jpegBytes, rois)
			cancel()
			if cErr != nil {
				m.Log.Debug("local classifier error", "camera", cameraID, "err", cErr)
			} else if len(dets) > 0 {
				ev := Event{
					CameraID:     cameraID,
					TimestampUTC: now,
					Detections:   dets,
					Source:       "nvr_classifier",
					SnapshotData: jpegBytes,
				}
				if NormalizeEvent(&ev, 0) {
					dedupeKey := fmt.Sprintf("nvrcls:%s:%s:%d", cameraID, ev.Type, now.Unix()/10)
					inserted, err := m.Store.Insert(ctx, ev, dedupeKey)
					if err == nil && inserted {
						m.Log.Info("nvr classifier event recorded", "camera", cameraID, "type", ev.Type, "class", ev.Class, "detections", len(ev.Detections))
						evJSON, _ := json.Marshal(map[string]any{"source": "nvr_classifier", "detections": len(ev.Detections)})
						_ = m.Store.SaveCapabilities(ctx, cameraID, []onvif.Capability{{
							Capability: "analytics." + ev.Type,
							Status:     onvif.CapConfirmed,
							Evidence:   string(evJSON),
							CheckedAt:  now,
						}})
					}
					return
				}
			}
		}
	}

	// Emit a motion event with extracted ROI bboxes (plus synchronous live JPEG when available)
	// only if nvr_motion is enabled for this camera and at least one region was localized.
	if entry, ok := m.Cameras.Get(cameraID); !ok || !entry.NVRMotion || len(rois) == 0 {
		return
	}
	conf := min(0.99, max(0.50, ratio*4.0))
	var roiDets []Detection
	var primaryBox []float64
	for _, box := range rois {
		c := conf
		roiDets = append(roiDets, Detection{
			Type:       "motion",
			Class:      "motion",
			Confidence: &c,
			BBox:       box,
		})
	}
	if len(rois) > 0 {
		primaryBox = rois[0]
	}
	dedupeKey := fmt.Sprintf("nvrmotion:%s:%d", cameraID, now.Unix()/10)
	inserted, err := m.Store.Insert(ctx, Event{
		CameraID:     cameraID,
		TimestampUTC: now,
		Type:         "motion",
		Class:        "motion",
		Confidence:   &conf,
		BBox:         primaryBox,
		Detections:   roiDets,
		Source:       "nvr_motion",
		SnapshotData: syncJPEG,
	}, dedupeKey)
	if err != nil {
		m.Log.Warn("nvr motion insert failed", "camera", cameraID, "err", err)
		return
	}
	if inserted {
		m.Log.Info("nvr motion event recorded", "camera", cameraID, "change_pct", int(ratio*100), "rois", len(rois))
		evJSON, _ := json.Marshal(map[string]any{"source": "nvr_motion", "change_ratio": ratio, "rois": len(rois)})
		_ = m.Store.SaveCapabilities(ctx, cameraID, []onvif.Capability{{
			Capability: "analytics.motion",
			Status:     onvif.CapConfirmed,
			Evidence:   string(evJSON),
			CheckedAt:  now,
		}})
	}
}

func downsampleRGB24ToGray(rgb []byte, srcW, srcH int, dst []byte, dstW, dstH int) {
	sx := srcW / dstW
	sy := srcH / dstH
	for y := 0; y < dstH; y++ {
		py := (y*sy + sy/2) * srcW * 3
		dy := y * dstW
		for x := 0; x < dstW; x++ {
			off := py + (x*sx+sx/2)*3
			r, g, b := int(rgb[off]), int(rgb[off+1]), int(rgb[off+2])
			dst[dy+x] = byte((299*r + 587*g + 114*b + 500) / 1000)
		}
	}
}

func encodeRGB24JPEG(rgb []byte, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, p := 0, 0; i+2 < len(rgb) && p+3 < len(img.Pix); i, p = i+3, p+4 {
		img.Pix[p] = rgb[i]
		img.Pix[p+1] = rgb[i+1]
		img.Pix[p+2] = rgb[i+2]
		img.Pix[p+3] = 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// DetectFrameChange returns the fraction of pixels whose luma difference
// exceeds pixelThresh. Both slices must have equal length.
func DetectFrameChange(prev, cur []byte, pixelThresh uint8) float64 {
	if len(prev) == 0 || len(prev) != len(cur) {
		return 0
	}
	changed := 0
	t := int(pixelThresh)
	for i := range prev {
		d := int(cur[i]) - int(prev[i])
		if d < 0 {
			d = -d
		}
		if d > t {
			changed++
		}
	}
	return float64(changed) / float64(len(prev))
}
