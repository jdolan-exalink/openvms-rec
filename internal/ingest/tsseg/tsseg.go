// Package tsseg cuts an MPEG-TS byte stream into independently decodable
// segments without decoding or re-encoding (ADR-001, PRD M03).
//
// FFmpeg remuxes the camera's RTSP stream with -c copy into MPEG-TS on stdout.
// tsseg reads it in 188-byte packets, learns the program layout from PAT/PMT,
// and starts a new segment only at a video random-access point (keyframe,
// signalled by the adaptation-field random_access_indicator) once the target
// duration has elapsed. Every segment begins with the latest PAT and PMT, then
// a keyframe, so each file plays on its own and can live on any volume.
//
// Why MPEG-TS: a truncated TS file (power loss, crash) is still playable up to
// the last complete packet, unlike MP4 whose index lives at the end. fMP4 may
// be added later for browser playback (PRD M03 asks to validate this first).
package tsseg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// PacketSize is the MPEG-TS packet size.
const PacketSize = 188

const syncByte = 0x47

// Stream types from ISO/IEC 13818-1 and common extensions.
var videoStreamTypes = map[byte]string{
	0x01: "mpeg1video", 0x02: "mpeg2video", 0x10: "mpeg4", 0x1b: "h264", 0x24: "h265",
}
var audioStreamTypes = map[byte]string{
	0x03: "mp1", 0x04: "mp2", 0x0f: "aac", 0x11: "aac_latm", 0x81: "ac3", 0x90: "pcm_alaw", 0x91: "pcm_mulaw",
}

// StartInfo describes a segment that is about to be written.
type StartInfo struct {
	Start      time.Time
	VideoCodec string
	AudioCodec string // "" when the stream has no (recognised) audio
	// OtherStreams counts elementary streams that are neither recognised
	// video nor audio (e.g. G.711 that the TS muxer wrapped as private data).
	OtherStreams int
}

// Keyframe is a random-access point inside a segment.
type Keyframe struct {
	OffsetBytes int64 `json:"o"`
	OffsetMS    int64 `json:"t"`
}

// EndInfo describes a completed segment.
type EndInfo struct {
	End       time.Time
	Bytes     int64
	Keyframes []Keyframe
}

// KeyframesJSON encodes keyframes compactly for the segment index.
func (e EndInfo) KeyframesJSON() string {
	b, _ := json.Marshal(e.Keyframes)
	return string(b)
}

// Writer receives the bytes of one segment.
type Writer interface {
	io.Writer
	// Close finalizes the segment (fsync, atomic rename, index update).
	Close(EndInfo) error
	// Abort discards or quarantines a segment that cannot be completed.
	Abort(cause error)
}

// Sink opens segments. It decides placement (which volume) per segment.
type Sink interface {
	Open(StartInfo) (Writer, error)
}

// Segmenter cuts a TS stream into segments.
type Segmenter struct {
	// Target is the minimum segment duration; cuts happen at the first
	// keyframe after it. Segments are therefore Target..Target+GOP long.
	Target time.Duration
	// MaxDuration forces a cut even without a keyframe (broken GOP signalling).
	// The forced segment may start without a keyframe; 0 = 3×Target.
	MaxDuration time.Duration
	// Now returns wall-clock time. Segment times are host UTC at packet
	// arrival; precision is about one GOP plus network jitter.
	Now func() time.Time

	sink     Sink
	pmtPID   int
	videoPID int
	audioPID int
	vcodec   string
	acodec   string
	others   int
	pat      []byte
	pmt      []byte

	cur      Writer
	curStart time.Time
	curBytes int64
	curKF    []Keyframe
}

// ErrNoVideo is returned when the stream ends before any video keyframe.
var ErrNoVideo = errors.New("tsseg: no video keyframe in stream")

// Run reads r until EOF or ctx is done. The open segment, if any, is closed
// normally on EOF and aborted on errors.
func (s *Segmenter) Run(ctx context.Context, r io.Reader, sink Sink) (err error) {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Target <= 0 {
		s.Target = time.Minute
	}
	if s.MaxDuration <= 0 {
		s.MaxDuration = 3 * s.Target
	}
	s.sink = sink
	s.pmtPID, s.videoPID, s.audioPID = -1, -1, -1

	defer func() {
		if s.cur == nil {
			return
		}
		if err == nil {
			err = s.closeCurrent()
		} else {
			s.cur.Abort(err)
			s.cur = nil
		}
	}()

	br := bufio.NewReaderSize(r, 64*PacketSize)
	pkt := make([]byte, PacketSize)
	sawKeyframe := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := readPacket(br, pkt); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if !sawKeyframe {
					return ErrNoVideo
				}
				return nil
			}
			return err
		}
		pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
		pusi := pkt[1]&0x40 != 0

		switch {
		case pid == 0 && pusi:
			s.pat = append(s.pat[:0], pkt...)
			if p, ok := parsePAT(pkt); ok {
				s.pmtPID = p
			}
		case pid == s.pmtPID && pusi:
			s.pmt = append(s.pmt[:0], pkt...)
			s.parsePMT(pkt)
		}

		isKey := pid == s.videoPID && randomAccess(pkt)
		now := s.Now()
		if isKey {
			sawKeyframe = true
		}
		if isKey || (s.cur != nil && now.Sub(s.curStart) >= s.MaxDuration) {
			if s.cur == nil || now.Sub(s.curStart) >= s.Target {
				if err := s.rotate(now); err != nil {
					return err
				}
			}
		}
		if s.cur == nil {
			continue // wait for the first keyframe
		}
		if isKey {
			s.curKF = append(s.curKF, Keyframe{OffsetBytes: s.curBytes, OffsetMS: now.Sub(s.curStart).Milliseconds()})
		}
		if err := s.write(pkt); err != nil {
			return err
		}
	}
}

func (s *Segmenter) rotate(now time.Time) error {
	if s.cur != nil {
		if err := s.closeCurrent(); err != nil {
			return err
		}
	}
	w, err := s.sink.Open(StartInfo{Start: now.UTC(), VideoCodec: s.vcodec, AudioCodec: s.acodec, OtherStreams: s.others})
	if err != nil {
		return err
	}
	s.cur, s.curStart, s.curBytes, s.curKF = w, now, 0, nil
	// Repeat program tables so the segment is self-describing.
	for _, t := range [][]byte{s.pat, s.pmt} {
		if len(t) == PacketSize {
			if err := s.write(t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Segmenter) write(pkt []byte) error {
	n, err := s.cur.Write(pkt)
	s.curBytes += int64(n)
	if err != nil {
		return fmt.Errorf("write segment: %w", err)
	}
	return nil
}

func (s *Segmenter) closeCurrent() error {
	w := s.cur
	s.cur = nil
	return w.Close(EndInfo{End: s.Now().UTC(), Bytes: s.curBytes, Keyframes: s.curKF})
}

// readPacket reads one aligned packet, resynchronising on a lost sync byte.
func readPacket(br *bufio.Reader, pkt []byte) error {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b != syncByte {
			continue
		}
		pkt[0] = b
		if _, err := io.ReadFull(br, pkt[1:]); err != nil {
			return err
		}
		// Confirm alignment with the next sync byte when available.
		if next, err := br.Peek(1); err == nil && next[0] != syncByte {
			continue
		}
		return nil
	}
}

func randomAccess(pkt []byte) bool {
	afc := (pkt[3] >> 4) & 0x3
	if afc&0x2 == 0 || pkt[4] == 0 {
		return false
	}
	return pkt[5]&0x40 != 0
}

// payload returns the packet payload after the adaptation field.
func payload(pkt []byte) []byte {
	afc := (pkt[3] >> 4) & 0x3
	off := 4
	if afc&0x2 != 0 {
		off += 1 + int(pkt[4])
	}
	if afc&0x1 == 0 || off >= PacketSize {
		return nil
	}
	return pkt[off:]
}

// section returns the PSI section starting after the pointer field.
func section(pkt []byte) []byte {
	p := payload(pkt)
	if len(p) < 1 {
		return nil
	}
	start := 1 + int(p[0])
	if start >= len(p) {
		return nil
	}
	sec := p[start:]
	if len(sec) < 3 {
		return nil
	}
	l := int(sec[1]&0x0f)<<8 | int(sec[2])
	if 3+l > len(sec) {
		return sec // section continues in another packet; parse what we have
	}
	return sec[:3+l]
}

func parsePAT(pkt []byte) (int, bool) {
	sec := section(pkt)
	if len(sec) < 12 || sec[0] != 0x00 {
		return 0, false
	}
	// Program loop: from byte 8 to len-4 (CRC), 4 bytes per program.
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		prog := int(sec[i])<<8 | int(sec[i+1])
		if prog == 0 {
			continue // network PID
		}
		return int(sec[i+2]&0x1f)<<8 | int(sec[i+3]), true
	}
	return 0, false
}

func (s *Segmenter) parsePMT(pkt []byte) {
	sec := section(pkt)
	if len(sec) < 16 || sec[0] != 0x02 {
		return
	}
	progInfoLen := int(sec[10]&0x0f)<<8 | int(sec[11])
	i := 12 + progInfoLen
	s.others = 0
	for i+5 <= len(sec)-4 {
		st := sec[i]
		epid := int(sec[i+1]&0x1f)<<8 | int(sec[i+2])
		esInfoLen := int(sec[i+3]&0x0f)<<8 | int(sec[i+4])
		if c, ok := videoStreamTypes[st]; ok && s.videoPID < 0 {
			s.videoPID, s.vcodec = epid, c
		} else if c, ok := audioStreamTypes[st]; ok && s.audioPID < 0 {
			s.audioPID, s.acodec = epid, c
		} else if epid != s.videoPID && epid != s.audioPID {
			s.others++
		}
		i += 5 + esInfoLen
	}
}
