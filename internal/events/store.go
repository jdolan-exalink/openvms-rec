// Package events stores camera analytics and detection events (from ONVIF
// PullPoint and Provision-ISR HTTP push) and verified camera capabilities.
package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

// Event mirrors the Event schema in api/openapi.yaml.
type Event struct {
	EventID      string      `json:"event_id"`
	CameraID     string      `json:"camera_id"`
	TimestampUTC time.Time   `json:"timestamp_utc"`
	Type         string      `json:"type"`
	Class        string      `json:"class,omitempty"`
	Confidence   *float64    `json:"confidence,omitempty"`
	BBox         []float64   `json:"bbox,omitempty"`
	Detections   []Detection `json:"detections,omitempty"`
	Zone         string      `json:"zone,omitempty"`
	Snapshot     string      `json:"snapshot,omitempty"`
	Source       string      `json:"source"`
	RawRef       string      `json:"raw_ref,omitempty"`
	SnapshotData []byte      `json:"-"` // transient raw frame from push; never stored in SQLite
}

// Store persists events and camera capabilities in SQLite.
type Store struct {
	db            *sql.DB
	MinConfidence float64
	OnInsert      func(ev Event)
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// NewEventID generates a unique event identifier.
func NewEventID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}

// Insert stores an event idempotently using dedupeKey. Returns true if a new
// row was inserted.
func (s *Store) Insert(ctx context.Context, ev Event, dedupeKey string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if !NormalizeEvent(&ev, s.MinConfidence) {
		return false, nil
	}
	if ev.EventID == "" {
		ev.EventID = NewEventID()
	}
	if ev.TimestampUTC.IsZero() {
		ev.TimestampUTC = time.Now().UTC()
	}
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s:%s:%s:%d", ev.Source, ev.CameraID, ev.Type, ev.TimestampUTC.UnixMilli())
	}
	var conf sql.NullFloat64
	if ev.Confidence != nil {
		conf = sql.NullFloat64{Float64: *ev.Confidence, Valid: true}
	}
	var bbox sql.NullString
	if len(ev.BBox) == 4 {
		b, _ := json.Marshal(ev.BBox)
		bbox = sql.NullString{String: string(b), Valid: true}
	}
	detectionsJSON := ""
	if len(ev.Detections) > 0 {
		b, _ := json.Marshal(ev.Detections)
		detectionsJSON = string(b)
	}
	now := time.Now().UTC().UnixMilli()
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO events
		(id, camera_id, timestamp_utc, type, class, confidence, bbox, detections, zone, snapshot_ref, source, raw_ref, dedupe_key, protected, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		ev.EventID, ev.CameraID, ev.TimestampUTC.UTC().UnixMilli(), ev.Type, ev.Class, conf, bbox, detectionsJSON,
		nullStr(ev.Zone), nullStr(ev.Snapshot), ev.Source, nullStr(ev.RawRef), dedupeKey, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 && ev.Snapshot == "" && s.OnInsert != nil {
		s.OnInsert(ev)
	}
	return n > 0, nil
}

// GetSnapshotRef returns the relative snapshot_ref for eventID, if any.
func (s *Store) GetSnapshotRef(ctx context.Context, eventID string) (string, error) {
	if s == nil || s.db == nil {
		return "", nil
	}
	var ref sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_ref FROM events WHERE id = ?`, eventID).Scan(&ref)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return ref.String, nil
}

// Query filters events by optional cameraID, time range, type, class, source, and minimum confidence.
type Query struct {
	CameraID      string
	From          time.Time
	To            time.Time
	Type          string
	Class         string
	Source        string
	MinConfidence *float64
	Limit         int
	Cursor        string
}

// List returns matching events ordered newest-first, plus next_cursor if more exist.
func (s *Store) List(ctx context.Context, q Query) ([]Event, string, error) {
	if s == nil || s.db == nil {
		return []Event{}, "", nil
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var where []string
	var args []any
	if q.CameraID != "" {
		where = append(where, "camera_id = ?")
		args = append(args, q.CameraID)
	}
	if !q.From.IsZero() {
		where = append(where, "timestamp_utc >= ?")
		args = append(args, q.From.UTC().UnixMilli())
	}
	if !q.To.IsZero() {
		where = append(where, "timestamp_utc <= ?")
		args = append(args, q.To.UTC().UnixMilli())
	}
	var primaryConds []string
	var primaryArgs []any
	var detConds []string
	var detArgs []any
	if q.Type != "" {
		if q.Type == "line_crossing" || q.Type == "intrusion" {
			primaryConds = append(primaryConds, "(type = ? OR zone = ?)")
			primaryArgs = append(primaryArgs, q.Type, q.Type)
		} else {
			primaryConds = append(primaryConds, "type = ?")
			primaryArgs = append(primaryArgs, q.Type)
		}
		detConds = append(detConds, "json_extract(value, '$.type') = ?")
		detArgs = append(detArgs, q.Type)
	}
	if q.Class != "" {
		primaryConds = append(primaryConds, "LOWER(class) = LOWER(?)")
		primaryArgs = append(primaryArgs, q.Class)
		detConds = append(detConds, "LOWER(json_extract(value, '$.class')) = LOWER(?)")
		detArgs = append(detArgs, q.Class)
	}
	if q.Source != "" {
		where = append(where, "source = ?")
		args = append(args, q.Source)
	}
	if q.MinConfidence != nil {
		primaryConds = append(primaryConds, "confidence IS NOT NULL AND confidence >= ?")
		primaryArgs = append(primaryArgs, *q.MinConfidence)
		detConds = append(detConds, "json_extract(value, '$.confidence') >= ?")
		detArgs = append(detArgs, *q.MinConfidence)
	}
	if len(primaryConds) > 0 {
		pExpr := strings.Join(primaryConds, " AND ")
		dExpr := strings.Join(detConds, " AND ")
		where = append(where, "(("+pExpr+") OR (detections <> '' AND EXISTS (SELECT 1 FROM json_each(detections) WHERE "+dExpr+")))")
		args = append(args, primaryArgs...)
		args = append(args, detArgs...)
	}
	if q.Cursor != "" {
		if tsStr, idStr, ok := strings.Cut(q.Cursor, "|"); ok {
			if ts, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
				where = append(where, "(timestamp_utc < ? OR (timestamp_utc = ? AND id < ?))")
				args = append(args, ts, ts, idStr)
			}
		}
	}
	sqlStr := `SELECT id, camera_id, timestamp_utc, type, class, confidence, bbox, detections, zone, snapshot_ref, source, raw_ref FROM events`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY timestamp_utc DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out := make([]Event, 0)
	for rows.Next() {
		var ev Event
		var tsMs int64
		var conf sql.NullFloat64
		var bbox, dets, zone, snap, raw sql.NullString
		if err := rows.Scan(&ev.EventID, &ev.CameraID, &tsMs, &ev.Type, &ev.Class, &conf, &bbox, &dets, &zone, &snap, &ev.Source, &raw); err != nil {
			return nil, "", err
		}
		ev.TimestampUTC = time.UnixMilli(tsMs).UTC()
		if conf.Valid {
			v := conf.Float64
			ev.Confidence = &v
		}
		if bbox.Valid && bbox.String != "" {
			_ = json.Unmarshal([]byte(bbox.String), &ev.BBox)
		}
		if dets.Valid && dets.String != "" {
			_ = json.Unmarshal([]byte(dets.String), &ev.Detections)
		}
		if zone.Valid {
			ev.Zone = zone.String
		}
		if snap.Valid {
			ev.Snapshot = snap.String
		}
		if raw.Valid {
			ev.RawRef = raw.String
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var nextCursor string
	if len(out) > limit {
		last := out[limit-1]
		nextCursor = fmt.Sprintf("%d|%s", last.TimestampUTC.UnixMilli(), last.EventID)
		out = out[:limit]
	}
	return out, nextCursor, nil
}

// SaveCapabilities upserts capability matrix rows for an API camera in SQLite.
func (s *Store) SaveCapabilities(ctx context.Context, cameraID string, caps []onvif.Capability) error {
	if s == nil || s.db == nil || cameraID == "" || len(caps) == 0 {
		return nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cameras WHERE id = ?`, cameraID).Scan(&exists); err != nil || exists == 0 {
		return nil
	}
	for _, c := range caps {
		ts := c.CheckedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO camera_capabilities (camera_id, capability, status, evidence, firmware, checked_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(camera_id, capability) DO UPDATE SET
				status = CASE WHEN camera_capabilities.status = 'confirmed' AND excluded.status = 'unknown' THEN 'confirmed' ELSE excluded.status END,
				evidence = excluded.evidence,
				firmware = CASE WHEN excluded.firmware <> '' THEN excluded.firmware ELSE camera_capabilities.firmware END,
				checked_at = excluded.checked_at`,
			cameraID, c.Capability, string(c.Status), c.Evidence, c.Firmware, ts.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// ListCapabilities returns all stored capabilities for cameraID.
func (s *Store) ListCapabilities(ctx context.Context, cameraID string) ([]onvif.Capability, error) {
	if s == nil || s.db == nil {
		return []onvif.Capability{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT capability, status, evidence, firmware, checked_at FROM camera_capabilities WHERE camera_id = ? ORDER BY capability`, cameraID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]onvif.Capability, 0)
	for rows.Next() {
		var c onvif.Capability
		var st string
		var ts int64
		if err := rows.Scan(&c.Capability, &st, &c.Evidence, &c.Firmware, &ts); err != nil {
			return nil, err
		}
		c.Status = onvif.CapabilityStatus(st)
		c.CheckedAt = time.UnixMilli(ts).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnrichRecentPushRule checks whether a classified push event (person/vehicle/face/lpr)
// was already recorded for cameraID near at, updating its rule zone when appropriate
// so an unclassified TVT alarm poller alert does not create an empty duplicate row.
func (s *Store) EnrichRecentPushRule(ctx context.Context, cameraID, ruleZone string, at time.Time, window time.Duration) (bool, error) {
	if s == nil || s.db == nil || cameraID == "" {
		return false, nil
	}
	if window <= 0 {
		window = 15 * time.Second
	}
	fromMS := at.Add(-window).UTC().UnixMilli()
	toMS := at.Add(2 * time.Second).UTC().UnixMilli()
	var id, curZone string
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(zone, '') FROM events
		WHERE camera_id = ? AND source = 'provisionISR'
		  AND type IN ('person', 'vehicle', 'face', 'lpr')
		  AND timestamp_utc BETWEEN ? AND ?
		ORDER BY timestamp_utc DESC LIMIT 1`, cameraID, fromMS, toMS).Scan(&id, &curZone)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ruleZone != "" && (curZone == "" || (curZone == "intrusion" && ruleZone == "line_crossing")) {
		_, _ = s.db.ExecContext(ctx, `UPDATE events SET zone = ? WHERE id = ?`, ruleZone, id)
	}
	return true, nil
}

// HasProvisionPhotoPush reports whether cameraID has previously stored an HTTP push
// event with an inline snapshot, indicating the camera uploads live alarm JPEGs.
func (s *Store) HasProvisionPhotoPush(ctx context.Context, cameraID string) bool {
	if s == nil || s.db == nil || cameraID == "" {
		return false
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE camera_id = ? AND source = 'provisionISR' AND COALESCE(snapshot_ref, '') <> '' LIMIT 1`, cameraID).Scan(&one)
	return err == nil && one == 1
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
