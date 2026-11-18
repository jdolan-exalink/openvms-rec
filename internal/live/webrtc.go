package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// ErrStreamInactive is returned when a camera has no active ingest tap feeding frames.
var ErrStreamInactive = errors.New("stream is not actively ingesting")

const (
	defaultStreamStale = 15 * time.Second
	defaultIdleStop    = 12 * time.Second
	gatherTimeout      = 4 * time.Second
)

// Service manages on-demand shared WebRTC broadcasters backed by live.Hub taps.
type Service struct {
	Hub    *Hub
	FFmpeg string
	Log    *slog.Logger

	// IdleStop closes a broadcaster after all WebRTC viewers disconnect.
	IdleStop time.Duration
	// DisableTranscoderForTest skips launching ffmpeg in unit tests.
	DisableTranscoderForTest bool

	once         sync.Once
	api          *webrtc.API
	mu           sync.Mutex
	broadcasters map[string]*broadcaster
	closed       bool
}

func (s *Service) init() {
	s.once.Do(func() {
		s.broadcasters = map[string]*broadcaster{}
		m := &webrtc.MediaEngine{}
		_ = m.RegisterDefaultCodecs()
		ir := &interceptor.Registry{}
		_ = webrtc.RegisterDefaultInterceptors(m, ir)
		se := webrtc.SettingEngine{}
		se.SetIncludeLoopbackCandidate(true)
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeTCP4,
		})
		s.api = webrtc.NewAPI(
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(ir),
			webrtc.WithSettingEngine(se),
		)
	})
}

// Close shuts down all active WebRTC broadcasters and peer connections.
func (s *Service) Close() error {
	s.init()
	s.mu.Lock()
	s.closed = true
	list := make([]*broadcaster, 0, len(s.broadcasters))
	for _, b := range s.broadcasters {
		list = append(list, b)
	}
	s.broadcasters = map[string]*broadcaster{}
	s.mu.Unlock()
	for _, b := range list {
		b.stop()
	}
	return nil
}

// NegotiateOffer creates a WebRTC Answer for a camera's tapped MPEG-TS stream
// without opening an extra RTSP connection to the camera.
func (s *Service) NegotiateOffer(ctx context.Context, cameraID, offerSDP, requestedRep string) (answerSDP, repUsed string, err error) {
	if s.Hub == nil {
		return "", "", ErrStreamInactive
	}
	s.init()

	rep := strings.TrimSpace(requestedRep)
	if rep != "" {
		if !s.hasActiveRep(cameraID, rep) {
			return "", "", ErrStreamInactive
		}
	} else {
		var ok bool
		rep, ok = s.Hub.PreferredStream(cameraID, defaultStreamStale)
		if !ok {
			return "", "", ErrStreamInactive
		}
	}

	if strings.TrimSpace(offerSDP) == "" {
		return "", "", errors.New("empty sdp offer")
	}

	b, err := s.getOrStart(cameraID, rep)
	if err != nil {
		return "", "", err
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", "", fmt.Errorf("new peer connection: %w", err)
	}

	sender, err := pc.AddTrack(b.track)
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("add track: %w", err)
	}
	go drainRTCP(sender)
	if b.audioTrack != nil && strings.Contains(offerSDP, "m=audio") {
		if aSender, aErr := pc.AddTrack(b.audioTrack); aErr == nil {
			go drainRTCP(aSender)
		}
	}

	b.addPeer(pc)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			b.removePeer(pc)
			_ = pc.Close()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		b.removePeer(pc)
		_ = pc.Close()
		return "", "", fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		b.removePeer(pc)
		_ = pc.Close()
		return "", "", fmt.Errorf("create answer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		b.removePeer(pc)
		_ = pc.Close()
		return "", "", fmt.Errorf("set local description: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, gatherTimeout)
	defer cancel()
	select {
	case <-gatherDone:
	case <-waitCtx.Done():
	}

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		b.removePeer(pc)
		_ = pc.Close()
		return "", "", errors.New("missing local description")
	}
	return localDesc.SDP, rep, nil
}

func (s *Service) hasActiveRep(cameraID, rep string) bool {
	s.Hub.mu.RLock()
	t := s.Hub.streams[streamKey(cameraID, rep)]
	s.Hub.mu.RUnlock()
	return t != nil && t.isActive(time.Now(), defaultStreamStale)
}

func (s *Service) getOrStart(cameraID, rep string) (*broadcaster, error) {
	key := streamKey(cameraID, rep)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("live service closed")
	}
	if b, ok := s.broadcasters[key]; ok && !b.isStopped() {
		return b, nil
	}
	b, err := s.startBroadcaster(key, cameraID, rep)
	if err != nil {
		return nil, err
	}
	s.broadcasters[key] = b
	return b, nil
}

func (s *Service) removeBroadcaster(key string, b *broadcaster) {
	s.mu.Lock()
	if s.broadcasters[key] == b {
		delete(s.broadcasters, key)
	}
	s.mu.Unlock()
}

type broadcaster struct {
	svc        *Service
	key        string
	cameraID   string
	rep        string
	track      *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP

	ctx      context.Context
	cancel   context.CancelFunc
	udp      *net.UDPConn
	audioUDP *net.UDPConn

	mu        sync.Mutex
	peers     map[*webrtc.PeerConnection]struct{}
	idleTimer *time.Timer
	stopped   bool
}

func (s *Service) startBroadcaster(key, cameraID, rep string) (*broadcaster, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeH264,
		ClockRate: 90000,
	}, "video", "openvms-"+cameraID)
	if err != nil {
		return nil, fmt.Errorf("create rtp track: %w", err)
	}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", "openvms-"+cameraID)
	if err != nil {
		return nil, fmt.Errorf("create audio rtp track: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &broadcaster{
		svc:        s,
		key:        key,
		cameraID:   cameraID,
		rep:        rep,
		track:      track,
		audioTrack: audioTrack,
		ctx:        ctx,
		cancel:     cancel,
		peers:      map[*webrtc.PeerConnection]struct{}{},
	}

	if s.DisableTranscoderForTest {
		return b, nil
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("listen rtp udp: %w", err)
	}
	_ = udpConn.SetReadBuffer(4 << 20)
	b.udp = udpConn
	port := udpConn.LocalAddr().(*net.UDPAddr).Port

	audioUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		_ = udpConn.Close()
		cancel()
		return nil, fmt.Errorf("listen audio rtp udp: %w", err)
	}
	_ = audioUDP.SetReadBuffer(1 << 20)
	b.audioUDP = audioUDP
	audioPort := audioUDP.LocalAddr().(*net.UDPAddr).Port

	go b.pumpRTP(udpConn, track)
	go b.pumpRTP(audioUDP, audioTrack)
	go b.runTranscoder(port, audioPort)
	return b, nil
}

func (b *broadcaster) pumpRTP(conn *net.UDPConn, track *webrtc.TrackLocalStaticRTP) {
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n > 0 {
			if _, werr := track.Write(buf[:n]); werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
				if b.svc.Log != nil {
					b.svc.Log.Debug("webrtc track write", "stream", b.key, "err", werr)
				}
			}
		}
	}
}

func (b *broadcaster) runTranscoder(rtpPort, audioPort int) {
	defer b.stop()

	ffmpeg := b.svc.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	sub := b.svc.Hub.Subscribe(b.ctx, b.cameraID, b.rep)
	defer sub.Close()

	scaleFilter := "scale=min(854\\,iw):-2"
	maxRate, bufSize := "1400k", "2800k"
	if b.rep == "main" {
		scaleFilter = "scale=min(1280\\,iw):-2"
		maxRate, bufSize = "2500k", "5000k"
	}

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "+genpts+discardcorrupt",
		"-analyzeduration", "5000000",
		"-probesize", "2097152",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-map", "0:v:0",
		"-vf", scaleFilter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-profile:v", "baseline",
		"-level", "3.1",
		"-pix_fmt", "yuv420p",
		"-g", "20",
		"-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*1)",
		"-crf", "23",
		"-maxrate", maxRate,
		"-bufsize", bufSize,
		"-x264-params", "repeat-headers=1:sliced-threads=0:bframes=0",
		"-f", "rtp",
		"-payload_type", "96",
		"rtp://127.0.0.1:" + strconv.Itoa(rtpPort) + "?pkt_size=1200",
		"-map", "0:a:0?",
		"-af", "aresample=async=1",
		"-c:a", "libopus",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", "64k",
		"-f", "rtp",
		"-payload_type", "111",
		"rtp://127.0.0.1:" + strconv.Itoa(audioPort) + "?pkt_size=1200",
	}

	cmd := exec.CommandContext(b.ctx, ffmpeg, args...)
	cmd.Stdin = sub
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && b.ctx.Err() == nil {
		if b.svc.Log != nil {
			b.svc.Log.Warn("live webrtc transcoder exited", "stream", b.key, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
}

func (b *broadcaster) addPeer(pc *webrtc.PeerConnection) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	b.peers[pc] = struct{}{}
}

func (b *broadcaster) removePeer(pc *webrtc.PeerConnection) {
	b.mu.Lock()
	delete(b.peers, pc)
	rem := len(b.peers)
	stopped := b.stopped
	if rem == 0 && !stopped {
		d := b.svc.IdleStop
		if d <= 0 {
			d = defaultIdleStop
		}
		if b.idleTimer != nil {
			b.idleTimer.Stop()
		}
		b.idleTimer = time.AfterFunc(d, func() {
			b.mu.Lock()
			empty := len(b.peers) == 0 && !b.stopped
			b.mu.Unlock()
			if empty {
				b.stop()
			}
		})
	}
	b.mu.Unlock()
}

func (b *broadcaster) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

func (b *broadcaster) stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	peers := make([]*webrtc.PeerConnection, 0, len(b.peers))
	for pc := range b.peers {
		peers = append(peers, pc)
	}
	b.peers = map[*webrtc.PeerConnection]struct{}{}
	b.mu.Unlock()

	b.svc.removeBroadcaster(b.key, b)
	b.cancel()
	if b.udp != nil {
		_ = b.udp.Close()
	}
	if b.audioUDP != nil {
		_ = b.audioUDP.Close()
	}
	for _, pc := range peers {
		_ = pc.Close()
	}
}

func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	const maxLen = 2048
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > maxLen {
		t.buf = t.buf[len(t.buf)-maxLen:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
