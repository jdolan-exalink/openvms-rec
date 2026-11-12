package tsseg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture generates a real MPEG-TS stream with FFmpeg (skips when absent).
func fixture(t *testing.T, vcodec string, seconds int, withAudio bool) []byte {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(t.TempDir(), "in.ts")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440")
	}
	args = append(args, "-t", fmt.Sprint(seconds), "-c:v", vcodec, "-g", "25", "-pix_fmt", "yuv420p")
	if vcodec == "libx265" {
		args = append(args, "-x265-params", "log-level=error")
	}
	if withAudio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, "-f", "mpegts", "-y", out)
	if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot encode %s: %v %s", vcodec, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeClock advances 1 ms per call so durations map to packet counts.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

type memSeg struct {
	start   StartInfo
	end     EndInfo
	buf     bytes.Buffer
	closed  bool
	aborted error
}

type memSink struct{ segs []*memSeg }

func (m *memSink) Open(si StartInfo) (Writer, error) {
	s := &memSeg{start: si}
	m.segs = append(m.segs, s)
	return s, nil
}

func (s *memSeg) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *memSeg) Close(e EndInfo) error       { s.end, s.closed = e, true; return nil }
func (s *memSeg) Abort(err error)             { s.aborted = err }

func pid(pkt []byte) int { return int(pkt[1]&0x1f)<<8 | int(pkt[2]) }

func TestSegmentsStartWithTablesAndKeyframe(t *testing.T) {
	in := fixture(t, "libx264", 10, true)
	clk := &fakeClock{t: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	seg := &Segmenter{Target: 300 * time.Millisecond, Now: clk.now}
	sink := &memSink{}
	if err := seg.Run(context.Background(), bytes.NewReader(in), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.segs) < 3 {
		t.Fatalf("only %d segments", len(sink.segs))
	}
	var total int
	for i, s := range sink.segs {
		if !s.closed || s.aborted != nil {
			t.Fatalf("seg %d not closed cleanly", i)
		}
		if s.start.VideoCodec != "h264" || s.start.AudioCodec != "aac" {
			t.Fatalf("codecs %+v", s.start)
		}
		b := s.buf.Bytes()
		if len(b)%PacketSize != 0 || int64(len(b)) != s.end.Bytes {
			t.Fatalf("seg %d size %d vs %d", i, len(b), s.end.Bytes)
		}
		if pid(b[0:]) != 0 || pid(b[PacketSize:]) != seg.pmtPID {
			t.Fatalf("seg %d does not start with PAT+PMT", i)
		}
		if first := b[2*PacketSize:]; pid(first) != seg.videoPID || !randomAccess(first) {
			t.Fatalf("seg %d does not start on a keyframe", i)
		}
		if len(s.end.Keyframes) == 0 || s.end.Keyframes[0].OffsetBytes != 2*PacketSize {
			t.Fatalf("seg %d keyframes %+v", i, s.end.Keyframes)
		}
		if !s.end.End.After(s.start.Start) {
			t.Fatalf("seg %d times", i)
		}
		if i > 0 && s.start.Start.Sub(sink.segs[i-1].start.Start) < seg.Target {
			t.Fatalf("seg %d shorter than target", i)
		}
		total += len(b) - 2*PacketSize
	}
	// Nothing after the first keyframe is lost (only leading packets before it).
	if total > len(in) || total < len(in)*9/10 {
		t.Fatalf("payload %d of %d bytes", total, len(in))
	}
}

func TestEachSegmentDecodesStandalone(t *testing.T) {
	for _, codec := range []string{"libx264", "libx265"} {
		t.Run(codec, func(t *testing.T) {
			in := fixture(t, codec, 6, false)
			ffprobe, err := exec.LookPath("ffprobe")
			if err != nil {
				t.Skip("ffprobe not installed")
			}
			clk := &fakeClock{t: time.Unix(0, 0)}
			sink := &memSink{}
			if err := (&Segmenter{Target: 100 * time.Millisecond, Now: clk.now}).Run(context.Background(), bytes.NewReader(in), sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.segs) < 2 {
				t.Fatalf("segments=%d", len(sink.segs))
			}
			dir := t.TempDir()
			for i, s := range sink.segs {
				p := filepath.Join(dir, fmt.Sprintf("%d.ts", i))
				os.WriteFile(p, s.buf.Bytes(), 0o644)
				out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
					"-show_entries", "stream=codec_name,nb_read_frames", "-of", "csv=p=0", p).CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) == "" || strings.Contains(string(out), "error") {
					t.Fatalf("segment %d not decodable: %v %s", i, err, out)
				}
				if !strings.HasPrefix(strings.TrimSpace(string(out)), map[string]string{"libx264": "h264", "libx265": "hevc"}[codec]) {
					t.Fatalf("segment %d: %s", i, out)
				}
			}
		})
	}
}

func TestTruncatedInputClosesLastSegment(t *testing.T) {
	in := fixture(t, "libx264", 4, false)
	cut := in[:len(in)-PacketSize/2] // power cut mid-packet
	sink := &memSink{}
	clk := &fakeClock{t: time.Unix(0, 0)}
	if err := (&Segmenter{Target: time.Hour, Now: clk.now}).Run(context.Background(), bytes.NewReader(cut), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.segs) != 1 || !sink.segs[0].closed || sink.segs[0].buf.Len()%PacketSize != 0 {
		t.Fatalf("%+v", sink.segs)
	}
}

func TestGarbageResync(t *testing.T) {
	in := fixture(t, "libx264", 2, false)
	noisy := append([]byte("garbage\x47\x00junk"), in...)
	sink := &memSink{}
	clk := &fakeClock{t: time.Unix(0, 0)}
	if err := (&Segmenter{Target: time.Hour, Now: clk.now}).Run(context.Background(), bytes.NewReader(noisy), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.segs) != 1 || sink.segs[0].buf.Len() < len(in)*9/10 {
		t.Fatal("did not resync")
	}
}

func TestNoVideo(t *testing.T) {
	err := (&Segmenter{}).Run(context.Background(), bytes.NewReader(nil), &memSink{})
	if !errors.Is(err, ErrNoVideo) {
		t.Fatalf("err=%v", err)
	}
}

type failSink struct{ memSink }

type failSeg struct{ *memSeg }

func (f failSeg) Write([]byte) (int, error) { return 0, errors.New("disk gone") }

func (f *failSink) Open(si StartInfo) (Writer, error) {
	w, _ := f.memSink.Open(si)
	return failSeg{w.(*memSeg)}, nil
}

func TestWriteErrorAbortsSegment(t *testing.T) {
	in := fixture(t, "libx264", 2, false)
	sink := &failSink{}
	err := (&Segmenter{Target: time.Hour}).Run(context.Background(), bytes.NewReader(in), sink)
	if err == nil || sink.segs[0].aborted == nil || sink.segs[0].closed {
		t.Fatalf("err=%v seg=%+v", err, sink.segs[0])
	}
}
