package live

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func makeTSPacket(pid uint16, pusi bool, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = byte((pid >> 8) & 0x1f)
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	pkt[3] = 0x10 // payload only
	copy(pkt[4:], payload)
	return pkt
}

func makePATPacket(pmtPID uint16) []byte {
	// pointer(0), table_id(0x00), section_length(13), ts_id(1), ver(0), sec(0), last(0), program(1)->pmtPID
	sec := []byte{
		0x00,       // pointer_field
		0x00,       // table_id PAT
		0xb0, 0x0d, // section_length = 13
		0x00, 0x01, 0xc1, 0x00, 0x00,
		0x00, 0x01, byte(0xe0 | ((pmtPID >> 8) & 0x1f)), byte(pmtPID),
		0x00, 0x00, 0x00, 0x00,
	}
	return makeTSPacket(0x0000, true, sec)
}

func TestHubCachesPATPMTAndFansOutNonBlockingly(t *testing.T) {
	h := NewHub()
	tap := h.Tap("cam-1", "sub")

	pat := makePATPacket(0x1000)
	pmt := makeTSPacket(0x1000, true, []byte{0x00, 0x02, 0xb0, 0x05})
	vid1 := makeTSPacket(0x0100, false, []byte("FRAME1"))

	// Write unaligned slices to exercise internal 188-byte alignment.
	stream := append(append([]byte{}, pat...), pmt...)
	if n, err := tap.Write(stream[:100]); err != nil || n != 100 {
		t.Fatalf("partial write 1: n=%d err=%v", n, err)
	}
	if n, err := tap.Write(stream[100:]); err != nil || n != len(stream)-100 {
		t.Fatalf("partial write 2: n=%d err=%v", n, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := h.Subscribe(ctx, "cam-1", "sub")
	defer sub.Close()

	if _, err := tap.Write(vid1); err != nil {
		t.Fatalf("write vid1: %v", err)
	}

	got := make([]byte, len(pat)+len(pmt)+len(vid1))
	if _, err := io.ReadFull(sub, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	want := append(append(append([]byte{}, pat...), pmt...), vid1...)
	if !bytes.Equal(got, want) {
		t.Fatalf("subscriber stream mismatch: got %d bytes, want %d", len(got), len(want))
	}

	if rep, ok := h.PreferredStream("cam-1", 5*time.Second); !ok || rep != "sub" {
		t.Fatalf("PreferredStream = (%q, %v), want (sub, true)", rep, ok)
	}
}

func makePMTPacket(pmtPID, videoPID uint16, streamType byte) []byte {
	sec := []byte{
		0x00,       // pointer_field
		0x02,       // table_id PMT
		0xb0, 0x12, // section_length = 18 (13 bytes header+crc + 5 bytes ES entry)
		0x00, 0x01, 0xc1, 0x00, 0x00,
		0xe1, 0x00, // PCR PID
		0xf0, 0x00, // program_info_length = 0
		streamType,
		byte(0xe0 | ((videoPID >> 8) & 0x1f)), byte(videoPID),
		0xf0, 0x00, // ES_info_length = 0
		0x00, 0x00, 0x00, 0x00,
	}
	return makeTSPacket(pmtPID, true, sec)
}

func makeKeyTSPacket(pid uint16, payload []byte) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x40 | byte((pid>>8)&0x1f) // PUSI
	pkt[2] = byte(pid)
	pkt[3] = 0x30 // adaptation + payload
	pkt[4] = 0x01 // 1 byte adaptation flags
	pkt[5] = 0x40 // random_access_indicator = 1
	copy(pkt[6:], payload)
	return pkt
}

func TestHubCachesVideoGOPFromKeyframe(t *testing.T) {
	h := NewHub()
	tap := h.Tap("cam-gop", "sub")

	pat := makePATPacket(0x1000)
	pmt := makePMTPacket(0x1000, 0x0100, 0x24) // HEVC video PID 0x0100
	stalePFrame := makeTSPacket(0x0100, false, []byte("STALE_P"))
	idr1 := makeKeyTSPacket(0x0100, []byte("IDR_1"))
	pAfter := makeTSPacket(0x0100, false, []byte("DELTA_1"))

	if _, err := tap.Write(append(append(append([]byte{}, pat...), pmt...), stalePFrame...)); err != nil {
		t.Fatal(err)
	}
	if _, err := tap.Write(append(append([]byte{}, idr1...), pAfter...)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := h.Subscribe(ctx, "cam-gop", "sub")
	defer sub.Close()

	// Subscriber must receive PAT + PMT + IDR_1 + DELTA_1 (never STALE_P before keyframe).
	got := make([]byte, len(pat)+len(pmt)+len(idr1)+len(pAfter))
	if _, err := io.ReadFull(sub, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	want := append(append(append(append([]byte{}, pat...), pmt...), idr1...), pAfter...)
	if !bytes.Equal(got, want) {
		t.Fatalf("GOP startup cache mismatch: got %q, want %q", got[:4], want[:4])
	}
}

func TestHubSlowSubscriberDoesNotBlockWrite(t *testing.T) {
	h := NewHub()
	tap := h.Tap("cam-2", "main")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = h.Subscribe(ctx, "cam-2", "main") // never reads

	vid := makeTSPacket(0x0100, false, []byte("PKT"))
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBufChunks*2; i++ {
			if _, err := tap.Write(vid); err != nil {
				t.Errorf("tap.Write error: %v", err)
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tap.Write blocked on slow subscriber")
	}
}

func TestServiceNegotiateOfferAndInactiveError(t *testing.T) {
	hub := NewHub()
	svc := &Service{Hub: hub, DisableTranscoderForTest: true}
	defer svc.Close()

	// Inactive stream returns ErrStreamInactive.
	if _, _, err := svc.NegotiateOffer(context.Background(), "cam-missing", "v=0...", ""); !errors.Is(err, ErrStreamInactive) {
		t.Fatalf("inactive stream err = %v, want ErrStreamInactive", err)
	}

	// Activate sub stream tap.
	if _, err := hub.Tap("cam-1", "sub").Write(makePATPacket(0x1000)); err != nil {
		t.Fatal(err)
	}

	// Create a real client PeerConnection Offer to test SDP Answer negotiation end-to-end.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	clientPC, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()

	if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	answerSDP, repUsed, err := svc.NegotiateOffer(context.Background(), "cam-1", offer.SDP, "")
	if err != nil {
		t.Fatalf("NegotiateOffer error: %v", err)
	}
	if repUsed != "sub" {
		t.Fatalf("repUsed = %q, want sub", repUsed)
	}
	if !strings.Contains(answerSDP, "m=video") || !strings.Contains(answerSDP, "H264") {
		t.Fatalf("unexpected SDP answer:\n%s", answerSDP)
	}
	if !strings.Contains(answerSDP, "m=audio") || !strings.Contains(answerSDP, "opus") {
		t.Fatalf("expected opus audio track in SDP answer:\n%s", answerSDP)
	}
}
