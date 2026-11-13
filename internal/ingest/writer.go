// Package ingest records camera streams into segments on the storage volumes.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest/tsseg"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/placement"
)

// TmpDir holds segments being written, inside each volume root so the final
// rename is atomic (same filesystem).
const TmpDir = ".tmp"

// VolumeChooser is implemented by storage.Manager.
type VolumeChooser interface {
	Choose(cameraID string, sizeBytes uint64) (placement.Decision, string, error)
	MarkFailed(volumeID string, err error)
}

// Index is the subset of segments.Store used by ingest.
type Index interface {
	Begin(ctx context.Context, seg segments.Segment) (int64, error)
	Finalize(ctx context.Context, id int64, end time.Time, size uint64, checksum, keyframes string) error
	MarkCorrupt(ctx context.Context, id int64, size uint64) error
	OpenGap(ctx context.Context, cameraID, rep string, start time.Time, reason string) (int64, error)
	CloseGap(ctx context.Context, id int64, end time.Time) error
}

// Sink places each segment on a volume, writes it to a temp file, and on
// close fsyncs, renames atomically into place and finalizes the index row.
// When no volume can take a segment it records a gap and discards the data
// instead of writing anywhere else.
type Sink struct {
	CameraID       string
	Representation string
	Volumes        VolumeChooser
	Index          Index
	Log            *slog.Logger
	// EstimateBytes is the expected size of the next segment, used to check
	// headroom before writing. Updated from observed segment sizes.
	EstimateBytes uint64

	mu        sync.Mutex
	gapID     int64  // open gap, 0 = none
	gapReason string // reason of the open gap
	metrics   *Metrics
}

// RelativePath is the stable layout of segment files under a volume root.
func RelativePath(cameraID, rep string, start time.Time) string {
	t := start.UTC()
	return path.Join(cameraID, rep, t.Format("2006/01/02/15"),
		fmt.Sprintf("%s_%s_%s.ts", cameraID, rep, t.Format("20060102T150405.000Z")))
}

// TempPath is where segment id is written before finalization.
func TempPath(root string, id int64) string {
	return filepath.Join(root, TmpDir, strconv.FormatInt(id, 10)+".ts")
}

// OpenGap records a hole starting at t (stream lost, no volume...). It is
// closed when the next segment starts.
//
// A "connecting" gap is provisional: a more specific reason (stream_lost,
// no_volume...) closes it and opens its own gap from t.
func (s *Sink) OpenGap(t time.Time, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gapID != 0 {
		if s.gapReason != "connecting" || reason == "connecting" {
			return
		}
		if err := s.Index.CloseGap(context.Background(), s.gapID, t); err != nil {
			s.Log.Error("close gap", "camera", s.CameraID, "err", err)
		}
		s.gapID = 0
	}
	id, err := s.Index.OpenGap(context.Background(), s.CameraID, s.Representation, t, reason)
	if err != nil {
		s.Log.Error("open gap", "camera", s.CameraID, "err", err)
		return
	}
	s.gapID, s.gapReason = id, reason
}

func (s *Sink) closeGap(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gapID == 0 {
		return
	}
	if err := s.Index.CloseGap(context.Background(), s.gapID, t); err != nil {
		s.Log.Error("close gap", "camera", s.CameraID, "err", err)
	}
	s.gapID = 0
}

// Open implements tsseg.Sink.
func (s *Sink) Open(si tsseg.StartInfo) (tsseg.Writer, error) {
	ctx := context.Background()
	est := s.EstimateBytes
	if est == 0 {
		est = 64 << 20
	}
	dec, root, err := s.Volumes.Choose(s.CameraID, est)
	if errors.Is(err, placement.ErrNoEligibleVolume) {
		s.Log.Error("no volume for segment; recording gap", "camera", s.CameraID, "err", err)
		s.OpenGap(si.Start, "no_volume")
		return &discardWriter{}, nil
	}
	if err != nil {
		return nil, err
	}
	if dec.Fallback {
		s.Log.Warn("camera fell back to a non-assigned volume", "camera", s.CameraID, "volume", dec.VolumeID, "reason", dec.Reason)
	}
	rel := RelativePath(s.CameraID, s.Representation, si.Start)
	id, err := s.Index.Begin(ctx, segments.Segment{
		CameraID: s.CameraID, RepresentationID: s.Representation, Start: si.Start,
		Codec: codecs(si), VolumeID: dec.VolumeID, RelativePath: rel, Source: segments.SourceLiveIngest,
	})
	if err != nil {
		return nil, err
	}
	tmp := TempPath(root, id)
	if err := os.MkdirAll(filepath.Dir(tmp), 0o750); err != nil {
		s.failVolume(dec.VolumeID, id, 0, err)
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		s.failVolume(dec.VolumeID, id, 0, err)
		return nil, err
	}
	s.closeGap(si.Start)
	return &fileWriter{sink: s, id: id, volumeID: dec.VolumeID, root: root, rel: rel, tmp: tmp, f: f, h: sha256.New(), start: si.Start}, nil
}

func (s *Sink) failVolume(volumeID string, id int64, size uint64, err error) {
	s.Volumes.MarkFailed(volumeID, err)
	if cerr := s.Index.MarkCorrupt(context.Background(), id, size); cerr != nil {
		s.Log.Error("mark corrupt", "segment", id, "err", cerr)
	}
}

func codecs(si tsseg.StartInfo) string {
	if si.AudioCodec == "" {
		return si.VideoCodec
	}
	return si.VideoCodec + "+" + si.AudioCodec
}

type fileWriter struct {
	sink     *Sink
	id       int64
	volumeID string
	root     string
	rel      string
	tmp      string
	f        *os.File
	h        hash.Hash
	n        int64
	start    time.Time
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	if err != nil {
		return n, err
	}
	return n, nil
}

func (w *fileWriter) Close(e tsseg.EndInfo) error {
	s := w.sink
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		s.failVolume(w.volumeID, w.id, uint64(w.n), err)
		return fmt.Errorf("fsync segment: %w", err)
	}
	if err := w.f.Close(); err != nil {
		s.failVolume(w.volumeID, w.id, uint64(w.n), err)
		return err
	}
	final := filepath.Join(w.root, filepath.FromSlash(w.rel))
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		s.failVolume(w.volumeID, w.id, uint64(w.n), err)
		return err
	}
	if err := os.Rename(w.tmp, final); err != nil {
		s.failVolume(w.volumeID, w.id, uint64(w.n), err)
		return err
	}
	syncDir(filepath.Dir(final))
	sum := hex.EncodeToString(w.h.Sum(nil))
	if err := s.Index.Finalize(context.Background(), w.id, e.End, uint64(w.n), sum, e.KeyframesJSON()); err != nil {
		return err
	}
	// Learn segment size for headroom checks (+25 % margin for bitrate swings).
	s.EstimateBytes = uint64(w.n) + uint64(w.n)/4
	if s.metrics != nil {
		s.metrics.segmentDone(w.n, e.End.Sub(w.start), e.End)
	}
	return nil
}

func (w *fileWriter) Abort(cause error) {
	w.f.Close()
	s := w.sink
	if isIOError(cause) {
		s.failVolume(w.volumeID, w.id, uint64(w.n), cause)
		return
	}
	// Stream ended abnormally (camera dropped, ffmpeg died): the temp file is
	// valid up to its last complete packet, so keep it as a short segment.
	if err := finalizeTemp(s.Index, w.id, w.root, w.rel, w.tmp, time.Now()); err != nil {
		s.Log.Error("finalize aborted segment", "segment", w.id, "err", err)
	}
}

// isIOError distinguishes disk failures (switch volume) from stream failures.
func isIOError(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe) || errors.Is(err, os.ErrClosed)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) Close(tsseg.EndInfo) error   { return nil }
func (discardWriter) Abort(error)                 {}
