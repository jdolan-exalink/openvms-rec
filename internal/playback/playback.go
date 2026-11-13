// Package playback serves recorded segments to browsers as HLS without
// transcoding: the recorder's MPEG-TS segments are valid HLS media segments.
// Access uses short-lived signed tokens; RTSP URLs and credentials are never
// exposed (PRD M07, M08).
package playback

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/segments"
)

// Grant is what a token allows: one camera stream over a time range.
type Grant struct {
	CameraID       string    `json:"c"`
	Representation string    `json:"r"`
	From           time.Time `json:"f"`
	To             time.Time `json:"t"`
	Expires        time.Time `json:"e"`
	// Compat asks for H.264 when the browser cannot decode the recorded
	// codec (HEVC); segments are converted on the fly, only for viewing.
	Compat bool `json:"x,omitempty"`
}

// Signer issues and verifies tokens with an HMAC key.
type Signer struct{ key []byte }

// NewSigner returns a signer with a fresh random key. Tokens do not survive a
// restart, which is acceptable for short-lived playback sessions.
func NewSigner() (*Signer, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return &Signer{key: k}, nil
}

// ErrBadToken covers malformed, forged and expired tokens alike.
var ErrBadToken = errors.New("playback: invalid or expired token")

// Sign encodes g as payload.signature (URL-safe).
func (s *Signer) Sign(g Grant) string {
	p, _ := json.Marshal(g)
	ps := base64.RawURLEncoding.EncodeToString(p)
	return ps + "." + base64.RawURLEncoding.EncodeToString(s.mac(ps))
}

// Verify checks signature and expiry.
func (s *Signer) Verify(tok string, now time.Time) (Grant, error) {
	ps, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return Grant{}, ErrBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(ps)) {
		return Grant{}, ErrBadToken
	}
	p, err := base64.RawURLEncoding.DecodeString(ps)
	if err != nil {
		return Grant{}, ErrBadToken
	}
	var g Grant
	if err := json.Unmarshal(p, &g); err != nil || now.After(g.Expires) {
		return Grant{}, ErrBadToken
	}
	return g, nil
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Playlist builds a VOD HLS playlist. Non-contiguous segments are separated
// by EXT-X-DISCONTINUITY so players jump gaps instead of stalling; the
// absolute time of each segment is given with EXT-X-PROGRAM-DATE-TIME.
func Playlist(segs []segments.Segment, segURL func(segments.Segment) string) string {
	var b strings.Builder
	maxDur := 1.0
	for _, s := range segs {
		maxDur = math.Max(maxDur, s.End.Sub(s.Start).Seconds())
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", int(math.Ceil(maxDur)))
	var prevEnd time.Time
	for i, s := range segs {
		if i > 0 && s.Start.Sub(prevEnd) > 2*time.Second {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%.3f,\n%s\n",
			s.Start.UTC().Format("2006-01-02T15:04:05.000Z"), s.End.Sub(s.Start).Seconds(), segURL(s))
		prevEnd = s.End
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
