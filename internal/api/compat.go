package api

import (
	"context"
	"io"
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

// transcodeExportMP4 converts an exported MP4 stream (with moov before mdat) to
// fragmented H.264 MP4 for browser playback without altering the stored archive.
func (s *Server) transcodeExportMP4(w http.ResponseWriter, r *http.Request, src io.Reader) {
	if s.FFmpeg == "" {
		writeError(w, http.StatusNotImplemented, "not_available", "ffmpeg not configured")
		return
	}
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
		"-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "25", "-pix_fmt", "yuv420p",
		"-vf", "scale=-2:'min(720,ih)'",
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1")
	cmd.Stdin = src
	cmd.Stdout = w
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("X-Playback-Mode", "compat-h264")
	w.Header().Set("Cache-Control", "no-store")
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		s.Log.Warn("compatible export playback failed", "err", err)
	}
}
