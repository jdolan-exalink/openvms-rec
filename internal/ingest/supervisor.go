package ingest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest/tsseg"
)

// Audio handling. MPEG-TS cannot carry every camera audio codec (G.711 is
// common and unsupported); video is always copied.
const (
	AudioCopy = "copy"
	AudioAAC  = "aac"  // transcode audio only, cheap; video untouched
	AudioNone = "none" // drop audio
	AudioAuto = "auto" // copy, switch to aac if the muxer rejects the codec
)

// Source describes how to pull a stream.
type Source struct {
	// URL is the RTSP URL including credentials. It is never logged; use
	// Redact for messages.
	URL       string
	Transport string // tcp (default) or udp
	Audio     string // AudioAuto by default
	// Timeout aborts a stalled connection (no data). Default 15 s.
	Timeout time.Duration
}

// Redact removes credentials from an RTSP URL for logs and the API.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	hadUser := u.User != nil
	u.User = nil
	u.RawQuery = ""
	out := u.String()
	if hadUser {
		out = strings.Replace(out, "://", "://***@", 1)
	}
	return out
}

// FFmpegArgs builds a remux-only pipeline: RTSP in, MPEG-TS on stdout.
func FFmpegArgs(src Source, audio string) []string {
	transport := src.Transport
	if transport == "" {
		transport = "tcp"
	}
	timeout := src.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-rtsp_transport", transport,
		// SETUP only the tracks we record. Some cameras (seen with a
		// Provision-ISR unit) answer 453 Not Enough Bandwidth when asked to
		// also set up their ONVIF metadata track.
		"-allowed_media_types", mediaTypes(audio),
		"-timeout", fmt.Sprint(timeout.Microseconds()), // socket I/O timeout (µs)
		"-fflags", "+genpts+discardcorrupt",
		"-i", src.URL,
		"-map", "0:v:0",
	}
	switch audio {
	case AudioNone:
	case AudioAAC:
		args = append(args, "-map", "0:a:0?", "-c:a", "aac", "-b:a", "64k")
	default:
		args = append(args, "-map", "0:a:0?", "-c:a", "copy")
	}
	return append(args, "-c:v", "copy", "-f", "mpegts", "-mpegts_flags", "+resend_headers", "pipe:1")
}

func mediaTypes(audio string) string {
	if audio == AudioNone {
		return "video"
	}
	return "video+audio"
}

// State of a camera recorder.
type State string

const (
	StateStarting  State = "starting"
	StateRecording State = "recording"
	StateBackoff   State = "backoff"
	StateStopped   State = "stopped"
)

// Stats are per-camera ingest counters (PRD M03).
type Stats struct {
	State       State
	LastError   string
	Reconnects  int64
	Segments    int64
	Bytes       int64
	LastWrite   time.Time
	BitrateKbps float64 // last segment
	Audio       string
	PID         int // FFmpeg process while a session runs, else 0
}

// Metrics guards Stats for concurrent readers (API) and the writer (ingest).
type Metrics struct {
	mu sync.Mutex
	s  Stats
}

// Snapshot returns a copy safe to serialize.
func (m *Metrics) Snapshot() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *Metrics) set(fn func(*Stats)) {
	m.mu.Lock()
	fn(&m.s)
	m.mu.Unlock()
}

func (m *Metrics) segmentDone(n int64, d time.Duration, end time.Time) {
	m.set(func(m *Stats) {
		m.Segments++
		m.Bytes += n
		m.LastWrite = end
		if d > 0 {
			m.BitrateKbps = float64(n*8) / d.Seconds() / 1000
		}
	})
}

// Recorder supervises one FFmpeg process per camera stream and restarts it
// with exponential backoff and jitter. Recording continues regardless of
// analytics; a lost stream becomes an explicit gap.
type Recorder struct {
	Source  Source
	FFmpeg  string // binary path; default "ffmpeg"
	Segment time.Duration
	Sink    *Sink
	Log     *slog.Logger
	// Args overrides FFmpegArgs (tests use a synthetic source).
	Args func(audio string) []string
	// Backoff bounds.
	MinBackoff, MaxBackoff time.Duration

	Metrics Metrics
	running atomic.Bool
}

// Run blocks until ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)
	r.Sink.metrics = &r.Metrics
	if r.FFmpeg == "" {
		r.FFmpeg = "ffmpeg"
	}
	if r.MinBackoff <= 0 {
		r.MinBackoff = time.Second
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = time.Minute
	}
	audio := r.Source.Audio
	if audio == "" {
		audio = AudioAuto
	}
	effAudio := audio
	if audio == AudioAuto {
		effAudio = AudioCopy
	}
	// Until the first segment arrives (RTSP connect, first keyframe) nothing
	// is recorded: say so instead of leaving an unexplained hole.
	r.Sink.OpenGap(time.Now(), "connecting")
	attempt := 0
	for ctx.Err() == nil {
		r.Metrics.set(func(m *Stats) { m.State, m.Audio = StateStarting, effAudio })
		started := time.Now()
		stderr, err := r.session(ctx, effAudio)
		if ctx.Err() != nil {
			break
		}
		if audio == AudioAuto && effAudio == AudioCopy && (audioUnsupported(stderr) || errors.Is(err, errAudioNotPlayable)) {
			r.Log.Warn("camera audio codec not supported in MPEG-TS; transcoding audio to AAC (video still copied)", "camera", r.Sink.CameraID)
			effAudio = AudioAAC
			continue
		}
		msg := r.redact(firstLine(stderr, err))
		r.Sink.OpenGap(time.Now(), "stream_lost")
		if time.Since(started) > 2*r.MaxBackoff {
			attempt = 0 // it was a healthy session; restart fast
		}
		wait := backoff(attempt, r.MinBackoff, r.MaxBackoff)
		attempt++
		r.Metrics.set(func(m *Stats) { m.State, m.LastError = StateBackoff, msg; m.Reconnects++ })
		r.Log.Warn("stream ended; reconnecting", "camera", r.Sink.CameraID, "source", Redact(r.Source.URL), "err", msg, "retry_in", wait.Round(100*time.Millisecond).String())
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	r.Sink.OpenGap(time.Now(), "recorder_stopped")
	r.Metrics.set(func(m *Stats) { m.State = StateStopped })
}

// session runs FFmpeg once and segments its output. It returns FFmpeg's
// stderr (bounded) and the error that ended the session.
func (r *Recorder) session(ctx context.Context, audio string) (string, error) {
	args := FFmpegArgs(r.Source, audio)
	if r.Args != nil {
		args = r.Args(audio)
	}
	cmd := exec.CommandContext(ctx, r.FFmpeg, args...)
	cmd.WaitDelay = 5 * time.Second
	bindToParent(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var errBuf strings.Builder
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := cmd.Process.Pid
	r.Metrics.set(func(m *Stats) { m.PID = pid })
	defer r.Metrics.set(func(m *Stats) { m.PID = 0 })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(errPipe)
		for sc.Scan() {
			if errBuf.Len() < 8<<10 {
				errBuf.WriteString(sc.Text())
				errBuf.WriteByte('\n')
			}
		}
	}()
	r.Metrics.set(func(m *Stats) { m.State, m.LastError = StateRecording, "" })
	seg := &tsseg.Segmenter{Target: r.Segment}
	var sink tsseg.Sink = r.Sink
	if r.Source.Audio == "" || r.Source.Audio == AudioAuto {
		if audio == AudioCopy {
			sink = audioGuard{r.Sink}
		}
	}
	segErr := seg.Run(ctx, stdout, sink)
	if segErr != nil && !errors.Is(segErr, context.Canceled) {
		// Disk or index failure: stop FFmpeg so the next session starts on a
		// keyframe in a healthy volume.
		cmd.Process.Kill()
	}
	io.Copy(io.Discard, stdout)
	wg.Wait()
	waitErr := cmd.Wait()
	if segErr != nil {
		return errBuf.String(), segErr
	}
	if waitErr == nil {
		waitErr = errors.New("stream ended")
	}
	return errBuf.String(), waitErr
}

func (r *Recorder) redact(s string) string {
	u, err := url.Parse(r.Source.URL)
	if err == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			s = strings.ReplaceAll(s, p, "***")
		}
		s = strings.ReplaceAll(s, r.Source.URL, Redact(r.Source.URL))
	}
	return s
}

// errAudioNotPlayable means the copied audio landed in the TS as an opaque
// data stream (typically G.711): the session restarts with AAC transcoding.
var errAudioNotPlayable = errors.New("camera audio codec cannot be stored as-is in MPEG-TS")

// audioGuard rejects the first segment when audio would be stored unplayable.
type audioGuard struct{ *Sink }

func (g audioGuard) Open(si tsseg.StartInfo) (tsseg.Writer, error) {
	if si.AudioCodec == "" && si.OtherStreams > 0 {
		return nil, errAudioNotPlayable
	}
	return g.Sink.Open(si)
}

func audioUnsupported(stderr string) bool {
	for _, m := range []string{"not currently supported in container", "Could not find tag for codec", "codec not currently supported"} {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

func firstLine(stderr string, err error) string {
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	if err != nil {
		return err.Error()
	}
	return "stream ended"
}

// backoff returns min·2^attempt capped at max, with ±50 % jitter so many
// cameras behind one failed switch don't reconnect in lockstep.
func backoff(attempt int, lo, hi time.Duration) time.Duration {
	d := lo << min(attempt, 16)
	if d > hi || d <= 0 {
		d = hi
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}
