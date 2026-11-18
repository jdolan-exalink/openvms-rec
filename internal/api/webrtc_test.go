package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/live"
	"github.com/pion/webrtc/v4"
)

func TestCameraWebRTCEndpoint(t *testing.T) {
	hub := live.NewHub()
	svc := &live.Service{Hub: hub, DisableTranscoderForTest: true}
	defer svc.Close()

	srv := &Server{
		Cfg: config.Config{
			Cameras: []config.Camera{
				{ID: "cam-1", Name: "Cam 1", MainURL: "rtsp://cam/main"},
			},
		},
		WebRTC: svc,
	}
	h := srv.Handler()

	// Unknown camera -> 404
	req404 := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/unknown/webrtc", strings.NewReader(`{"sdp":"v=0"}`))
	rec404 := httptest.NewRecorder()
	h.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("unknown camera code = %d, want 404", rec404.Code)
	}

	// Inactive stream -> 409 Conflict
	reqInactive := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-1/webrtc", strings.NewReader(`{"sdp":"v=0"}`))
	recInactive := httptest.NewRecorder()
	h.ServeHTTP(recInactive, reqInactive)
	if recInactive.Code != http.StatusConflict {
		t.Fatalf("inactive stream code = %d, want 409", recInactive.Code)
	}

	// Activate tapped stream with a PAT packet
	pat := make([]byte, 188)
	pat[0] = 0x47
	pat[1] = 0x40
	pat[3] = 0x10
	if _, err := hub.Tap("cam-1", "sub").Write(pat); err != nil {
		t.Fatal(err)
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer clientPC.Close()
	if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
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

	body, _ := json.Marshal(webrtcOfferRequest{SDP: offer.SDP})
	reqOK := httptest.NewRequest(http.MethodPost, "/api/v1/cameras/cam-1/webrtc", bytes.NewReader(body))
	recOK := httptest.NewRecorder()
	h.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid webrtc offer code = %d, body=%s", recOK.Code, recOK.Body.String())
	}
	var resp webrtcOfferResponse
	if err := json.Unmarshal(recOK.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Stream != "sub" || !strings.Contains(resp.SDP, "m=video") {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
