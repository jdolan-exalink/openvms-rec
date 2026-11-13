package playback

import (
	"strings"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

func TestTokenRoundTripAndTamper(t *testing.T) {
	s, _ := NewSigner()
	now := time.Now()
	g := Grant{CameraID: "cam1", Representation: "main", From: now.Add(-time.Hour).UTC(), To: now.UTC(), Expires: now.Add(time.Minute).UTC()}
	tok := s.Sign(g)
	got, err := s.Verify(tok, now)
	if err != nil || got.CameraID != "cam1" || !got.From.Equal(g.From) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Verify(tok, now.Add(2*time.Minute)); err != ErrBadToken {
		t.Fatal("expired token accepted")
	}
	// Change camera in payload, keep signature.
	ps, sig, _ := strings.Cut(tok, ".")
	other := s.Sign(Grant{CameraID: "cam2", Expires: g.Expires})
	ops, _, _ := strings.Cut(other, ".")
	if _, err := s.Verify(ops+"."+sig, now); err != ErrBadToken {
		t.Fatal("forged token accepted")
	}
	s2, _ := NewSigner()
	if _, err := s2.Verify(ps+"."+sig, now); err != ErrBadToken {
		t.Fatal("token from another key accepted")
	}
}

func TestPlaylistDiscontinuity(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	segs := []segments.Segment{
		{ID: 1, Start: t0, End: t0.Add(60 * time.Second)},
		{ID: 2, Start: t0.Add(60 * time.Second), End: t0.Add(121 * time.Second)},
		{ID: 3, Start: t0.Add(10 * time.Minute), End: t0.Add(11 * time.Minute)},
	}
	pl := Playlist(segs, func(s segments.Segment) string { return "s" + string(rune('0'+s.ID)) + ".ts" })
	if strings.Count(pl, "#EXT-X-DISCONTINUITY") != 1 || !strings.Contains(pl, "#EXT-X-TARGETDURATION:61") ||
		!strings.Contains(pl, "#EXTINF:61.000,\ns2.ts") || !strings.HasSuffix(pl, "#EXT-X-ENDLIST\n") ||
		!strings.Contains(pl, "#EXT-X-PROGRAM-DATE-TIME:2026-11-12T10:10:00.000Z") {
		t.Fatal(pl)
	}
}
