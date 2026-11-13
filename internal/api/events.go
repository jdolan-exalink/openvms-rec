package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EventDTO is the Event schema.
type EventDTO struct {
	EventID      string          `json:"event_id"`
	CameraID     string          `json:"camera_id"`
	TimestampUTC string          `json:"timestamp_utc"`
	Type         string          `json:"type"`
	Class        string          `json:"class,omitempty"`
	Source       string          `json:"source"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

// listEvents returns events of a camera in [from, to), newest first. The
// cursor is the timestamp (ms) of the last item of the previous page.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "no camera with that id")
		return
	}
	from, to, ok := parseRange(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be in [1, 500]")
			return
		}
		limit = n
	}
	// Cursor "<ts>_<id>": continue after that item (newest first, ties by id).
	curTS, curID := to.UnixMilli(), ""
	if c := q.Get("cursor"); c != "" {
		ts, cid, ok := strings.Cut(c, "_")
		n, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || cid == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid cursor")
			return
		}
		curTS, curID = n, cid
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT id, timestamp_utc, type, class, source, COALESCE(raw_ref, '') FROM events
		WHERE camera_id = ? AND timestamp_utc >= ? AND timestamp_utc < ?
		AND (timestamp_utc < ? OR (timestamp_utc = ? AND id > ?))
		ORDER BY timestamp_utc DESC, id LIMIT ?`,
		id, from.UnixMilli(), to.UnixMilli(), curTS, curTS, curID, limit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query failed")
		return
	}
	defer rows.Close()
	out := []EventDTO{}
	for rows.Next() {
		var e EventDTO
		var ts int64
		var raw sql.NullString
		if err := rows.Scan(&e.EventID, &ts, &e.Type, &e.Class, &e.Source, &raw); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "query failed")
			return
		}
		e.CameraID = id
		e.TimestampUTC = time.UnixMilli(ts).UTC().Format(time.RFC3339Nano)
		if raw.Valid && json.Valid([]byte(raw.String)) {
			e.Raw = json.RawMessage(raw.String)
		}
		out = append(out, e)
	}
	resp := map[string]any{"items": out}
	if len(out) > limit {
		resp["items"] = out[:limit]
		ts, _ := time.Parse(time.RFC3339Nano, out[limit-1].TimestampUTC)
		resp["next_cursor"] = strconv.FormatInt(ts.UnixMilli(), 10) + "_" + out[limit-1].EventID
	}
	writeJSON(w, http.StatusOK, resp)
}
