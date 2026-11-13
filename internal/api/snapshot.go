package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

// Snapshots are taken from what is already recorded, never from the camera:
// the recorder holds the only RTSP session and many cameras reject a second
// one. The newest keyframe of the segment being written is decoded, so the
// image is a few seconds old (one keyframe interval at most).
const (
	snapshotTTL   = 5 * time.Second
	snapshotTail  = 8 << 20 // bytes read from the end of the segment
	snapshotSlots = 2
	snapshotWidth = 640
)

type snapshot struct {
	at    time.Time // capture time (segment wall clock)
	taken time.Time
	jpeg  []byte
}

type snapshotCache struct {
	mu   sync.Mutex
	byID map[string]*snapshotEntry
	sem  chan struct{}
}

type snapshotEntry struct {
	mu   sync.Mutex // one extraction per camera at a time
	last snapshot
}

func (s *Server) snapshots() *snapshotCache {
	s.snapOnce.Do(func() {
		s.snap = &snapshotCache{byID: map[string]*snapshotEntry{}, sem: make(chan struct{}, snapshotSlots)}
	})
	return s.snap
}

func (c *snapshotCache) entry(id string) *snapshotEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.byID[id]
	if e == nil {
		e = &snapshotEntry{}
		c.byID[id] = e
	}
	return e
}

func (s *Server) cameraSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "camera not found")
		return
	}
	if s.FFmpeg == "" {
		writeError(w, http.StatusNotImplemented, "not_available", "snapshots need ffmpeg")
		return
	}
	rep := r.URL.Query().Get("representation")
	if rep == "" {
		rep = "main"
	}
	c := s.snapshots()
	e := c.entry(id + "/" + rep)
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.now().Sub(e.last.taken) > snapshotTTL || e.last.jpeg == nil {
		snap, err := s.takeSnapshot(r, c, id, rep)
		switch {
		case err == nil:
			e.last = snap
		case e.last.jpeg == nil || s.now().Sub(e.last.taken) > time.Minute:
			if errors.Is(err, errNoFootage) {
				writeError(w, http.StatusNotFound, "no_recording", "the camera has no recorded footage yet")
				return
			}
			s.Log.Warn("snapshot", "camera", id, "err", err)
			writeError(w, http.StatusServiceUnavailable, "snapshot_failed", "could not decode a frame from the latest recording")
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Snapshot-Time", e.last.at.UTC().Format(time.RFC3339))
	w.Header().Set("Content-Length", strconv.Itoa(len(e.last.jpeg)))
	_, _ = w.Write(e.last.jpeg)
}

var errNoFootage = errors.New("no footage")

func (s *Server) takeSnapshot(r *http.Request, c *snapshotCache, id, rep string) (snapshot, error) {
	ctx := r.Context()
	segs, err := segments.New(s.DB).Latest(ctx, id, rep, 2)
	if err != nil {
		return snapshot{}, err
	}
	if len(segs) == 0 {
		return snapshot{}, errNoFootage
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return snapshot{}, ctx.Err()
	}
	var lastErr error = errNoFootage
	for _, sg := range segs {
		root, ok := s.volumeRoot(r, sg.VolumeID)
		if !ok {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(sg.RelativePath))
		if sg.Status == segments.StatusWriting {
			path = ingest.TempPath(root, sg.ID) // still being written
		}
		img, err := s.lastKeyframe(ctx, path)
		if err != nil {
			lastErr = err
			continue
		}
		at := sg.End
		if at.IsZero() {
			at = s.now()
		}
		return snapshot{at: at, taken: s.now(), jpeg: img}, nil
	}
	return snapshot{}, lastErr
}

// lastKeyframe decodes only keyframes of the file's tail and keeps the last
// one, scaled down. Reading the tail keeps the cost flat however long the
// segment is, and works on the segment still being written.
func (s *Server) lastKeyframe(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := int64(0)
	if size > snapshotTail {
		off = (size - snapshotTail) / 188 * 188
	}
	tail := io.NewSectionReader(f, off, size-off)

	out, err := os.CreateTemp("", "ovr-snap-*.jpg")
	if err != nil {
		return nil, err
	}
	name := out.Name()
	out.Close()
	defer os.Remove(name)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.FFmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-skip_frame", "nokey", "-f", "mpegts", "-i", "pipe:0",
		"-map", "0:v:0", "-an", "-vf", "scale='min("+strconv.Itoa(snapshotWidth)+",iw)':-2",
		"-q:v", "5", "-update", "1", "-y", name)
	cmd.Stdin = tail
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("ffmpeg: " + firstLine(stderr.String(), err))
	}
	img, err := os.ReadFile(name)
	if err != nil || len(img) == 0 {
		return nil, errors.New("no keyframe in the latest recording")
	}
	return img, nil
}

func firstLine(s string, err error) string {
	if i := bytes.IndexByte([]byte(s), '\n'); i > 0 {
		return s[:i]
	}
	if s != "" {
		return s
	}
	return err.Error()
}
