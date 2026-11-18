package events

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
)

// ObjectClassifier classifies objects (person, vehicle subtypes, face, etc.)
// from a JPEG frame and optional normalized motion ROIs.
type ObjectClassifier interface {
	ClassifyFrame(ctx context.Context, cameraID string, jpegBytes []byte, motionROIs [][]float64) ([]Detection, error)
}

// NewObjectClassifier builds a classifier from config. Returns nil when disabled ("none").
func NewObjectClassifier(cfg config.ClassifierConfig, transport http.RoundTripper) ObjectClassifier {
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2500 * time.Millisecond
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "http":
		return &HTTPClassifier{
			Endpoint: strings.TrimSpace(cfg.Endpoint),
			Client:   &http.Client{Transport: transport, Timeout: timeout},
			MinConf:  cfg.MinConfidence,
		}
	case "command":
		return &CommandClassifier{
			Command: append([]string(nil), cfg.Command...),
			Timeout: timeout,
			MinConf: cfg.MinConfidence,
		}
	default:
		return nil
	}
}

type classifierRequest struct {
	CameraID     string      `json:"camera_id"`
	ImageJPEGB64 string      `json:"image_jpeg_base64"`
	MotionROIs   [][]float64 `json:"motion_rois,omitempty"`
}

type classifierResponse struct {
	Detections []struct {
		Type       string    `json:"type"`
		Class      string    `json:"class"`
		Label      string    `json:"label"`
		Confidence *float64  `json:"confidence"`
		BBox       []float64 `json:"bbox"`
	} `json:"detections"`
}

// HTTPClassifier delegates object inference to an external HTTP endpoint.
type HTTPClassifier struct {
	Endpoint string
	Client   *http.Client
	MinConf  float64
}

func (c *HTTPClassifier) ClassifyFrame(ctx context.Context, cameraID string, jpegBytes []byte, motionROIs [][]float64) ([]Detection, error) {
	if c == nil || c.Endpoint == "" || len(jpegBytes) == 0 {
		return nil, nil
	}
	payload, _ := json.Marshal(classifierRequest{
		CameraID:     cameraID,
		ImageJPEGB64: base64.StdEncoding.EncodeToString(jpegBytes),
		MotionROIs:   motionROIs,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("classifier HTTP %d", resp.StatusCode)
	}
	return parseClassifierResponse(body, c.MinConf)
}

// CommandClassifier runs a local command passing JPEG bytes on stdin and reading
// JSON classifierResponse from stdout.
type CommandClassifier struct {
	Command []string
	Timeout time.Duration
	MinConf float64
}

func (c *CommandClassifier) ClassifyFrame(ctx context.Context, _ string, jpegBytes []byte, _ [][]float64) ([]Detection, error) {
	if c == nil || len(c.Command) == 0 || len(jpegBytes) == 0 {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.Command[0], c.Command[1:]...)
	cmd.Stdin = bytes.NewReader(jpegBytes)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseClassifierResponse(out, c.MinConf)
}

func parseClassifierResponse(raw []byte, minConf float64) ([]Detection, error) {
	var parsed classifierResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	var out []Detection
	for _, d := range parsed.Detections {
		rawC := d.Class
		if rawC == "" {
			rawC = d.Label
		}
		nt, nc := NormalizeDetectionClass(d.Type, rawC)
		if nt == "" {
			continue
		}
		var conf *float64
		if d.Confidence != nil {
			v := clampConfidence(*d.Confidence)
			if minConf > 0 && v < minConf {
				continue
			}
			conf = &v
		}
		out = append(out, Detection{
			Type:       nt,
			Class:      nc,
			Confidence: conf,
			BBox:       NormalizeBBoxSlice(d.BBox),
		})
	}
	if len(out) == 0 && len(parsed.Detections) == 0 && !json.Valid(raw) {
		return nil, errors.New("invalid classifier JSON response")
	}
	return out, nil
}

// ExtractMotionROIs computes the changed-pixel ratio and normalized [x, y, w, h]
// bounding boxes of contiguous active motion regions using pure-Go connected
// components, filtering single-pixel noise and merging touching clusters.
func ExtractMotionROIs(prev, cur []byte, w, h int, pixelThresh uint8) (float64, [][]float64) {
	if w <= 0 || h <= 0 || len(prev) != w*h || len(cur) != len(prev) {
		return 0, nil
	}
	changedCount := 0
	mask := make([]bool, len(prev))
	t := int(pixelThresh)
	for i := range prev {
		d := int(cur[i]) - int(prev[i])
		if d < 0 {
			d = -d
		}
		if d > t {
			mask[i] = true
			changedCount++
		}
	}
	ratio := float64(changedCount) / float64(len(prev))
	if changedCount < 6 {
		return ratio, nil
	}

	visited := make([]bool, len(prev))
	queue := make([]int, 0, 256)
	type pxBox struct{ x1, y1, x2, y2, area int }
	var boxes []pxBox

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if !mask[idx] || visited[idx] {
				continue
			}
			visited[idx] = true
			queue = append(queue[:0], idx)
			minX, minY, maxX, maxY, area := x, y, x, y, 0
			for head := 0; head < len(queue); head++ {
				curIdx := queue[head]
				cx, cy := curIdx%w, curIdx/w
				area++
				if cx < minX {
					minX = cx
				}
				if cx > maxX {
					maxX = cx
				}
				if cy < minY {
					minY = cy
				}
				if cy > maxY {
					maxY = cy
				}
				// 8-connectivity merges adjacent diagonal motion fragments.
				for dy := -1; dy <= 1; dy++ {
					ny := cy + dy
					if ny < 0 || ny >= h {
						continue
					}
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx := cx + dx
						if nx < 0 || nx >= w {
							continue
						}
						nidx := ny*w + nx
						if mask[nidx] && !visited[nidx] {
							visited[nidx] = true
							queue = append(queue, nidx)
						}
					}
				}
			}
			if area >= 6 {
				boxes = append(boxes, pxBox{minX, minY, maxX + 1, maxY + 1, area})
			}
		}
	}

	// Merge close/overlapping boxes within a small spatial margin.
	const mergePad = 4
	merged := true
	for merged {
		merged = false
		for i := 0; i < len(boxes) && !merged; i++ {
			for j := i + 1; j < len(boxes); j++ {
				a, b := boxes[i], boxes[j]
				if a.x1-mergePad <= b.x2 && b.x1-mergePad <= a.x2 &&
					a.y1-mergePad <= b.y2 && b.y1-mergePad <= a.y2 {
					boxes[i] = pxBox{
						x1:   min(a.x1, b.x1),
						y1:   min(a.y1, b.y1),
						x2:   max(a.x2, b.x2),
						y2:   max(a.y2, b.y2),
						area: a.area + b.area,
					}
					boxes = append(boxes[:j], boxes[j+1:]...)
					merged = true
					break
				}
			}
		}
	}

	// Keep up to top 5 ROIs sorted largest-first (insertion sort since tiny N).
	for i := 1; i < len(boxes); i++ {
		for j := i; j > 0 && boxes[j].area > boxes[j-1].area; j-- {
			boxes[j], boxes[j-1] = boxes[j-1], boxes[j]
		}
	}
	if len(boxes) > 5 {
		boxes = boxes[:5]
	}

	rois := make([][]float64, 0, len(boxes))
	fw, fh := float64(w), float64(h)
	for _, b := range boxes {
		rois = append(rois, []float64{
			float64(b.x1) / fw,
			float64(b.y1) / fh,
			float64(b.x2-b.x1) / fw,
			float64(b.y2-b.y1) / fh,
		})
	}
	return ratio, rois
}
