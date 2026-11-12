// Package segments is the SQLite index of recorded segments (ADR-001 model).
package segments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/storage/retention"
)

// Status values.
const (
	StatusWriting  = "writing"
	StatusFinal    = "final"
	StatusCorrupt  = "corrupt"
	StatusDeleting = "deleting"
	StatusDeleted  = "deleted"
)

// Source values (ADR-003).
const (
	SourceLiveIngest  = "live_ingest"
	SourceRecoveredSD = "recovered_sd"
	SourceRemoteOnly  = "remote_only"
)

// Segment mirrors the segments table.
type Segment struct {
	ID               int64
	CameraID         string
	RepresentationID string
	Start            time.Time
	End              time.Time // zero while writing
	Codec            string
	KeyframeInfo     string
	VolumeID         string
	RelativePath     string // forward slashes
	SizeBytes        uint64
	Checksum         string
	Status           string
	ProtectionUntil  time.Time
	Source           string
	Generation       int
}

// Store wraps the database.
type Store struct{ db *sql.DB }

// New returns a store on an opened, migrated database.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Begin registers a segment that ingest starts writing.
func (s *Store) Begin(ctx context.Context, seg Segment) (int64, error) {
	if seg.RepresentationID == "" {
		seg.RepresentationID = "main"
	}
	if seg.Source == "" {
		seg.Source = SourceLiveIngest
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO segments
		(camera_id, representation_id, start_utc, codec, keyframe_info, volume_id, relative_path, status, source, generation, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'writing', ?, 1, ?)`,
		seg.CameraID, seg.RepresentationID, ms(seg.Start), seg.Codec, seg.KeyframeInfo,
		seg.VolumeID, seg.RelativePath, seg.Source, ms(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("begin segment: %w", err)
	}
	return res.LastInsertId()
}

// Finalize records the end of a segment once its file is complete and synced.
func (s *Store) Finalize(ctx context.Context, id int64, end time.Time, size uint64, checksum, keyframes string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE segments
		SET end_utc = ?, size_bytes = ?, checksum = ?, keyframe_info = COALESCE(NULLIF(?, ''), keyframe_info), status = 'final'
		WHERE id = ? AND status = 'writing'`, ms(end), size, checksum, keyframes, id)
	return one(res, err, "finalize segment %d", id)
}

// Protect sets protection_until (evidence, user pin). Extending only: an
// earlier date never shortens existing protection.
func (s *Store) Protect(ctx context.Context, id int64, until time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE segments
		SET protection_until = MAX(COALESCE(protection_until, 0), ?) WHERE id = ?`, ms(until), id)
	return one(res, err, "protect segment %d", id)
}

// Hold blocks deletion while holder (export:<id>) reads the segment.
func (s *Store) Hold(ctx context.Context, id int64, holder string, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO segment_holds(segment_id, holder, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(segment_id, holder) DO UPDATE SET expires_at = excluded.expires_at`, id, holder, ms(until))
	return err
}

// Release removes a hold.
func (s *Store) Release(ctx context.Context, id int64, holder string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM segment_holds WHERE segment_id = ? AND holder = ?`, id, holder)
	return err
}

// RecorderBytes is the indexed size of live data on a volume (everything not
// yet deleted), used for the effective-limit computation.
func (s *Store) RecorderBytes(ctx context.Context, volumeID string) (uint64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(size_bytes) FROM segments
		WHERE volume_id = ? AND status IN ('writing', 'final', 'corrupt', 'deleting')`, volumeID).Scan(&n)
	return uint64(n.Int64), err
}

// RetentionCandidates lists a volume's segments for the retention planner.
func (s *Store) RetentionCandidates(ctx context.Context, volumeID string, now time.Time) ([]retention.Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.camera_id, s.representation_id, s.start_utc,
			COALESCE(s.end_utc, s.start_utc), s.size_bytes, COALESCE(s.protection_until, 0), s.status,
			EXISTS (SELECT 1 FROM segment_holds h WHERE h.segment_id = s.id AND h.expires_at > ?)
		FROM segments s
		WHERE s.volume_id = ? AND s.status IN ('writing', 'final', 'corrupt')
		ORDER BY s.start_utc, s.id`, ms(now), volumeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retention.Segment
	for rows.Next() {
		var r retention.Segment
		var start, end, prot int64
		var status string
		if err := rows.Scan(&r.ID, &r.CameraID, &r.Representation, &start, &end, &r.SizeBytes, &prot, &status, &r.Held); err != nil {
			return nil, err
		}
		r.Start, r.End = fromMS(start), fromMS(end)
		if prot > 0 {
			r.ProtectedUntil = fromMS(prot)
		}
		r.Writing = status == StatusWriting
		out = append(out, r)
	}
	return out, rows.Err()
}

// BeginDelete implements retention.Store. The eligibility re-check and the
// status change happen in one statement, so a protection or hold added after
// planning wins.
func (s *Store) BeginDelete(ctx context.Context, id int64, now time.Time) (string, error) {
	var rel string
	err := s.db.QueryRowContext(ctx, `UPDATE segments SET status = 'deleting'
		WHERE id = ? AND status IN ('final', 'corrupt')
		  AND COALESCE(protection_until, 0) <= ?
		  AND NOT EXISTS (SELECT 1 FROM segment_holds h WHERE h.segment_id = segments.id AND h.expires_at > ?)
		RETURNING relative_path`, id, ms(now), ms(now)).Scan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", retention.ErrNoLongerEligible
	}
	return rel, err
}

// FinishDelete implements retention.Store. The row is kept as 'deleted' for
// audit and timeline gap reporting; a later compaction job may purge it.
func (s *Store) FinishDelete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE segments SET status = 'deleted' WHERE id = ? AND status = 'deleting'`, id)
	return one(res, err, "finish delete %d", id)
}

// PendingDeletes returns rows left in 'deleting' by a crash, for startup reconciliation.
func (s *Store) PendingDeletes(ctx context.Context, volumeID string) ([]Segment, error) {
	return s.query(ctx, `WHERE volume_id = ? AND status = 'deleting' ORDER BY id`, volumeID)
}

// Range lists non-deleted segments of a camera overlapping [from, to), in
// chronological order across all volumes (ST-07). Segments on missing
// volumes are still returned; callers join with volume state to report them
// as unavailable instead of hiding them.
func (s *Store) Range(ctx context.Context, cameraID, rep string, from, to time.Time) ([]Segment, error) {
	return s.query(ctx, `WHERE camera_id = ? AND representation_id = ? AND status IN ('writing', 'final')
		AND start_utc < ? AND COALESCE(end_utc, ?) > ? ORDER BY start_utc, id`,
		cameraID, rep, ms(to), ms(to), ms(from))
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, camera_id, representation_id, start_utc, COALESCE(end_utc, 0),
		codec, keyframe_info, volume_id, relative_path, size_bytes, checksum, status,
		COALESCE(protection_until, 0), source, generation FROM segments `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var g Segment
		var start, end, prot int64
		if err := rows.Scan(&g.ID, &g.CameraID, &g.RepresentationID, &start, &end, &g.Codec, &g.KeyframeInfo,
			&g.VolumeID, &g.RelativePath, &g.SizeBytes, &g.Checksum, &g.Status, &prot, &g.Source, &g.Generation); err != nil {
			return nil, err
		}
		g.Start = fromMS(start)
		if end > 0 {
			g.End = fromMS(end)
		}
		if prot > 0 {
			g.ProtectionUntil = fromMS(prot)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func one(res sql.Result, err error, format string, a ...any) error {
	if err != nil {
		return fmt.Errorf(format+": %w", append(a, err)...)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf(format+": no matching segment in expected state", a...)
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
