package ingest

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProbeStream is one elementary stream reported by FFmpeg.
type ProbeStream struct {
	Kind       string  `json:"kind"` // video | audio | data
	Codec      string  `json:"codec"`
	Profile    string  `json:"profile,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	SampleRate int     `json:"sample_rate,omitempty"`
}

// ProbeResult is the outcome of a connection test. Error codes:
// auth_failed, not_found, unreachable, timeout, no_video, error.
type ProbeResult struct {
	OK         bool          `json:"ok"`
	Error      string        `json:"error,omitempty"`
	Message    string        `json:"message,omitempty"`
	Streams    []ProbeStream `json:"streams"`
	DurationMS int64         `json:"duration_ms"`
}

var (
	reStream = regexp.MustCompile(`Stream #\d+:\d+[^:]*: (Video|Audio|Data): (\w+)(?: \(([^)]*)\))?(.*)`)
	reSize   = regexp.MustCompile(`, (\d{2,5})x(\d{2,5})`)
	reFPS    = regexp.MustCompile(`, ([\d.]+) (?:fps|tbr)`)
	reHz     = regexp.MustCompile(`, (\d+) Hz`)
	reURL    = regexp.MustCompile(`(?i)rtsps?://\S+`)
)

// Probe opens src the way the recorder would and reports its streams. The
// URL (with credentials) never appears in the result.
func Probe(ctx context.Context, ffmpeg string, src Source) ProbeResult {
	if src.Timeout <= 0 {
		src.Timeout = 8 * time.Second
	}
	transport := src.Transport
	if transport == "" {
		transport = "tcp"
	}
	ctx, cancel := context.WithTimeout(ctx, src.Timeout+5*time.Second)
	defer cancel()
	// -t 0 with the null muxer: open, read stream info, write nothing.
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin",
		"-rtsp_transport", transport, "-timeout", strconvI(src.Timeout.Microseconds()),
		"-i", src.URL, "-t", "0", "-f", "null", "-")
	bindToParent(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	res := ParseProbe(stderr.String())
	res.DurationMS = time.Since(start).Milliseconds()
	if ctx.Err() != nil && res.Error == "" {
		res.OK, res.Error, res.Message = false, "timeout", "the camera did not answer in time"
	}
	if err != nil && res.OK {
		res.OK = false
	}
	if !res.OK && res.Error == "" {
		res.Error = "error"
		if errors.Is(err, exec.ErrNotFound) {
			res.Message = "ffmpeg not found"
		}
	}
	return res
}

func strconvI(v int64) string { return strconv.FormatInt(v, 10) }

// ParseProbe interprets FFmpeg's stderr from Probe.
func ParseProbe(stderr string) ProbeResult {
	res := ProbeResult{Streams: []ProbeStream{}}
	var last string
	output := false // streams after "Output #0" are FFmpeg's own
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Output #") || strings.HasPrefix(line, "Stream mapping") {
			output = true
		}
		if m := reStream.FindStringSubmatch(line); m != nil && !output {
			st := ProbeStream{Kind: strings.ToLower(m[1]), Codec: m[2], Profile: m[3]}
			rest := m[4]
			if s := reSize.FindStringSubmatch(rest); s != nil {
				st.Width, _ = strconv.Atoi(s[1])
				st.Height, _ = strconv.Atoi(s[2])
			}
			if s := reFPS.FindStringSubmatch(rest); s != nil {
				st.FPS, _ = strconv.ParseFloat(s[1], 64)
			}
			if s := reHz.FindStringSubmatch(rest); s != nil {
				st.SampleRate, _ = strconv.Atoi(s[1])
			}
			res.Streams = append(res.Streams, st)
			continue
		}
		if !strings.HasPrefix(line, "Input #") && !strings.HasPrefix(line, "Metadata") && !strings.HasPrefix(line, "Duration") {
			last = line
		}
	}
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "401 unauthorized") || strings.Contains(low, "403 forbidden"):
		res.Error, res.Message = "auth_failed", "the camera rejected the username or password"
	case strings.Contains(low, "404 not found") || strings.Contains(low, "454 session not found"):
		res.Error, res.Message = "not_found", "the camera does not have that stream path"
	case strings.Contains(low, "connection refused") || strings.Contains(low, "no route to host") ||
		strings.Contains(low, "network is unreachable") || strings.Contains(low, "name or service not known") ||
		strings.Contains(low, "no such host") || strings.Contains(low, "failed to resolve"):
		res.Error, res.Message = "unreachable", "cannot connect to the camera (address or port)"
	case strings.Contains(low, "timed out") || strings.Contains(low, "timeout"):
		res.Error, res.Message = "timeout", "the camera did not answer in time"
	}
	if res.Error != "" {
		return res
	}
	hasVideo := false
	for _, s := range res.Streams {
		hasVideo = hasVideo || s.Kind == "video"
	}
	switch {
	case hasVideo:
		res.OK = true
	case len(res.Streams) > 0:
		res.Error, res.Message = "no_video", "the stream has no video"
	default:
		res.Error, res.Message = "error", reURL.ReplaceAllString(last, "<url>")
	}
	return res
}
