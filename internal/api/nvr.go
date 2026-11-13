package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr"
)

// NVRDetectRequest is the body of POST /api/v1/nvrs/detect. The password is
// used for this call only.
type NVRDetectRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	HTTPS    bool   `json:"https"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// NVRChannelDTO is one recorder channel, ready to become a camera.
type NVRChannelDTO struct {
	ID           string `json:"id"`
	Number       int    `json:"number,omitempty"`
	Name         string `json:"name"`
	Address      string `json:"address,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Online       *bool  `json:"online,omitempty"`
	MainURL      string `json:"main_url,omitempty"`
	SubURL       string `json:"sub_url,omitempty"`
	CameraID     string `json:"camera_id,omitempty"`
}

// NVRCapabilityDTO is one row of the recorder capability matrix.
type NVRCapabilityDTO struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	Source     string `json:"source,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	CheckedAt  string `json:"checked_at"`
}

// NVRDetectResponse describes a Provision-ISR (TVT NVMS-9000) recorder.
type NVRDetectResponse struct {
	Driver       string             `json:"driver"`
	Name         string             `json:"name"`
	Model        string             `json:"model"`
	Hardware     string             `json:"hardware,omitempty"`
	Firmware     string             `json:"firmware"`
	RTSPPort     int                `json:"rtsp_port,omitempty"`
	RTSPCodec    string             `json:"rtsp_codec,omitempty"`
	Channels     []NVRChannelDTO    `json:"channels"`
	Capabilities []NVRCapabilityDTO `json:"capabilities"`
}

func (s *Server) nvrRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/nvrs/detect", s.detectNVR)
}

func capStatus(st provisionnvr.CapState) string {
	switch st {
	case provisionnvr.CapConfirmed:
		return "confirmed"
	case provisionnvr.CapUnsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

func (s *Server) detectNVR(w http.ResponseWriter, r *http.Request) {
	var req NVRDetectRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if req.Username == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "username is required")
		return
	}
	port := req.Port
	if port == 0 {
		port = 80
		if req.HTTPS {
			port = 443
		}
	}
	if port < 1 || port > 65535 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "port must be in [1, 65535]")
		return
	}
	// Reuse the ONVIF target check: host must be inside discovery.allowed_networks.
	_, ip, err := s.probeEndpoint(r.Context(), ProbeRequest{Host: strings.TrimSpace(req.Host), ONVIFPort: port})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_target", err.Error())
		return
	}
	scheme := "http"
	if req.HTTPS {
		scheme = "https"
	}
	base := scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(port))
	select {
	case s.probeSlots() <- struct{}{}:
		defer func() { <-s.probeSlots() }()
	default:
		writeError(w, http.StatusTooManyRequests, "busy", "another device test is running; retry in a few seconds")
		return
	}
	detect := s.NVRDetect
	if detect == nil {
		detect = provisionnvr.Detect
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := detect(ctx, provisionnvr.DetectInput{BaseURL: base, Username: req.Username, Password: req.Password, Transport: guardedTransport(s.cameraNetworks())})
	audit := map[string]any{"host": ip, "driver": "provision_nvr"}
	if err != nil {
		class, _ := provisionnvr.ClassOf(err)
		audit["error"] = string(class)
		s.audit(r.Context(), "nvr.detect", "", audit)
		switch {
		case errors.Is(err, provisionnvr.ErrNotActivated):
			writeError(w, http.StatusUnprocessableEntity, "not_activated", "the recorder is not activated: finish its setup first")
		case class == provisionnvr.ClassAuthFailed:
			writeError(w, http.StatusUnprocessableEntity, "auth_failed", "the recorder rejected the username or password")
		case class == provisionnvr.ClassLocked:
			writeError(w, http.StatusUnprocessableEntity, "locked", "the recorder locked the account after failed attempts; wait and retry")
		case class == provisionnvr.ClassUnsupported:
			writeError(w, http.StatusUnprocessableEntity, "unsupported", "the device is not a Provision-ISR / TVT NVMS-9000 recorder")
		default:
			writeError(w, http.StatusBadGateway, "unreachable", "the recorder did not answer at "+base)
		}
		return
	}
	s.audit(r.Context(), "nvr.detect", "", audit)

	out := NVRDetectResponse{Driver: "provision_nvr", Name: d.Device.Name, Model: d.Device.Model, Hardware: d.Device.HardwareModel,
		Firmware: d.Device.SoftwareVersion, Channels: []NVRChannelDTO{}, Capabilities: []NVRCapabilityDTO{}}
	rtspPort := d.Services.RTSPPort
	if d.Services.RTSPOn && rtspPort > 0 {
		out.RTSPPort = rtspPort
	}
	if d.RTSP != nil {
		out.RTSPCodec = d.RTSP.Codec
	}
	now := s.now().UTC().Format(time.RFC3339)
	for _, c := range d.Capabilities {
		out.Capabilities = append(out.Capabilities, NVRCapabilityDTO{Capability: "provision_nvr." + c.Name, Status: capStatus(c.State),
			Source: c.Source, Evidence: c.Detail, CheckedAt: now})
	}
	byHost := s.existingStreams()
	for _, ch := range d.Channels {
		dto := NVRChannelDTO{ID: ch.ID, Name: ch.Name, Address: ch.Address, Model: ch.Model, Manufacturer: ch.Manufacturer}
		if ch.Status != nil {
			on := ch.Status.Online
			dto.Online = &on
		}
		if n, ok := provisionnvr.ChannelNumber(ch.ID); ok {
			dto.Number = n
			if out.RTSPPort > 0 {
				hp := net.JoinHostPort(ip, strconv.Itoa(out.RTSPPort))
				dto.MainURL = "rtsp://" + hp + provisionnvr.RTSPPath(n, provisionnvr.StreamMain)
				dto.SubURL = "rtsp://" + hp + provisionnvr.RTSPPath(n, provisionnvr.StreamSub)
				dto.CameraID = byHost[dto.MainURL]
			}
		}
		out.Channels = append(out.Channels, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

// existingStreams maps each camera's main URL to its id.
func (s *Server) existingStreams() map[string]string {
	out := map[string]string{}
	for _, e := range s.registry().List() {
		out[e.MainURL] = e.ID
	}
	return out
}
