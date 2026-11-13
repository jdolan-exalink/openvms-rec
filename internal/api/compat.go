package api

import (
	"context"
	"net/http"
	"os/exec"
	"time"
)

// compatSlots bounds concurrent on-the-fly conversions: each one uses a few
// CPU cores while a viewer watches HEVC footage in a browser without HEVC.
const compatSlots = 2

func (s *Server) compatSem() chan struct{} {
	s.compatOnce.Do(func() { s.compat = make(chan struct{}, compatSlots) })
	return s.compat
}

// transcodeSegment streams one recorded segment converted to H.264 (720p at
// most, audio copied). The recording itself is never modified; this is only
// for viewing. Timestamps are kept (-copyts) so consecutive segments play
// continuously.
func (s *Server) transcodeSegment(w http.ResponseWriter, r *http.Request, path string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	select {
	case s.compatSem() <- struct{}{}:
		defer func() { <-s.compatSem() }()
	case <-time.After(30 * time.Second):
		writeError(w, http.StatusServiceUnavailable, "busy", "too many compatible-mode viewers; retry shortly")
		return
	case <-ctx.Done():
		return
	}
	cmd := exec.CommandContext(ctx, s.FFmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-copyts", "-i", path,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p",
		"-vf", "scale=-2:'min(720,ih)'",
		"-c:a", "copy",
		"-muxdelay", "0", "-f", "mpegts", "pipe:1")
	cmd.Stdout = w
	w.Header().Set("X-Playback-Mode", "compat-h264")
	w.Header().Del("Cache-Control")
	w.Header().Set("Cache-Control", "no-store")
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		s.Log.Warn("compatible playback conversion failed", "err", err)
	}
}
