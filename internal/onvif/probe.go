package onvif

// Device probe used by the camera add flow. Adapted from the OpenVMS edge agent
// (internal/agent/onvifdiscover/probe.go): same clock-skew compensation, Media2
// first with Media1 fallback, and main/sub selection by resolution. Unlike the
// agent it keeps the query string of stream URIs (Provision-ISR recorders put
// the channel there) and reports a capability list with evidence instead of
// raw service data.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	probeOperationTimeout = 4 * time.Second
	maxProbeProfiles      = 32
	maxProbeFieldRunes    = 256
	maxStreamURIBytes     = 2048
)

// Capability statuses, as in the Recorder API (CapabilityStatus).
const (
	CapConfirmed   = "confirmed"
	CapUnsupported = "unsupported"
	CapUnknown     = "unknown"
	CapError       = "error"
)

// ProbeProfile is one media profile of the device.
type ProbeProfile struct {
	Token        string  `json:"token"`
	Name         string  `json:"name"`
	MediaVersion string  `json:"media_version"`
	Encoding     string  `json:"encoding,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	FrameRate    float64 `json:"frame_rate,omitempty"`
	BitrateKbps  int     `json:"bitrate_kbps,omitempty"`
	StreamURI    string  `json:"stream_uri,omitempty"`
}

// ProbeCapability is one capability with the operation that justifies it.
type ProbeCapability struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	Evidence   string `json:"evidence,omitempty"`
}

// ProbeResult is what the probe learned. Stream URIs carry no credentials.
type ProbeResult struct {
	Manufacturer  string            `json:"manufacturer"`
	Model         string            `json:"model"`
	Firmware      string            `json:"firmware"`
	Serial        string            `json:"serial"`
	HardwareID    string            `json:"hardware_id,omitempty"`
	ClockOffsetMS int64             `json:"clock_offset_ms"`
	Profiles      []ProbeProfile    `json:"profiles"`
	MainURL       string            `json:"main_url,omitempty"`
	SubURL        string            `json:"sub_url,omitempty"`
	EventTopics   []string          `json:"event_topics,omitempty"`
	Analytics     []string          `json:"analytics,omitempty"`
	Capabilities  []ProbeCapability `json:"capabilities"`
}

// ProbeTransport is the default HTTP transport: no proxy, and self-signed
// certificates accepted because cameras rarely have valid ones.
func ProbeTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, // #nosec G402 -- LAN cameras use self-signed certificates.
		ResponseHeaderTimeout: probeOperationTimeout,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}
}

// ErrProbeAuth is returned when the device rejects the credentials.
var ErrProbeAuth = errors.New("the camera rejected the username or password")

// ProbeDevice runs a bounded, read-only probe of the ONVIF device service at
// endpoint. Nothing on the device is changed.
func ProbeDevice(ctx context.Context, transport http.RoundTripper, endpoint Endpoint, username, password string) (ProbeResult, error) {
	if transport == nil {
		transport = ProbeTransport()
	}
	var creds *Credentials
	if username != "" {
		creds = NewCredentials(username, password)
	}
	client := NewClient(transport, Config{Timeout: probeOperationTimeout, MaxBodyBytes: 256 << 10, MaxAttempts: 1, Credentials: creds})
	unauth := NewDeviceClient(NewClient(transport, Config{Timeout: probeOperationTimeout, MaxBodyBytes: 64 << 10, MaxAttempts: 1}), endpoint)
	device := NewDeviceClient(client, endpoint)
	var res ProbeResult
	add := func(name, status, evidence string) {
		res.Capabilities = append(res.Capabilities, ProbeCapability{Capability: name, Status: status, Evidence: evidence})
	}

	// GetSystemDateAndTime is unauthenticated by spec; WS-Security digests are
	// rejected when the clocks differ, so align to the camera before anything else.
	if clock, err := unauth.GetSystemDateAndTime(ctx); err == nil && clock.UTC.Date.Year >= 2000 {
		cam := time.Date(clock.UTC.Date.Year, time.Month(clock.UTC.Date.Month), clock.UTC.Date.Day,
			clock.UTC.Time.Hour, clock.UTC.Time.Minute, clock.UTC.Time.Second, 0, time.UTC)
		off := cam.Sub(time.Now().UTC())
		client.SetTimeOffset(off)
		res.ClockOffsetMS = off.Milliseconds()
		add("onvif.device.system_date_and_time", CapConfirmed, "GetSystemDateAndTime answered")
	} else if err != nil {
		var ce *CallError
		if !errors.As(err, &ce) {
			return res, err
		}
		add("onvif.device.system_date_and_time", CapError, "GetSystemDateAndTime: "+ce.Code)
	}

	info, err := device.GetDeviceInformation(ctx)
	if err != nil {
		var ce *CallError
		if errors.As(err, &ce) && ce.Code == "authentication_failed" {
			return res, ErrProbeAuth
		}
		if errors.As(err, &ce) && ce.Code == "transport_error" {
			return res, err
		}
		add("onvif.device.information", CapError, "GetDeviceInformation failed")
	} else {
		res.Manufacturer, res.Model = clean(info.Manufacturer), clean(info.Model)
		res.Firmware, res.Serial, res.HardwareID = clean(info.FirmwareVersion), clean(info.SerialNumber), clean(info.HardwareID)
		add("onvif.device.information", CapConfirmed, "GetDeviceInformation answered")
	}

	services, err := device.GetServices(ctx)
	if err != nil || len(services) == 0 {
		if caps, capsErr := device.GetCapabilities(ctx); capsErr == nil && len(caps) > 0 {
			services = caps
			add("onvif.device.services", CapConfirmed, "GetServices unavailable; GetCapabilities answered")
		} else {
			var ce *CallError
			if errors.As(err, &ce) && ce.Code == "authentication_failed" {
				return res, ErrProbeAuth
			}
			add("onvif.device.services", CapError, "neither GetServices nor GetCapabilities answered")
		}
	} else {
		add("onvif.device.services", CapConfirmed, "GetServices answered")
	}

	host := endpointHost(endpoint)
	profiles, version, perr := probeMediaProfiles(ctx, client, services)
	switch {
	case perr != nil && errors.Is(perr, ErrProbeAuth):
		return res, ErrProbeAuth
	case perr != nil:
		add("onvif.media.profiles", CapError, perr.Error())
	case len(profiles) == 0:
		add("onvif.media.profiles", CapUnknown, "no media profiles reported")
	default:
		add("onvif.media"+version+".profiles", CapConfirmed, "GetProfiles returned profiles")
	}
	withURI := 0
	for i := range profiles {
		profiles[i].StreamURI = sanitizeStreamURI(profiles[i].StreamURI, host, password)
		if profiles[i].StreamURI != "" {
			withURI++
		}
	}
	if len(profiles) > 0 {
		if withURI > 0 {
			add("onvif.media.stream_uri", CapConfirmed, "GetStreamUri answered for profiles")
		} else {
			add("onvif.media.stream_uri", CapError, "GetStreamUri gave no usable rtsp:// URI")
		}
	}
	res.Profiles = profiles
	res.MainURL, res.SubURL = pickStreams(profiles)

	for _, s := range services {
		switch {
		case strings.Contains(s.Namespace, "/ptz/"):
			add("onvif.ptz", CapUnknown, "advertised in the service list, not exercised")
		case strings.Contains(s.Namespace, "/events/"):
			ep, epErr := s.ValidatedEndpoint()
			if epErr != nil {
				continue
			}
			props, evErr := NewEventsClient(client, ep).GetEventProperties(ctx)
			if evErr != nil {
				add("onvif.events.properties", CapError, "GetEventProperties failed")
				continue
			}
			add("onvif.events.properties", CapConfirmed, "GetEventProperties answered")
			res.EventTopics = props.TopicSet
			for _, a := range DetectAnalytics(props.TopicSet) {
				res.Analytics = append(res.Analytics, a.ID)
			}
		case strings.Contains(s.Namespace, "/recording/") || strings.Contains(s.Namespace, "/replay/"):
			add("onvif.profile_g."+lastPathPart(s.Namespace), CapUnknown, "advertised in the service list, not exercised")
		}
	}
	return res, nil
}

func lastPathPart(ns string) string {
	ns = strings.TrimSuffix(ns, "/wsdl")
	if i := strings.LastIndex(ns, "/ver"); i >= 0 {
		ns = ns[:i]
	}
	return ns[strings.LastIndex(ns, "/")+1:]
}

func probeMediaProfiles(ctx context.Context, client *Client, services []Service) ([]ProbeProfile, string, error) {
	var media1, media2 *Service
	for i := range services {
		switch {
		case strings.Contains(services[i].Namespace, "/ver20/media/") && media2 == nil:
			media2 = &services[i]
		case strings.Contains(services[i].Namespace, "/ver10/media/") && media1 == nil:
			media1 = &services[i]
		}
	}
	authErr := func(err error) bool {
		var ce *CallError
		return errors.As(err, &ce) && ce.Code == "authentication_failed"
	}
	if media2 != nil {
		if endpoint, err := media2.ValidatedEndpoint(); err == nil {
			media := NewMedia2Client(client, endpoint)
			profiles, err := media.GetProfiles(ctx)
			if authErr(err) {
				return nil, "", ErrProbeAuth
			}
			if err == nil && len(profiles) > 0 {
				out := make([]ProbeProfile, 0, min(len(profiles), maxProbeProfiles))
				for _, profile := range profiles[:min(len(profiles), maxProbeProfiles)] {
					p := ProbeProfile{Token: clean(profile.Token), Name: clean(profile.Name), MediaVersion: "2.0"}
					if cfg := profile.VideoEncoderConfiguration; cfg != nil {
						p.Encoding, p.Width, p.Height = clean(cfg.Encoding), cfg.Resolution.Width, cfg.Resolution.Height
						p.FrameRate, p.BitrateKbps = cfg.RateControl.FrameRateLimit, cfg.RateControl.BitrateLimit
					}
					if uri, uriErr := media.GetStreamURI(ctx, profile.Token); uriErr == nil {
						p.StreamURI = uri
					}
					out = append(out, p)
				}
				return out, "2", nil
			}
		}
	}
	if media1 == nil {
		if media2 != nil {
			return nil, "", errors.New("Media2 GetProfiles failed and no Media1 service")
		}
		return nil, "", errors.New("no media service advertised")
	}
	endpoint, err := media1.ValidatedEndpoint()
	if err != nil {
		return nil, "", errors.New("invalid media service address")
	}
	media := NewMediaClient(client, endpoint)
	profiles, err := media.GetProfiles(ctx)
	if authErr(err) {
		return nil, "", ErrProbeAuth
	}
	if err != nil {
		return nil, "", errors.New("Media GetProfiles failed")
	}
	out := make([]ProbeProfile, 0, min(len(profiles), maxProbeProfiles))
	for _, profile := range profiles[:min(len(profiles), maxProbeProfiles)] {
		p := ProbeProfile{Token: clean(profile.Token), Name: clean(profile.Name), MediaVersion: "1.0"}
		if cfg := profile.VideoEncoderConfiguration; cfg != nil {
			p.Encoding, p.Width, p.Height = clean(cfg.Encoding), cfg.Resolution.Width, cfg.Resolution.Height
			p.FrameRate, p.BitrateKbps = float64(cfg.RateControl.FrameRateLimit), cfg.RateControl.BitrateLimit
		}
		if uri, uriErr := media.GetStreamURI(ctx, profile.Token); uriErr == nil {
			p.StreamURI = uri
		}
		out = append(out, p)
	}
	return out, "", nil
}

// pickStreams chooses the highest-resolution profile as main and the next
// lower resolution with a different URI as sub. Ties keep device order.
func pickStreams(profiles []ProbeProfile) (main, sub string) {
	idx := make([]int, 0, len(profiles))
	for i, p := range profiles {
		if p.StreamURI != "" {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return "", ""
	}
	sort.SliceStable(idx, func(a, b int) bool {
		pa, pb := profiles[idx[a]], profiles[idx[b]]
		return pa.Width*pa.Height > pb.Width*pb.Height
	})
	m := profiles[idx[0]]
	for _, i := range idx[1:] {
		p := profiles[i]
		if p.StreamURI != m.StreamURI && (p.Width*p.Height < m.Width*m.Height || m.Width*m.Height == 0) {
			return m.StreamURI, p.StreamURI
		}
	}
	return m.StreamURI, ""
}

func endpointHost(e Endpoint) string {
	u, err := url.Parse(e.String())
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// sanitizeStreamURI drops credentials from a device-provided URI, rewrites
// unusable hosts (0.0.0.0, loopback) to the probed host and rejects URIs that
// still contain a secret (some devices put user/password in the query).
func sanitizeStreamURI(raw, probedHost string, secrets ...string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxStreamURIBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") {
		return ""
	}
	u.User = nil
	u.Fragment = ""
	if h := u.Hostname(); probedHost != "" {
		ip := net.ParseIP(h)
		if h == "" || h == "localhost" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())) {
			if p := u.Port(); p != "" {
				u.Host = net.JoinHostPort(probedHost, p)
			} else {
				u.Host = probedHost
			}
		}
	}
	if u.Hostname() == "" {
		return ""
	}
	if q := u.Query(); len(q) > 0 {
		changed := false
		for k := range q {
			switch strings.ToLower(k) {
			case "user", "username", "usr", "password", "pass", "pwd", "passwd":
				q.Del(k)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	out := u.String()
	for _, s := range secrets {
		if len(s) >= 4 && (strings.Contains(out, s) || strings.Contains(out, url.QueryEscape(s))) {
			return ""
		}
	}
	return out
}

func clean(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var b strings.Builder
	n := 0
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		if n >= maxProbeFieldRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
