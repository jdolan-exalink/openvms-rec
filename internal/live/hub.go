// Package live provides zero-extra-RTSP-session live WebRTC video delivery
// by tapping the MPEG-TS stream already produced by active ingest recorders.
package live

import (
	"context"
	"io"
	"sync"
	"time"
)

const (
	tsPacketSize     = 188
	tsSyncByte       = 0x47
	subBufChunks     = 128
	maxGOPCacheBytes = 4 << 20 // cap cached GOP at 4 MiB for instant keyframe startup
)

// Hub multiplexes active ingest MPEG-TS streams to on-demand live subscribers.
// Calling Tap returns an io.Writer whose Write method never blocks the recorder.
type Hub struct {
	mu      sync.RWMutex
	streams map[string]*streamTap
}

// NewHub constructs an empty live stream hub.
func NewHub() *Hub {
	return &Hub{streams: map[string]*streamTap{}}
}

func streamKey(cameraID, rep string) string {
	return cameraID + "/" + rep
}

// Tap returns an io.Writer for cameraID/rep that should be wired to Recorder.LiveTap.
func (h *Hub) Tap(cameraID, rep string) io.Writer {
	return h.getOrCreate(streamKey(cameraID, rep))
}

// PreferredStream picks the best active representation for live viewing
// (preferring "sub" for low-latency mosaic grids, falling back to "main").
func (h *Hub) PreferredStream(cameraID string, staleAfter time.Duration) (rep string, ok bool) {
	now := time.Now()
	for _, candidate := range []string{"sub", "main"} {
		h.mu.RLock()
		t := h.streams[streamKey(cameraID, candidate)]
		h.mu.RUnlock()
		if t != nil && t.isActive(now, staleAfter) {
			return candidate, true
		}
	}
	return "", false
}

// Subscribe attaches a subscriber to cameraID/rep. The returned ReadCloser
// streams aligned MPEG-TS bytes starting with the latest cached PAT/PMT
// packets when available. Calling Close detaches the subscriber cleanly.
func (h *Hub) Subscribe(ctx context.Context, cameraID, rep string) io.ReadCloser {
	t := h.getOrCreate(streamKey(cameraID, rep))
	return t.subscribe(ctx)
}

func (h *Hub) getOrCreate(key string) *streamTap {
	h.mu.RLock()
	t := h.streams[key]
	h.mu.RUnlock()
	if t != nil {
		return t
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if t = h.streams[key]; t == nil {
		t = newStreamTap()
		h.streams[key] = t
	}
	return t
}

type streamTap struct {
	mu       sync.Mutex
	rem      []byte
	pat      [tsPacketSize]byte
	hasPAT   bool
	pmtPID   uint16
	pmt      [tsPacketSize]byte
	hasPMT   bool
	videoPID uint16
	gop      []byte
	hasGOP   bool
	lastSeen time.Time
	subs     map[*subscriber]struct{}
}

func newStreamTap() *streamTap {
	return &streamTap{subs: map[*subscriber]struct{}{}}
}

func (t *streamTap) isActive(now time.Time, staleAfter time.Duration) bool {
	t.mu.Lock()
	ls := t.lastSeen
	t.mu.Unlock()
	if ls.IsZero() {
		return false
	}
	if staleAfter <= 0 {
		staleAfter = 10 * time.Second
	}
	return now.Sub(ls) <= staleAfter
}

// Write parses 188-byte aligned MPEG-TS packets, caches the latest PAT/PMT so
// late subscribers synchronize immediately, and fans out bytes non-blockingly.
func (t *streamTap) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	t.lastSeen = time.Now()
	// Short-circuit parsing overhead if nobody is watching, except keep header cache warm.
	hasSubs := len(t.subs) > 0
	t.rem = append(t.rem, p...)
	// Re-align to MPEG-TS sync byte if needed.
	for len(t.rem) >= tsPacketSize && t.rem[0] != tsSyncByte {
		idx := -1
		for i := 1; i < len(t.rem); i++ {
			if t.rem[i] == tsSyncByte && (i+tsPacketSize >= len(t.rem) || t.rem[i+tsPacketSize] == tsSyncByte) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.rem = t.rem[:0]
			break
		}
		t.rem = t.rem[idx:]
	}
	alignedLen := (len(t.rem) / tsPacketSize) * tsPacketSize
	var out []byte
	if alignedLen > 0 {
		chunk := t.rem[:alignedLen]
		for off := 0; off < alignedLen; off += tsPacketSize {
			pkt := chunk[off : off+tsPacketSize]
			if pkt[0] == tsSyncByte {
				t.inspectPacket(pkt)
			}
		}
		if hasSubs {
			out = make([]byte, alignedLen)
			copy(out, chunk)
		}
		remLeft := len(t.rem) - alignedLen
		copy(t.rem[:remLeft], t.rem[alignedLen:])
		t.rem = t.rem[:remLeft]
	}
	var subs []*subscriber
	if len(out) > 0 && hasSubs {
		subs = make([]*subscriber, 0, len(t.subs))
		for s := range t.subs {
			subs = append(subs, s)
		}
	}
	t.mu.Unlock()

	for _, s := range subs {
		s.push(out)
	}
	return len(p), nil
}

func (t *streamTap) inspectPacket(pkt []byte) {
	pid := ((uint16(pkt[1]) & 0x1f) << 8) | uint16(pkt[2])
	if pid == 0x0000 {
		copy(t.pat[:], pkt)
		t.hasPAT = true
		if pmtPID := parsePATPMTPID(pkt); pmtPID != 0 {
			t.pmtPID = pmtPID
		}
		return
	}
	if t.pmtPID != 0 && pid == t.pmtPID {
		copy(t.pmt[:], pkt)
		t.hasPMT = true
		if vpid := parsePMTVideoPID(pkt); vpid != 0 {
			t.videoPID = vpid
		}
		return
	}
	if t.videoPID != 0 && pid == t.videoPID {
		afc := (pkt[3] >> 4) & 0x03
		if (afc == 2 || afc == 3) && pkt[4] >= 1 && (pkt[5]&0x40) != 0 {
			t.gop = append(t.gop[:0], pkt...)
			t.hasGOP = true
			return
		}
	}
	if t.hasGOP && len(t.gop)+tsPacketSize <= maxGOPCacheBytes {
		t.gop = append(t.gop, pkt...)
	}
}

// parsePMTVideoPID extracts the first video elementary PID from a PMT 188-byte packet.
func parsePMTVideoPID(pkt []byte) uint16 {
	pusi := (pkt[1] & 0x40) != 0
	afc := (pkt[3] >> 4) & 0x03
	if !pusi || afc == 0 || afc == 2 {
		return 0
	}
	pos := 4
	if afc == 3 {
		pos = 5 + int(pkt[4])
	}
	if pos >= tsPacketSize {
		return 0
	}
	pos += 1 + int(pkt[pos])
	if pos+12 > tsPacketSize || pkt[pos] != 0x02 {
		return 0
	}
	secLen := int(pkt[pos+1]&0x0f)<<8 | int(pkt[pos+2])
	secEnd := pos + 3 + secLen - 4
	if secEnd > tsPacketSize {
		secEnd = tsPacketSize
	}
	progInfoLen := int(pkt[pos+10]&0x0f)<<8 | int(pkt[pos+11])
	esPos := pos + 12 + progInfoLen
	for esPos+5 <= secEnd {
		streamType := pkt[esPos]
		elemPID := ((uint16(pkt[esPos+1]) & 0x1f) << 8) | uint16(pkt[esPos+2])
		esInfoLen := int(pkt[esPos+3]&0x0f)<<8 | int(pkt[esPos+4])
		switch streamType {
		case 0x01, 0x02, 0x10, 0x1b, 0x24:
			return elemPID
		}
		esPos += 5 + esInfoLen
	}
	return 0
}

// parsePATPMTPID extracts the first program PMT PID from a PAT 188-byte packet.
func parsePATPMTPID(pkt []byte) uint16 {
	pusi := (pkt[1] & 0x40) != 0
	afc := (pkt[3] >> 4) & 0x03
	if !pusi || afc == 0 || afc == 2 {
		return 0
	}
	pos := 4
	if afc == 3 {
		adaptLen := int(pkt[4])
		pos = 5 + adaptLen
	}
	if pos >= tsPacketSize {
		return 0
	}
	ptr := int(pkt[pos])
	pos += 1 + ptr
	// PAT table starts with table_id (0x00), section_length (bytes 1..2), ... programs start at offset 8.
	if pos+12 > tsPacketSize || pkt[pos] != 0x00 {
		return 0
	}
	secLen := int(pkt[pos+1]&0x0f)<<8 | int(pkt[pos+2])
	secEnd := pos + 3 + secLen - 4 // exclude 4-byte CRC32
	if secEnd > tsPacketSize {
		secEnd = tsPacketSize
	}
	for progPos := pos + 8; progPos+4 <= secEnd; progPos += 4 {
		progNum := uint16(pkt[progPos])<<8 | uint16(pkt[progPos+1])
		if progNum != 0 {
			return ((uint16(pkt[progPos+2]) & 0x1f) << 8) | uint16(pkt[progPos+3])
		}
	}
	return 0
}

func (t *streamTap) subscribe(ctx context.Context) io.ReadCloser {
	s := &subscriber{
		tap: t,
		ch:  make(chan []byte, subBufChunks),
	}
	var hdr []byte
	t.mu.Lock()
	if t.hasPAT {
		hdr = append(hdr, t.pat[:]...)
	}
	if t.hasPMT {
		hdr = append(hdr, t.pmt[:]...)
	}
	if t.hasGOP && len(t.gop) > 0 {
		hdr = append(hdr, t.gop...)
	}
	t.subs[s] = struct{}{}
	if len(hdr) > 0 {
		s.ch <- hdr
	}
	t.mu.Unlock()
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			_ = s.Close()
		}()
	}
	return s
}

type subscriber struct {
	tap    *streamTap
	ch     chan []byte
	cur    []byte
	mu     sync.Mutex
	closed bool
}

func (s *subscriber) push(chunk []byte) {
	select {
	case s.ch <- chunk:
	default:
		// Slow subscriber: drop incoming chunk cleanly so ingest never stalls.
	}
}

func (s *subscriber) Read(p []byte) (int, error) {
	for len(s.cur) == 0 {
		chunk, ok := <-s.ch
		if !ok {
			return 0, io.EOF
		}
		s.cur = chunk
	}
	n := copy(p, s.cur)
	s.cur = s.cur[n:]
	return n, nil
}

func (s *subscriber) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.tap.mu.Lock()
	delete(s.tap.subs, s)
	s.tap.mu.Unlock()
	close(s.ch)
	return nil
}
