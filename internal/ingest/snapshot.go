package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const snapshotTailBytes = 8 << 20

// LastKeyframe decodes the last keyframe in the tail of an MPEG-TS segment as
// a JPEG scaled to at most width pixels wide (preserving aspect ratio).
func LastKeyframe(ctx context.Context, ffmpeg, path string, width int) ([]byte, error) {
	if ffmpeg == "" {
		return nil, errors.New("ffmpeg path not configured")
	}
	if width <= 0 {
		width = 640
	}
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
	if size > snapshotTailBytes {
		off = (size - snapshotTailBytes) / 188 * 188
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
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-skip_frame", "nokey", "-f", "mpegts", "-i", "pipe:0",
		"-map", "0:v:0", "-an", "-vf", "scale='min("+strconv.Itoa(width)+",iw)':-2",
		"-q:v", "5", "-update", "1", "-y", name)
	cmd.Stdin = tail
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := err.Error()
		if s := stderr.String(); s != "" {
			if i := bytes.IndexByte([]byte(s), '\n'); i > 0 {
				msg = s[:i]
			} else {
				msg = s
			}
		}
		return nil, errors.New("ffmpeg: " + msg)
	}
	img, err := os.ReadFile(name)
	if err != nil || len(img) == 0 {
		return nil, errors.New("no keyframe in the latest recording")
	}
	return img, nil
}
