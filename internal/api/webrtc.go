package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jdolan-exalink/openvms-rec/internal/live"
)

type webrtcOfferRequest struct {
	SDP    string `json:"sdp"`
	Stream string `json:"stream,omitempty"`
}

type webrtcOfferResponse struct {
	SDP    string `json:"sdp"`
	Stream string `json:"stream"`
}

func (s *Server) liveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cameras/{id}/webrtc", s.cameraWebRTC)
}

func (s *Server) cameraWebRTC(w http.ResponseWriter, r *http.Request) {
	if s.WebRTC == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "live webrtc unavailable")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findCamera(id); !ok {
		writeError(w, http.StatusNotFound, "camera_not_found", "unknown camera")
		return
	}

	var req webrtcOfferRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON offer")
		return
	}
	if strings.TrimSpace(req.SDP) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "sdp is required")
		return
	}
	if req.Stream != "" && req.Stream != "main" && req.Stream != "sub" {
		writeError(w, http.StatusBadRequest, "bad_request", "stream must be main or sub")
		return
	}

	answerSDP, repUsed, err := s.WebRTC.NegotiateOffer(r.Context(), id, req.SDP, req.Stream)
	if err != nil {
		if errors.Is(err, live.ErrStreamInactive) {
			writeError(w, http.StatusConflict, "stream_inactive", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "webrtc_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, webrtcOfferResponse{
		SDP:    answerSDP,
		Stream: repUsed,
	})
}
