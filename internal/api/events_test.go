package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func TestListEventsPaging(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg, _ := cameras.Open(context.Background(), cameras.Options{DB: d, CredsFile: filepath.Join(dir, "c.yaml"),
		Static: []config.Camera{{ID: "patio", MainURL: "rtsp://10.0.0.1/a"}}})
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i, ts := range []int64{0, 1000, 1000, 1000, 5000} {
		d.Exec(`INSERT INTO events (id, camera_id, timestamp_utc, type, source, raw_ref, dedupe_key, created_at) VALUES (?, 'patio', ?, 'motion', 'onvif_events', '{"topic":"x"}', ?, 0)`,
			string(rune('a'+i)), base.UnixMilli()+ts, string(rune('a'+i)))
	}
	s := &Server{Cfg: config.Default(), DB: d, Prober: fakeProber{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Cameras: reg}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	q := ts.URL + "/api/v1/cameras/patio/events?from=2026-11-13T11:00:00Z&to=2026-11-13T13:00:00Z&limit=2"
	var ids []string
	cursor := ""
	for i := 0; i < 5; i++ {
		var page struct {
			Items []EventDTO `json:"items"`
			Next  string     `json:"next_cursor"`
		}
		u := q
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		if c := send(t, "GET", u, "", &page); c != 200 {
			t.Fatalf("status %d", c)
		}
		for _, e := range page.Items {
			ids = append(ids, e.EventID)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if got := len(ids); got != 5 || ids[0] != "e" || ids[4] != "a" {
		t.Fatalf("paged ids %v", ids)
	}
}
