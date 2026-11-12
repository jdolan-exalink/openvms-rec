// Package exports produces evidence exports: an MP4 remuxed (never
// re-encoded) from the recorded segments of a camera over a UTC range, plus
// a manifest with SHA-256 of every source segment and of the output
// (PRD M07). Source segments are held against retention while exporting and
// their checksums are verified before use.
package exports

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/ingest/tsseg"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

// States.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Request is what the client asks for.
type Request struct {
	CameraID       string    `json:"camera_id"`
	Representation string    `json:"representation"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
}

// Export is the job as returned by the API.
type Export struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Progress   float64   `json:"progress"`
	Error      string    `json:"error,omitempty"`
	Request    Request   `json:"request"`
	ActualFrom time.Time `json:"actual_from,omitzero"`
	ActualTo   time.Time `json:"actual_to,omitzero"`
	SHA256     string    `json:"sha256,omitempty"`
	SizeBytes  int64     `json:"size_bytes,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Manifest is written next to the video inside the export archive.
type Manifest struct {
	ExportID         string           `json:"export_id"`
	RecorderVersion  string           `json:"recorder_version"`
	CreatedAt        time.Time        `json:"created_at"`
	CameraID         string           `json:"camera_id"`
	Representation   string           `json:"representation"`
	RequestedFrom    time.Time        `json:"requested_from"`
	RequestedTo      time.Time        `json:"requested_to"`
	ActualFrom       time.Time        `json:"actual_from"`
	ActualTo         time.Time        `json:"actual_to"`
	CutPrecision     string           `json:"cut_precision"`
	TimeSource       string           `json:"time_source"`
	Video            ManifestFile     `json:"video"`
	SourceSegments   []ManifestSource `json:"source_segments"`
	Gaps             []ManifestGap    `json:"gaps"`
	TranscodingVideo bool             `json:"transcoding_video"`
}

type ManifestFile struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type ManifestSource struct {
	SegmentID int64     `json:"segment_id"`
	VolumeID  string    `json:"volume_id"`
	Path      string    `json:"relative_path"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Origin    string    `json:"origin"`
	SHA256    string    `json:"sha256"`
	Verified  bool      `json:"checksum_verified"`
}

type ManifestGap struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitzero"`
	Reason string    `json:"reason"`
}

// Service runs exports as background jobs stored in the jobs table.
type Service struct {
	DB      *sql.DB
	Store   *segments.Store
	Root    func(volumeID string) (string, bool) // reachable volume roots
	Dir     string                               // output dir (data dir, not a video volume)
	FFmpeg  string
	Version string
	Log     *slog.Logger
	// MaxRange bounds one export.
	MaxRange time.Duration

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// Errors returned to the API.
var (
	ErrNotFound   = errors.New("export not found")
	ErrBadRequest = errors.New("invalid export request")
	ErrNotReady   = errors.New("export not ready")
)

// Create validates and queues an export. With an idempotency key, a repeat
// returns the existing job.
func (s *Service) Create(ctx context.Context, req Request, idemKey string) (Export, error) {
	if req.Representation == "" {
		req.Representation = "main"
	}
	maxR := s.MaxRange
	if maxR == 0 {
		maxR = 24 * time.Hour
	}
	if req.CameraID == "" || !req.To.After(req.From) || req.To.Sub(req.From) > maxR {
		return Export{}, fmt.Errorf("%w: camera_id required, to after from, range up to %s", ErrBadRequest, maxR)
	}
	req.From, req.To = req.From.UTC(), req.To.UTC()
	if idemKey != "" {
		var id string
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM jobs WHERE idempotency_key = ?`, "export:"+idemKey).Scan(&id)
		if err == nil {
			return s.Get(ctx, id)
		}
	}
	id := newID()
	now := time.Now().UTC()
	e := Export{ID: id, State: StateQueued, Request: req, CreatedAt: now, UpdatedAt: now}
	payload, _ := json.Marshal(e)
	var key any
	if idemKey != "" {
		key = "export:" + idemKey
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO jobs(id, kind, state, payload, idempotency_key, created_at, updated_at)
		VALUES (?, 'export', ?, ?, ?, ?, ?)`, id, StateQueued, string(payload), key, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Export{}, err
	}
	s.audit(ctx, "export.create", id, req)
	jctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]context.CancelFunc{}
	}
	s.cancels[id] = cancel
	s.mu.Unlock()
	go s.run(jctx, e)
	return e, nil
}

// Get returns an export job.
func (s *Service) Get(ctx context.Context, id string) (Export, error) {
	var payload string
	err := s.DB.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE id = ? AND kind = 'export'`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, ErrNotFound
	}
	if err != nil {
		return Export{}, err
	}
	var e Export
	return e, json.Unmarshal([]byte(payload), &e)
}

// Cancel stops a queued or running export.
func (s *Service) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cancels[id]
	if ok {
		c()
	}
	return ok
}

// Archive returns the path of a finished export archive.
func (s *Service) Archive(ctx context.Context, id string) (string, Export, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return "", e, err
	}
	if e.State != StateSucceeded {
		return "", e, ErrNotReady
	}
	s.audit(ctx, "export.download", id, nil)
	return filepath.Join(s.Dir, id, "export.zip"), e, nil
}

func (s *Service) save(e *Export) {
	e.UpdatedAt = time.Now().UTC()
	payload, _ := json.Marshal(e)
	if _, err := s.DB.Exec(`UPDATE jobs SET state = ?, progress = ?, error = ?, payload = ?, updated_at = ? WHERE id = ?`,
		e.State, e.Progress, e.Error, string(payload), e.UpdatedAt.UnixMilli(), e.ID); err != nil {
		s.Log.Error("save export", "id", e.ID, "err", err)
	}
}

func (s *Service) run(ctx context.Context, e Export) {
	defer func() {
		s.mu.Lock()
		delete(s.cancels, e.ID)
		s.mu.Unlock()
		s.Store.ReleaseHolder(context.Background(), "export:"+e.ID)
	}()
	e.State = StateRunning
	s.save(&e)
	err := s.export(ctx, &e)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		e.State, e.Error = StateCancelled, "cancelled"
		os.RemoveAll(filepath.Join(s.Dir, e.ID))
	case err != nil:
		e.State, e.Error = StateFailed, err.Error()
		os.RemoveAll(filepath.Join(s.Dir, e.ID))
		s.Log.Error("export failed", "id", e.ID, "err", err)
	default:
		e.State, e.Progress = StateSucceeded, 1
		s.Log.Info("export done", "id", e.ID, "bytes", e.SizeBytes)
	}
	s.save(&e)
}

func (s *Service) export(ctx context.Context, e *Export) error {
	req := e.Request
	segs, err := s.Store.Range(ctx, req.CameraID, req.Representation, req.From, req.To)
	if err != nil {
		return err
	}
	var usable []segments.Segment
	for _, g := range segs {
		if g.Status == segments.StatusFinal {
			usable = append(usable, g)
		}
	}
	if len(usable) == 0 {
		return errors.New("no finished recordings in the requested range")
	}
	holdUntil := time.Now().Add(2 * time.Hour)
	for _, g := range usable {
		if err := s.Store.Hold(ctx, g.ID, "export:"+e.ID, holdUntil); err != nil {
			return err
		}
	}
	dir := filepath.Join(s.Dir, e.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	// Verify sources and build the concat list.
	var list strings.Builder
	list.WriteString("ffconcat version 1.0\n")
	var sources []ManifestSource
	for i, g := range usable {
		root, ok := s.Root(g.VolumeID)
		if !ok {
			return fmt.Errorf("segment %d is on volume %s which is not available", g.ID, g.VolumeID)
		}
		p := filepath.Join(root, filepath.FromSlash(g.RelativePath))
		sum, err := fileSHA256(p)
		if err != nil {
			return fmt.Errorf("read segment %d: %w", g.ID, err)
		}
		if g.Checksum != "" && sum != g.Checksum {
			return fmt.Errorf("integrity check failed for segment %d (%s)", g.ID, g.RelativePath)
		}
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(filepath.ToSlash(p), "'", `'\''`))
		sources = append(sources, ManifestSource{SegmentID: g.ID, VolumeID: g.VolumeID, Path: g.RelativePath,
			Start: g.Start, End: g.End, Origin: g.Source, SHA256: sum, Verified: g.Checksum != ""})
		e.Progress = 0.5 * float64(i+1) / float64(len(usable))
		if i%10 == 0 {
			s.save(e)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	listPath := filepath.Join(dir, "concat.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o640); err != nil {
		return err
	}

	// Cut on keyframes: start at the last keyframe at or before `from`.
	first, last := usable[0], usable[len(usable)-1]
	offset := max(req.From.Sub(first.Start), 0)
	offset = snapToKeyframe(first.KeyframeInfo, offset)
	actualFrom := first.Start.Add(offset)
	actualTo := last.End
	if req.To.Before(actualTo) {
		actualTo = req.To
	}
	out := filepath.Join(dir, "video.mp4")
	ff := s.FFmpeg
	if ff == "" {
		ff = "ffmpeg"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-ss", secs(offset), "-i", listPath,
		"-t", secs(actualTo.Sub(actualFrom)), "-map", "0:v", "-map", "0:a?",
		"-c", "copy", "-movflags", "+faststart"}
	if strings.HasPrefix(first.Codec, "h265") {
		args = append(args, "-tag:v", "hvc1") // playable in Apple players
	}
	args = append(args, out)
	if b, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("remux: %v: %s", err, firstLine(string(b)))
	}
	os.Remove(listPath)
	e.Progress = 0.9
	s.save(e)

	vsum, err := fileSHA256(out)
	if err != nil {
		return err
	}
	fi, _ := os.Stat(out)
	gaps, _ := s.Store.Gaps(ctx, req.CameraID, req.Representation, actualFrom, actualTo)
	man := Manifest{
		ExportID: e.ID, RecorderVersion: s.Version, CreatedAt: time.Now().UTC(),
		CameraID: req.CameraID, Representation: req.Representation,
		RequestedFrom: req.From, RequestedTo: req.To, ActualFrom: actualFrom, ActualTo: actualTo,
		CutPrecision:   "start snapped to the previous keyframe; end within one GOP",
		TimeSource:     "recorder host clock (UTC) at packet arrival",
		Video:          ManifestFile{Name: "video.mp4", SHA256: vsum, SizeBytes: fi.Size()},
		SourceSegments: sources, Gaps: []ManifestGap{},
	}
	for _, g := range gaps {
		man.Gaps = append(man.Gaps, ManifestGap{Start: g.Start, End: g.End, Reason: g.Reason})
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := writeZip(filepath.Join(dir, "export.zip"), out, mb); err != nil {
		return err
	}
	os.Remove(out)
	zsum, _ := fileSHA256(filepath.Join(dir, "export.zip"))
	zfi, _ := os.Stat(filepath.Join(dir, "export.zip"))
	e.ActualFrom, e.ActualTo, e.SHA256, e.SizeBytes = actualFrom, actualTo, zsum, zfi.Size()
	return nil
}

// snapToKeyframe returns the largest keyframe offset <= offset.
func snapToKeyframe(kfJSON string, offset time.Duration) time.Duration {
	var kfs []tsseg.Keyframe
	if json.Unmarshal([]byte(kfJSON), &kfs) != nil || len(kfs) == 0 {
		return 0
	}
	best := time.Duration(0)
	for _, k := range kfs {
		d := time.Duration(k.OffsetMS) * time.Millisecond
		if d <= offset && d > best {
			best = d
		}
	}
	return best
}

func writeZip(dst, video string, manifest []byte) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	// Store (no compression): video does not compress and this keeps it fast.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "video.mp4", Method: zip.Store, Modified: time.Now()})
	if err == nil {
		var in *os.File
		if in, err = os.Open(video); err == nil {
			_, err = io.Copy(w, in)
			in.Close()
		}
	}
	if err == nil {
		if w, err = zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: time.Now()}); err == nil {
			_, err = w.Write(manifest)
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *Service) audit(ctx context.Context, action, target string, details any) {
	b, _ := json.Marshal(details)
	if details == nil {
		b = []byte("{}")
	}
	s.DB.ExecContext(ctx, `INSERT INTO audit_log(at, actor, action, target, details) VALUES (?, 'api', ?, ?, ?)`,
		time.Now().UnixMilli(), action, target, string(b))
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func secs(d time.Duration) string { return fmt.Sprintf("%.3f", d.Seconds()) }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "exp_" + hex.EncodeToString(b)
}

// RecoverJobs marks exports interrupted by a restart as failed and releases
// their holds. Call at startup before accepting requests.
func (s *Service) RecoverJobs(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM jobs WHERE kind = 'export' AND state IN ('queued', 'running')`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		e, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		e.State, e.Error = StateFailed, "interrupted by recorder restart"
		s.save(&e)
		s.Store.ReleaseHolder(ctx, "export:"+id)
		os.RemoveAll(filepath.Join(s.Dir, id))
	}
	return nil
}

// List returns recent jobs (all kinds), newest first.
func (s *Service) List(ctx context.Context, kind, state string, limit int) ([]map[string]any, error) {
	q := `SELECT id, kind, state, progress, error, created_at, updated_at FROM jobs WHERE 1=1`
	var args []any
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, k, st, er string
		var p float64
		var c, u int64
		if err := rows.Scan(&id, &k, &st, &p, &er, &c, &u); err != nil {
			return nil, err
		}
		j := map[string]any{"id": id, "kind": k, "state": st, "progress": p,
			"created_at": time.UnixMilli(c).UTC(), "updated_at": time.UnixMilli(u).UTC()}
		if er != "" {
			j["error"] = er
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
