package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

// DiscoverRequest starts a LAN discovery.
type DiscoverRequest struct {
	Sweep     bool `json:"sweep"`
	TimeoutMS int  `json:"timeout_ms"`
}

// DiscoveredDTO is one device found on the LAN.
type DiscoveredDTO struct {
	onvif.LANDevice
	Allowed  bool   `json:"allowed"`
	CameraID string `json:"camera_id,omitempty"`
}

// ProbeRequest is the body of POST /api/v1/cameras/probe. Credentials are
// never stored or logged by the probe.
type ProbeRequest struct {
	Host      string  `json:"host"`
	ONVIFPort int     `json:"onvif_port"`
	XAddr     string  `json:"xaddr"`
	CameraID  string  `json:"camera_id"`
	Username  *string `json:"username"`
	Password  *string `json:"password"`
	Driver    string  `json:"driver"`
}

// ProbeCapabilityDTO adds the check time required by the API contract.
type ProbeCapabilityDTO struct {
	onvif.ProbeCapability
	Firmware  string `json:"firmware,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// ProbeResponse is the probe result plus a suggested camera.
type ProbeResponse struct {
	onvif.ProbeResult
	Capabilities []ProbeCapabilityDTO `json:"capabilities"`
	Suggested    SuggestedCamera      `json:"suggested"`
	ONVIFURL     string               `json:"onvif_url"`
	CameraID     string               `json:"camera_id,omitempty"`
}

// SuggestedCamera prefills the add-camera form.
type SuggestedCamera struct {
	Name    string `json:"name"`
	MainURL string `json:"main_url"`
	SubURL  string `json:"sub_url,omitempty"`
}

func (s *Server) onvifRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cameras/discover", s.discoverCameras)
	mux.HandleFunc("POST /api/v1/cameras/probe", s.probeCamera)
}

func (s *Server) cameraNetworks() []netip.Prefix { return s.Cfg.Discovery.CameraNetworks() }

func allowedAddr(nets []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// cameraByHost maps the host of each camera's main URL to the camera id.
func (s *Server) cameraByHost() map[string]string {
	out := map[string]string{}
	for _, e := range s.registry().List() {
		if u, err := url.Parse(e.MainURL); err == nil && u.Hostname() != "" {
			if _, ok := out[u.Hostname()]; !ok {
				out[u.Hostname()] = e.ID
			}
		}
	}
	return out
}

func (s *Server) discoverCameras(w http.ResponseWriter, r *http.Request) {
	var req DiscoverRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
	}
	timeout := 3 * time.Second
	if req.TimeoutMS != 0 {
		if req.TimeoutMS < 500 || req.TimeoutMS > 10000 {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "timeout_ms must be in [500, 10000]")
			return
		}
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	select {
	case s.discoverSlot() <- struct{}{}:
		defer func() { <-s.discoverSlot() }()
	default:
		writeError(w, http.StatusTooManyRequests, "busy", "a discovery is already running")
		return
	}
	discover := s.Discover
	if discover == nil {
		discover = onvif.DiscoverLAN
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+5*time.Second)
	defer cancel()
	devices, ifaces, err := discover(ctx, onvif.LANOptions{Timeout: timeout, Sweep: req.Sweep})
	if err != nil && len(devices) == 0 && len(ifaces) == 0 {
		s.Log.Warn("onvif discovery", "err", err)
		writeError(w, http.StatusServiceUnavailable, "discovery_failed", err.Error())
		return
	}
	nets, byHost := s.cameraNetworks(), s.cameraByHost()
	out := make([]DiscoveredDTO, 0, len(devices))
	for _, d := range devices {
		dto := DiscoveredDTO{LANDevice: d}
		if a, err := netip.ParseAddr(d.Address); err == nil {
			dto.Allowed = allowedAddr(nets, a)
		}
		dto.CameraID = byHost[d.Address]
		out = append(out, dto)
	}
	if ifaces == nil {
		ifaces = []onvif.LANInterface{}
	}
	s.audit(r.Context(), "camera.discover", "", map[string]any{"sweep": req.Sweep, "found": len(out)})
	writeJSON(w, http.StatusOK, map[string]any{"devices": out, "interfaces": ifaces, "sweep": req.Sweep})
}

// probeEndpoint resolves the request into a device-service endpoint whose
// host is an IP inside the allow-list.
func (s *Server) probeEndpoint(ctx context.Context, req ProbeRequest) (onvif.Endpoint, string, error) {
	nets := s.cameraNetworks()
	check := func(host string) (netip.Addr, error) {
		if a, err := netip.ParseAddr(host); err == nil {
			if !allowedAddr(nets, a) {
				return netip.Addr{}, fmt.Errorf("%s is outside discovery.allowed_networks", host)
			}
			return a.Unmap(), nil
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return netip.Addr{}, fmt.Errorf("cannot resolve %q", host)
		}
		for _, ip := range ips {
			if !allowedAddr(nets, ip) {
				return netip.Addr{}, fmt.Errorf("%s resolves outside discovery.allowed_networks", host)
			}
		}
		return ips[0].Unmap(), nil
	}
	if req.XAddr != "" {
		u, err := url.Parse(req.XAddr)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return onvif.Endpoint{}, "", errors.New("xaddr must be http(s)://host[:port]/path without credentials")
		}
		a, err := check(u.Hostname())
		if err != nil {
			return onvif.Endpoint{}, "", err
		}
		if p := u.Port(); p != "" {
			u.Host = net.JoinHostPort(a.String(), p)
		} else {
			u.Host = a.String()
		}
		u.RawQuery, u.Fragment = "", ""
		ep, err := onvif.ParseEndpoint(u.String())
		return ep, a.String(), err
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		return onvif.Endpoint{}, "", errors.New("host or xaddr is required")
	}
	port := req.ONVIFPort
	if port == 0 {
		port = 80
	}
	if port < 1 || port > 65535 {
		return onvif.Endpoint{}, "", errors.New("onvif_port must be in [1, 65535]")
	}
	a, err := check(host)
	if err != nil {
		return onvif.Endpoint{}, "", err
	}
	hp := a.String()
	if port != 80 {
		hp = net.JoinHostPort(hp, strconv.Itoa(port))
	}
	ep, err := onvif.ParseEndpoint("http://" + hp + "/onvif/device_service")
	return ep, a.String(), err
}

func (s *Server) probeCamera(w http.ResponseWriter, r *http.Request) {
	var req ProbeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if req.Driver != "" && req.Driver != "onvif" {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_driver", "only the onvif driver is available")
		return
	}
	ep, host, err := s.probeEndpoint(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_target", err.Error())
		return
	}
	user, pass := "", ""
	if req.Username != nil {
		user = *req.Username
		if req.Password != nil {
			pass = *req.Password
		}
	}
	if req.CameraID != "" {
		if c, ok := s.registry().Credentials(req.CameraID); ok {
			if req.Username == nil {
				user, pass = c.Username, c.Password
			} else if req.Password == nil && user == c.Username {
				pass = c.Password
			}
		}
	}
	select {
	case s.probeSlots() <- struct{}{}:
		defer func() { <-s.probeSlots() }()
	default:
		writeError(w, http.StatusTooManyRequests, "busy", "another camera test is running; retry in a few seconds")
		return
	}
	probe := s.ONVIFProbe
	if probe == nil {
		nets := s.cameraNetworks()
		probe = func(ctx context.Context, ep onvif.Endpoint, u, p string) (onvif.ProbeResult, error) {
			return onvif.ProbeDevice(ctx, onvif.GuardedTransport(nets), ep, u, p)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := probe(ctx, ep, user, pass)
	audit := map[string]any{"host": host, "credentials": user != ""}
	if err != nil {
		audit["error"] = errCode(err)
		s.audit(r.Context(), "camera.probe", "", audit)
		if errors.Is(err, onvif.ErrProbeAuth) {
			writeError(w, http.StatusUnprocessableEntity, "auth_failed", "the camera rejected the username or password (check also that ONVIF is enabled and the user has ONVIF rights)")
			return
		}
		writeError(w, http.StatusBadGateway, "probe_failed", "the camera did not answer ONVIF at "+ep.String())
		return
	}
	s.audit(r.Context(), "camera.probe", "", audit)
	now := s.now().UTC().Format(time.RFC3339)
	out := ProbeResponse{ProbeResult: res, Capabilities: make([]ProbeCapabilityDTO, 0, len(res.Capabilities))}
	for _, c := range res.Capabilities {
		out.Capabilities = append(out.Capabilities, ProbeCapabilityDTO{ProbeCapability: c, Firmware: res.Firmware, CheckedAt: now})
	}
	if out.Profiles == nil {
		out.Profiles = []onvif.ProbeProfile{}
	}
	name := strings.TrimSpace(strings.Join(nonEmpty(res.Manufacturer, res.Model), " "))
	if name == "" {
		name = "Cámara " + host
	} else {
		name += " " + host
	}
	out.ONVIFURL = ep.String()
	out.Suggested = SuggestedCamera{Name: name, MainURL: res.MainURL, SubURL: res.SubURL}
	out.CameraID = s.cameraByHost()[host]
	writeJSON(w, http.StatusOK, out)
}

func nonEmpty(v ...string) []string {
	out := v[:0:0]
	for _, s := range v {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func errCode(err error) string {
	var ce *onvif.CallError
	switch {
	case errors.Is(err, onvif.ErrProbeAuth):
		return "auth_failed"
	case errors.As(err, &ce):
		return ce.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unreachable"
	}
}

func (s *Server) discoverSlot() chan struct{} {
	s.discoverOnce.Do(func() { s.discoverSem = make(chan struct{}, 1) })
	return s.discoverSem
}
