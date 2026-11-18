package api

import (
	"errors"
	"net/http"
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
		img, err := ingest.LastKeyframe(ctx, s.FFmpeg, path, snapshotWidth)
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
