// Package onvif implements a bounded, read-only ONVIF SOAP client,
// WS-Discovery multicast probe, media profile lookup, and PullPoint event
// subscription without external dependencies.
package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" // WS-Security UsernameToken PasswordDigest requires SHA-1.
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	soapNamespace   = "http://www.w3.org/2003/05/soap-envelope"
	deviceNamespace = "http://www.onvif.org/ver10/device/wsdl"
	mediaNamespace  = "http://www.onvif.org/ver10/media/wsdl"
	media2Namespace = "http://www.onvif.org/ver20/media/wsdl"
	eventsNamespace = "http://www.onvif.org/ver10/events/wsdl"
	schemaNamespace = "http://www.onvif.org/ver10/schema"
	wstopNamespace  = "http://docs.oasis-open.org/wsn/t-1"

	passwordDigestType = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
	base64BinaryType   = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary"
)

var (
	ErrResponseTooLarge = errors.New("onvif response exceeds configured limit")
	ErrInvalidEndpoint  = errors.New("invalid onvif endpoint")
)

// Endpoint is a validated, credential-free HTTP(S) service URL.
type Endpoint struct{ value string }

func (e Endpoint) String() string { return e.value }

// ParseEndpoint validates raw as an http/https URL without credentials.
func ParseEndpoint(raw string) (Endpoint, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n\t") {
		return Endpoint{}, ErrInvalidEndpoint
	}
	return Endpoint{value: u.String()}, nil
}

// EndpointForHost builds the standard ONVIF device_service endpoint for host and port.
func EndpointForHost(host string, port int) (Endpoint, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\:@ \t\r\n") {
		return Endpoint{}, ErrInvalidEndpoint
	}
	if port <= 0 {
		port = 80
	}
	hp := host
	if port != 80 {
		hp = net.JoinHostPort(host, fmt.Sprintf("%d", port))
	} else if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		hp = "[" + host + "]"
	}
	return ParseEndpoint("http://" + hp + "/onvif/device_service")
}

// RewriteToHost keeps only the path of a device-supplied XAddr and binds it to
// the configured camera host, preventing SSRF or credential leakage to another IP.
func RewriteToHost(rawXAddr, targetHost string, fallbackPort int) (Endpoint, error) {
	u, err := url.Parse(strings.TrimSpace(rawXAddr))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return Endpoint{}, ErrInvalidEndpoint
	}
	port := u.Port()
	if port == "" && fallbackPort > 0 && fallbackPort != 80 && fallbackPort != 443 {
		port = fmt.Sprintf("%d", fallbackPort)
	}
	hp := targetHost
	if port != "" {
		hp = net.JoinHostPort(targetHost, port)
	}
	path := u.Path
	if path == "" {
		path = "/onvif/device_service"
	}
	raw := u.Scheme + "://" + hp + path
	if u.RawQuery != "" {
		raw += "?" + u.RawQuery
	}
	return ParseEndpoint(raw)
}

// Status classifies the outcome of an ONVIF SOAP call.
type Status string

const (
	StatusSupported   Status = "supported"
	StatusUnsupported Status = "unsupported"
	StatusDegraded    Status = "degraded"
	StatusFailed      Status = "failed"
)

// CallError exposes redacted failure details.
type CallError struct {
	Status Status
	Code   string
	Reason string
	cause  error
}

func (e *CallError) Error() string {
	if e.Code != "" && e.Reason != "" {
		return fmt.Sprintf("onvif %s: %s (%s)", e.Status, e.Code, e.Reason)
	}
	if e.Code != "" {
		return fmt.Sprintf("onvif %s: %s", e.Status, e.Code)
	}
	return fmt.Sprintf("onvif %s", e.Status)
}

func (e *CallError) Unwrap() error { return e.cause }

// Credentials holds an ONVIF UsernameToken identity without exposing the password.
type Credentials struct{ username, password string }

func NewCredentials(username, password string) *Credentials {
	if username == "" {
		return nil
	}
	return &Credentials{username: username, password: password}
}

func (*Credentials) String() string   { return "onvif.Credentials{REDACTED}" }
func (*Credentials) GoString() string { return "onvif.Credentials{REDACTED}" }

// Config configures the ONVIF SOAP client.
type Config struct {
	Timeout      time.Duration
	MaxBodyBytes int64
	Credentials  *Credentials
	TimeOffset   time.Duration
	NonceSource  io.Reader
	Now          func() time.Time
}

// Client executes SOAP 1.2 requests against ONVIF endpoints.
type Client struct {
	transport   http.RoundTripper
	config      Config
	nonceSource io.Reader
	now         func() time.Time
}

func NewClient(transport http.RoundTripper, cfg Config) *Client {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 4 * time.Second
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 512 << 10
	}
	nonce := cfg.NonceSource
	if nonce == nil {
		nonce = rand.Reader
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{transport: transport, config: cfg, nonceSource: nonce, now: now}
}

func (c *Client) SetTimeOffset(d time.Duration) { c.config.TimeOffset = d }
func (c *Client) TimeOffset() time.Duration     { return c.config.TimeOffset }

type securityHeader struct {
	XMLName        xml.Name      `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Security"`
	MustUnderstand string        `xml:"http://www.w3.org/2003/05/soap-envelope mustUnderstand,attr"`
	Token          usernameToken `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd UsernameToken"`
}

type usernameToken struct {
	Username string          `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Username"`
	Password passwordElement `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Password"`
	Nonce    nonceElement    `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd Nonce"`
	Created  createdElement  `xml:"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd Created"`
}

type passwordElement struct {
	Type  string `xml:"Type,attr"`
	Value string `xml:",chardata"`
}

type nonceElement struct {
	EncodingType string `xml:"EncodingType,attr"`
	Value        string `xml:",chardata"`
}

type createdElement struct {
	Value string `xml:",chardata"`
}

func (c *Client) requestBody(operationXML, extraHeader string) ([]byte, error) {
	body := []byte(`<s:Envelope xmlns:s="` + soapNamespace + `"><s:Body>` + operationXML + `</s:Body></s:Envelope>`)
	var secPart string
	if c.config.Credentials != nil && c.config.Credentials.username != "" {
		creds := c.config.Credentials
		var nonce [20]byte
		if _, err := io.ReadFull(c.nonceSource, nonce[:]); err != nil {
			return nil, errors.New("onvif nonce unavailable")
		}
		created := c.now().Add(c.config.TimeOffset).UTC().Format("2006-01-02T15:04:05Z")
		h := sha1.New()
		_, _ = h.Write(nonce[:])
		_, _ = io.WriteString(h, created)
		_, _ = io.WriteString(h, creds.password)
		sec := securityHeader{
			MustUnderstand: "true",
			Token: usernameToken{
				Username: creds.username,
				Password: passwordElement{Type: passwordDigestType, Value: base64.StdEncoding.EncodeToString(h.Sum(nil))},
				Nonce:    nonceElement{EncodingType: base64BinaryType, Value: base64.StdEncoding.EncodeToString(nonce[:])},
				Created:  createdElement{Value: created},
			},
		}
		secXML, err := xml.Marshal(sec)
		if err != nil {
			return nil, err
		}
		secPart = string(secXML)
	}
	if secPart == "" && extraHeader == "" {
		return body, nil
	}
	header := `<s:Header>` + extraHeader + secPart + `</s:Header>`
	return bytes.Replace(body, []byte(`<s:Body>`), []byte(header+`<s:Body>`), 1), nil
}

type soapEnvelope struct {
	XMLName xml.Name `xml:"http://www.w3.org/2003/05/soap-envelope Envelope"`
	Body    struct {
		Inner string `xml:",innerxml"`
	} `xml:"http://www.w3.org/2003/05/soap-envelope Body"`
}

type soapFault struct {
	Code struct {
		Value   string `xml:"Value"`
		Subcode struct {
			Value string `xml:"Value"`
			Sub   struct {
				Value string `xml:"Value"`
			} `xml:"Subcode"`
		} `xml:"Subcode"`
	} `xml:"Code"`
	Reason struct {
		Text string `xml:"Text"`
	} `xml:"Reason"`
}

// Call executes one SOAP operation against endpoint.
func (c *Client) Call(ctx context.Context, endpoint Endpoint, action, operationXML string) ([]byte, error) {
	return c.call(ctx, endpoint, action, operationXML, "")
}

// CallWithAddressing executes one SOAP operation with WS-Addressing To and Action headers.
func (c *Client) CallWithAddressing(ctx context.Context, endpoint Endpoint, wsaAction, wsaTo, operationXML string) ([]byte, error) {
	var escAction, escTo bytes.Buffer
	_ = xml.EscapeText(&escAction, []byte(wsaAction))
	_ = xml.EscapeText(&escTo, []byte(wsaTo))
	hdr := `<wsa5:Action xmlns:wsa5="http://www.w3.org/2005/08/addressing" s:mustUnderstand="1">` + escAction.String() + `</wsa5:Action>` +
		`<wsa5:To xmlns:wsa5="http://www.w3.org/2005/08/addressing" s:mustUnderstand="1">` + escTo.String() + `</wsa5:To>`
	return c.call(ctx, endpoint, wsaAction, operationXML, hdr)
}

func (c *Client) call(ctx context.Context, endpoint Endpoint, action, operationXML, extraHeader string) ([]byte, error) {
	if endpoint.value == "" {
		return nil, ErrInvalidEndpoint
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	body, err := c.requestBody(operationXML, extraHeader)
	if err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "authentication_failed"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.value, bytes.NewReader(body))
	if err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "request_failed", cause: err}
	}
	req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8; action="`+action+`"`)
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &CallError{Status: StatusDegraded, Code: "timeout", cause: ctx.Err()}
		}
		return nil, &CallError{Status: StatusDegraded, Code: "transport_error", cause: err}
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, c.config.MaxBodyBytes+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, &CallError{Status: StatusFailed, Code: "response_read_failed", cause: readErr}
	}
	if int64(len(data)) > c.config.MaxBodyBytes {
		return nil, ErrResponseTooLarge
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &CallError{Status: StatusFailed, Code: "authentication_failed"}
	}

	var env soapEnvelope
	if err := xml.Unmarshal(data, &env); err != nil || strings.TrimSpace(env.Body.Inner) == "" {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &CallError{Status: StatusFailed, Code: fmt.Sprintf("http_%d", resp.StatusCode)}
		}
		return nil, &CallError{Status: StatusFailed, Code: "invalid_soap_response"}
	}

	var fault soapFault
	if err := xml.Unmarshal([]byte(env.Body.Inner), &fault); err == nil && (fault.Code.Value != "" || fault.Code.Subcode.Value != "") {
		code := fault.Code.Subcode.Sub.Value
		if code == "" {
			code = fault.Code.Subcode.Value
		}
		if code == "" {
			code = fault.Code.Value
		}
		lower := strings.ToLower(code + " " + fault.Reason.Text)
		if strings.Contains(lower, "notauthorized") || strings.Contains(lower, "failedauthentication") || strings.Contains(lower, "securitytoken") || strings.Contains(lower, "sender") && strings.Contains(lower, "auth") {
			return nil, &CallError{Status: StatusFailed, Code: "authentication_failed", Reason: fault.Reason.Text}
		}
		status := StatusFailed
		if strings.Contains(lower, "unsupported") || strings.Contains(lower, "actionnotsupported") || strings.Contains(lower, "capabilities") {
			status = StatusUnsupported
		}
		return nil, &CallError{Status: status, Code: "soap_fault", Reason: fault.Reason.Text}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &CallError{Status: StatusFailed, Code: fmt.Sprintf("http_%d", resp.StatusCode)}
	}
	return bytes.Clone(data), nil
}

// DeviceInformation holds ONVIF GetDeviceInformation fields.
type DeviceInformation struct {
	Manufacturer    string `xml:"Manufacturer" json:"manufacturer"`
	Model           string `xml:"Model" json:"model"`
	FirmwareVersion string `xml:"FirmwareVersion" json:"firmware"`
	SerialNumber    string `xml:"SerialNumber" json:"serial"`
	HardwareID      string `xml:"HardwareId" json:"hardware_id"`
}

// Service describes one ONVIF service advertised by the device.
type Service struct {
	Namespace string `xml:"Namespace" json:"namespace"`
	Endpoint  string `xml:"XAddr" json:"endpoint"`
}

// DeviceClock holds the UTC date/time reported by GetSystemDateAndTime.
type DeviceClock struct {
	DateTimeType string `xml:"DateTimeType"`
	UTC          struct {
		Time struct {
			Hour   int `xml:"Hour"`
			Minute int `xml:"Minute"`
			Second int `xml:"Second"`
		} `xml:"Time"`
		Date struct {
			Year  int `xml:"Year"`
			Month int `xml:"Month"`
			Day   int `xml:"Day"`
		} `xml:"Date"`
	} `xml:"UTCDateTime"`
}

// Time returns the parsed UTC time, or zero if not valid.
func (c DeviceClock) Time() time.Time {
	d := c.UTC.Date
	t := c.UTC.Time
	if d.Year < 2000 || d.Month < 1 || d.Month > 12 || d.Day < 1 || d.Day > 31 {
		return time.Time{}
	}
	return time.Date(d.Year, time.Month(d.Month), d.Day, t.Hour, t.Minute, t.Second, 0, time.UTC)
}

// DeviceClient provides ONVIF Device management operations.
type DeviceClient struct {
	client   *Client
	endpoint Endpoint
}

func NewDeviceClient(client *Client, endpoint Endpoint) *DeviceClient {
	return &DeviceClient{client: client, endpoint: endpoint}
}

func (d *DeviceClient) GetDeviceInformation(ctx context.Context) (DeviceInformation, error) {
	data, err := d.call(ctx, "GetDeviceInformation", "")
	if err != nil {
		return DeviceInformation{}, err
	}
	var info DeviceInformation
	if err := decodeLocalOperation(data, "GetDeviceInformationResponse", &info); err != nil {
		return DeviceInformation{}, &CallError{Status: StatusFailed, Code: "invalid_device_information"}
	}
	return info, nil
}

func (d *DeviceClient) GetServices(ctx context.Context) ([]Service, error) {
	data, err := d.call(ctx, "GetServices", `<tds:IncludeCapability>false</tds:IncludeCapability>`)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Services []Service `xml:"Service"`
	}
	if err := decodeLocalOperation(data, "GetServicesResponse", &resp); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_services"}
	}
	return resp.Services, nil
}

func (d *DeviceClient) GetCapabilities(ctx context.Context) ([]Service, error) {
	data, err := d.call(ctx, "GetCapabilities", `<tds:Category>All</tds:Category>`)
	if err != nil {
		return nil, err
	}
	type capEP struct {
		XAddr string `xml:"XAddr"`
	}
	var resp struct {
		Capabilities struct {
			Device    *capEP `xml:"Device"`
			Events    *capEP `xml:"Events"`
			Imaging   *capEP `xml:"Imaging"`
			Media     *capEP `xml:"Media"`
			PTZ       *capEP `xml:"PTZ"`
			Analytics *capEP `xml:"Analytics"`
		} `xml:"Capabilities"`
	}
	if err := decodeLocalOperation(data, "GetCapabilitiesResponse", &resp); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_capabilities"}
	}
	c := resp.Capabilities
	mapping := []struct {
		ep *capEP
		ns string
	}{
		{c.Media, mediaNamespace},
		{c.Device, deviceNamespace},
		{c.Events, eventsNamespace},
		{c.PTZ, "http://www.onvif.org/ver20/ptz/wsdl"},
		{c.Analytics, "http://www.onvif.org/ver20/analytics/wsdl"},
	}
	var out []Service
	for _, m := range mapping {
		if m.ep != nil && strings.TrimSpace(m.ep.XAddr) != "" {
			out = append(out, Service{Namespace: m.ns, Endpoint: strings.TrimSpace(m.ep.XAddr)})
		}
	}
	return out, nil
}

func (d *DeviceClient) GetSystemDateAndTime(ctx context.Context) (DeviceClock, error) {
	data, err := d.call(ctx, "GetSystemDateAndTime", "")
	if err != nil {
		return DeviceClock{}, err
	}
	var resp struct {
		Clock DeviceClock `xml:"SystemDateAndTime"`
	}
	if err := decodeLocalOperation(data, "GetSystemDateAndTimeResponse", &resp); err != nil {
		return DeviceClock{}, &CallError{Status: StatusFailed, Code: "invalid_system_time"}
	}
	return resp.Clock, nil
}

func (d *DeviceClient) call(ctx context.Context, action, content string) ([]byte, error) {
	body := `<tds:` + action + ` xmlns:tds="` + deviceNamespace + `">` + content + `</tds:` + action + `>`
	return d.client.Call(ctx, d.endpoint, deviceNamespace+"/"+action, body)
}

// MediaProfile represents one ONVIF Media1 or Media2 stream profile.
type MediaProfile struct {
	Token        string  `json:"token"`
	Name         string  `json:"name"`
	MediaVersion string  `json:"media_version"`
	Encoding     string  `json:"encoding"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	FPS          float64 `json:"fps"`
	BitrateKbps  int     `json:"bitrate_kbps"`
	HasAudio     bool    `json:"has_audio"`
	HasPTZ       bool    `json:"has_ptz"`
	StreamURI    string  `json:"stream_uri,omitempty"`
}

// MediaClient queries Media1 and Media2 profiles and stream URIs.
type MediaClient struct {
	client   *Client
	endpoint Endpoint
	isMedia2 bool
}

func NewMediaClient(client *Client, endpoint Endpoint, isMedia2 bool) *MediaClient {
	return &MediaClient{client: client, endpoint: endpoint, isMedia2: isMedia2}
}

func (m *MediaClient) GetProfiles(ctx context.Context) ([]MediaProfile, error) {
	ns := mediaNamespace
	prefix := "trt"
	content := ""
	ver := "1.0"
	if m.isMedia2 {
		ns = media2Namespace
		prefix = "tr2"
		content = `<tr2:Type>All</tr2:Type>`
		ver = "2.0"
	}
	body := `<` + prefix + `:GetProfiles xmlns:` + prefix + `="` + ns + `">` + content + `</` + prefix + `:GetProfiles>`
	data, err := m.client.Call(ctx, m.endpoint, ns+"/GetProfiles", body)
	if err != nil {
		return nil, err
	}
	type videoEnc struct {
		Encoding   string `xml:"Encoding"`
		Resolution struct {
			Width  int `xml:"Width"`
			Height int `xml:"Height"`
		} `xml:"Resolution"`
		RateControl struct {
			FrameRateLimit float64 `xml:"FrameRateLimit"`
			BitrateLimit   int     `xml:"BitrateLimit"`
		} `xml:"RateControl"`
	}
	type rawProf struct {
		Token          string `xml:"token,attr"`
		Name           string `xml:"Name"`
		Configurations *struct {
			Video *videoEnc `xml:"VideoEncoder"`
			Audio *struct{} `xml:"AudioEncoder"`
			PTZ   *struct{} `xml:"PTZ"`
		} `xml:"Configurations"`
		Video *videoEnc `xml:"VideoEncoderConfiguration"`
		Audio *struct{} `xml:"AudioEncoderConfiguration"`
		PTZ   *struct{} `xml:"PTZConfiguration"`
	}
	var resp struct {
		Profiles []rawProf `xml:"Profiles"`
	}
	if err := decodeLocalOperation(data, "GetProfilesResponse", &resp); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_profiles"}
	}
	out := make([]MediaProfile, 0, len(resp.Profiles))
	for _, rp := range resp.Profiles {
		p := MediaProfile{
			Token:        strings.TrimSpace(rp.Token),
			Name:         strings.TrimSpace(rp.Name),
			MediaVersion: ver,
			HasAudio:     rp.Audio != nil,
			HasPTZ:       rp.PTZ != nil,
		}
		v := rp.Video
		if rp.Configurations != nil {
			if rp.Configurations.Video != nil {
				v = rp.Configurations.Video
			}
			if rp.Configurations.Audio != nil {
				p.HasAudio = true
			}
			if rp.Configurations.PTZ != nil {
				p.HasPTZ = true
			}
		}
		if v != nil {
			p.Encoding = strings.TrimSpace(v.Encoding)
			p.Width = v.Resolution.Width
			p.Height = v.Resolution.Height
			p.FPS = v.RateControl.FrameRateLimit
			p.BitrateKbps = v.RateControl.BitrateLimit
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *MediaClient) GetStreamURI(ctx context.Context, profileToken string) (string, error) {
	var body, ns string
	if m.isMedia2 {
		ns = media2Namespace
		body = `<tr2:GetStreamUri xmlns:tr2="` + media2Namespace + `"><tr2:Protocol>RTSP</tr2:Protocol><tr2:ProfileToken>` + escapeXML(profileToken) + `</tr2:ProfileToken></tr2:GetStreamUri>`
	} else {
		ns = mediaNamespace
		body = `<trt:GetStreamUri xmlns:trt="` + mediaNamespace + `">` +
			`<trt:StreamSetup><tt:Stream xmlns:tt="` + schemaNamespace + `">RTP-Unicast</tt:Stream>` +
			`<tt:Transport xmlns:tt="` + schemaNamespace + `"><tt:Protocol>RTSP</tt:Protocol></tt:Transport></trt:StreamSetup>` +
			`<trt:ProfileToken>` + escapeXML(profileToken) + `</trt:ProfileToken></trt:GetStreamUri>`
	}
	data, err := m.client.Call(ctx, m.endpoint, ns+"/GetStreamUri", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		URI      string `xml:"Uri"`
		MediaURI struct {
			URI string `xml:"Uri"`
		} `xml:"MediaUri"`
	}
	if err := decodeLocalOperation(data, "GetStreamUriResponse", &resp); err != nil {
		return "", &CallError{Status: StatusFailed, Code: "invalid_stream_uri"}
	}
	uri := strings.TrimSpace(resp.URI)
	if uri == "" {
		uri = strings.TrimSpace(resp.MediaURI.URI)
	}
	return SanitizeRTSPURI(uri), nil
}

// SanitizeRTSPURI removes credentials, fragments, and control characters from an RTSP URI.
func SanitizeRTSPURI(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.Fragment = ""
	return u.String()
}

// EventsClient provides ONVIF GetEventProperties and PullPoint operations.
type EventsClient struct {
	client   *Client
	endpoint Endpoint
}

func NewEventsClient(client *Client, endpoint Endpoint) *EventsClient {
	return &EventsClient{client: client, endpoint: endpoint}
}

// AnalyticsDetection captures object-level metadata decoded from ONVIF
// VideoAnalytics frames or ElementItem descriptors (Profile M / T).
type AnalyticsDetection struct {
	ObjectID   string    `json:"object_id,omitempty"`
	Class      string    `json:"class,omitempty"`
	Confidence *float64  `json:"confidence,omitempty"`
	BBox       []float64 `json:"bbox,omitempty"` // normalized [x, y, w, h] in [0, 1]
}

type EventNotification struct {
	Topic      string               `json:"topic"`
	Timestamp  time.Time            `json:"timestamp"`
	Data       map[string]string    `json:"data"`
	Detections []AnalyticsDetection `json:"detections,omitempty"`
}

func (e *EventsClient) GetEventProperties(ctx context.Context) ([]string, error) {
	body := `<tev:GetEventProperties xmlns:tev="` + eventsNamespace + `"/>`
	data, err := e.client.Call(ctx, e.endpoint, eventsNamespace+"/GetEventProperties", body)
	if err != nil {
		return nil, err
	}
	return parseTopicSet(data), nil
}

func (e *EventsClient) CreatePullPointSubscription(ctx context.Context, initialTermination string) (string, error) {
	if initialTermination == "" {
		initialTermination = "PT10M"
	}
	body := `<tev:CreatePullPointSubscription xmlns:tev="` + eventsNamespace + `"><tev:InitialTerminationTime>` + initialTermination + `</tev:InitialTerminationTime></tev:CreatePullPointSubscription>`
	data, err := e.client.Call(ctx, e.endpoint, eventsNamespace+"/CreatePullPointSubscription", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		SubRef struct {
			Address string `xml:"Address"`
		} `xml:"SubscriptionReference"`
	}
	if err := decodeLocalOperation(data, "CreatePullPointSubscriptionResponse", &resp); err != nil || strings.TrimSpace(resp.SubRef.Address) == "" {
		return "", &CallError{Status: StatusFailed, Code: "invalid_pullpoint_subscription"}
	}
	return strings.TrimSpace(resp.SubRef.Address), nil
}

func (e *EventsClient) PullMessages(ctx context.Context, subEndpoint Endpoint, timeoutSeconds, limit int) ([]EventNotification, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	if limit <= 0 {
		limit = 20
	}
	body := fmt.Sprintf(`<tev:PullMessages xmlns:tev="%s"><tev:Timeout>PT%dS</tev:Timeout><tev:MessageLimit>%d</tev:MessageLimit></tev:PullMessages>`,
		eventsNamespace, timeoutSeconds, limit)
	data, err := e.client.CallWithAddressing(ctx, subEndpoint, eventsNamespace+"/PullPointSubscription/PullMessagesRequest", subEndpoint.String(), body)
	if err != nil {
		return nil, err
	}
	var notifs []EventNotification
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "NotificationMessage" {
			continue
		}
		type msgBody struct {
			UtcTime    string `xml:"UtcTime,attr"`
			PropertyOp string `xml:"PropertyOperation,attr"`
			Data       struct {
				SimpleItems []struct {
					Name  string `xml:"Name,attr"`
					Value string `xml:"Value,attr"`
				} `xml:"SimpleItem"`
				InnerXML string `xml:",innerxml"`
			} `xml:"Data"`
			SourceItems []struct {
				Name  string `xml:"Name,attr"`
				Value string `xml:"Value,attr"`
			} `xml:"Source>SimpleItem"`
		}
		type msgWrapper struct {
			msgBody
			Inner *msgBody `xml:"Message"`
		}
		var msg struct {
			Topic   string     `xml:"Topic"`
			Message msgWrapper `xml:"Message"`
		}
		if err := dec.DecodeElement(&msg, &se); err == nil {
			mb := msg.Message.msgBody
			if msg.Message.Inner != nil {
				mb = *msg.Message.Inner
			}
			d := make(map[string]string)
			for _, it := range mb.SourceItems {
				d[it.Name] = it.Value
			}
			for _, it := range mb.Data.SimpleItems {
				d[it.Name] = it.Value
			}
			if mb.PropertyOp != "" {
				d["_property_op"] = mb.PropertyOp
			}
			dets := parseAnalyticsXML(mb.Data.InnerXML, d)
			ts, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(mb.UtcTime))
			notifs = append(notifs, EventNotification{
				Topic:      strings.TrimSpace(msg.Topic),
				Timestamp:  ts,
				Data:       d,
				Detections: dets,
			})
		}
	}
	return notifs, nil
}

// Unsubscribe cleanly releases a PullPoint subscription on the camera so
// restarts do not exhaust the camera's NotificationProducer slots.
func (e *EventsClient) Unsubscribe(ctx context.Context, subEndpoint Endpoint) error {
	if subEndpoint.value == "" {
		return nil
	}
	const wsnBW2 = "http://docs.oasis-open.org/wsn/bw-2"
	body := `<wsnt:Unsubscribe xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"/>`
	_, err := e.client.CallWithAddressing(ctx, subEndpoint, wsnBW2+"/SubscriptionManager/UnsubscribeRequest", subEndpoint.String(), body)
	return err
}

const maxEventTopics = 200

func parseTopicSet(data []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		path     []string
		inSet    bool
		setDepth int
		depth    int
		seen     = map[string]bool{}
		topics   []string
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if !inSet {
				if t.Name.Local == "TopicSet" {
					inSet, setDepth = true, depth
				}
				continue
			}
			path = append(path, t.Name.Local)
			if !isTopicElement(t) {
				continue
			}
			topic := strings.Join(path, "/")
			if len(topic) <= 200 && !seen[topic] && len(topics) < maxEventTopics {
				seen[topic] = true
				topics = append(topics, topic)
			}
		case xml.EndElement:
			if inSet && depth == setDepth {
				inSet, path = false, nil
			} else if inSet && len(path) > 0 {
				path = path[:len(path)-1]
			}
			depth--
		}
	}
	sort.Strings(topics)
	return topics
}

func isTopicElement(el xml.StartElement) bool {
	for _, attr := range el.Attr {
		if attr.Name.Local == "topic" && (attr.Name.Space == wstopNamespace || attr.Name.Space == "") && strings.EqualFold(attr.Value, "true") {
			return true
		}
	}
	return false
}

func decodeLocalOperation(data []byte, localName string, target any) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("missing SOAP element %s", localName)
			}
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != localName {
			continue
		}
		return dec.DecodeElement(target, &se)
	}
}

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// parseAnalyticsXML extracts VideoAnalytics objects, ElementItem bounding boxes,
// and object classification metadata from an ONVIF NotificationMessage body.
func parseAnalyticsXML(rawXML string, flat map[string]string) []AnalyticsDetection {
	if strings.TrimSpace(rawXML) == "" {
		return nil
	}
	type xmlBBox struct {
		Left   string `xml:"left,attr"`
		Top    string `xml:"top,attr"`
		Right  string `xml:"right,attr"`
		Bottom string `xml:"bottom,attr"`
	}
	type xmlCandidate struct {
		Type       string `xml:"Type"`
		Likelihood string `xml:"Likelihood,attr"`
	}
	type xmlObject struct {
		ObjectID string `xml:"ObjectId,attr"`
		Shape    struct {
			BoundingBox xmlBBox `xml:"BoundingBox"`
		} `xml:"Appearance>Shape"`
		AppearanceClass struct {
			Candidate []xmlCandidate `xml:"ClassCandidate"`
			Type      string         `xml:"Type"`
		} `xml:"Appearance>Class"`
		ClassExt struct {
			ObjectClassification struct {
				Type       string `xml:"Type"`
				Likelihood string `xml:"Likelihood"`
			} `xml:"ObjectClassification"`
		} `xml:"Extension"`
	}

	var dets []AnalyticsDetection
	var standaloneBox *xmlBBox
	dec := xml.NewDecoder(strings.NewReader("<root>" + rawXML + "</root>"))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "Object":
			var obj xmlObject
			if dec.DecodeElement(&obj, &se) != nil {
				continue
			}
			det := AnalyticsDetection{ObjectID: strings.TrimSpace(obj.ObjectID)}
			if box := normalizeONVIFBBox(obj.Shape.BoundingBox.Left, obj.Shape.BoundingBox.Top, obj.Shape.BoundingBox.Right, obj.Shape.BoundingBox.Bottom); len(box) == 4 {
				det.BBox = box
			}
			if cls := strings.TrimSpace(obj.ClassExt.ObjectClassification.Type); cls != "" {
				det.Class = cls
				if f, err := strconv.ParseFloat(strings.TrimSpace(obj.ClassExt.ObjectClassification.Likelihood), 64); err == nil {
					if f > 1 && f <= 100 {
						f /= 100
					}
					det.Confidence = &f
				}
			} else if len(obj.AppearanceClass.Candidate) > 0 {
				best := obj.AppearanceClass.Candidate[0]
				bestScore := -1.0
				for _, c := range obj.AppearanceClass.Candidate {
					if s, err := strconv.ParseFloat(strings.TrimSpace(c.Likelihood), 64); err == nil && s > bestScore {
						best = c
						bestScore = s
					}
				}
				det.Class = strings.TrimSpace(best.Type)
				if bestScore >= 0 {
					if bestScore > 1 && bestScore <= 100 {
						bestScore /= 100
					}
					det.Confidence = &bestScore
				}
			} else if cls := strings.TrimSpace(obj.AppearanceClass.Type); cls != "" {
				det.Class = cls
			}
			if det.Class != "" || len(det.BBox) == 4 {
				dets = append(dets, det)
			}
		case "BoundingBox":
			var bb xmlBBox
			if dec.DecodeElement(&bb, &se) == nil && standaloneBox == nil {
				standaloneBox = &bb
			}
		case "ObjectClassification":
			var oc struct {
				Type       string `xml:"Type"`
				Likelihood string `xml:"Likelihood"`
			}
			if dec.DecodeElement(&oc, &se) == nil && strings.TrimSpace(oc.Type) != "" {
				if flat["ObjectType"] == "" {
					flat["ObjectType"] = strings.TrimSpace(oc.Type)
				}
				if strings.TrimSpace(oc.Likelihood) != "" && flat["Confidence"] == "" {
					flat["Confidence"] = strings.TrimSpace(oc.Likelihood)
				}
			}
		}
	}

	// If no <tt:Object> was present but an ElementItem/SimpleItem gave ObjectType
	// alongside a standalone <BoundingBox>, combine them into one detection.
	if len(dets) == 0 && standaloneBox != nil {
		if box := normalizeONVIFBBox(standaloneBox.Left, standaloneBox.Top, standaloneBox.Right, standaloneBox.Bottom); len(box) == 4 {
			cls := firstNonEmptySimple(flat, "ObjectType", "Class", "TargetType", "VehicleType", "PlateNumber", "LicensePlate")
			var conf *float64
			if rawConf := firstNonEmptySimple(flat, "Confidence", "Likelihood", "Score"); rawConf != "" {
				if f, err := strconv.ParseFloat(rawConf, 64); err == nil {
					if f > 1 && f <= 100 {
						f /= 100
					}
					conf = &f
				}
			}
			dets = append(dets, AnalyticsDetection{
				ObjectID:   flat["ObjectId"],
				Class:      cls,
				Confidence: conf,
				BBox:       box,
			})
		}
	}
	return dets
}

func firstNonEmptySimple(m map[string]string, keys ...string) string {
	for _, k := range keys {
		for mk, mv := range m {
			if strings.EqualFold(mk, k) && strings.TrimSpace(mv) != "" {
				return strings.TrimSpace(mv)
			}
		}
	}
	return ""
}

// normalizeONVIFBBox converts ONVIF BoundingBox left/top/right/bottom attributes
// from standard ONVIF normalized Cartesian space [-1, 1] (y-up) or [0, 1]
// screen coordinates (y-down) into normalized [x, y, w, h] in [0, 1].
func normalizeONVIFBBox(lRaw, tRaw, rRaw, bRaw string) []float64 {
	l, err1 := strconv.ParseFloat(strings.TrimSpace(lRaw), 64)
	t, err2 := strconv.ParseFloat(strings.TrimSpace(tRaw), 64)
	r, err3 := strconv.ParseFloat(strings.TrimSpace(rRaw), 64)
	b, err4 := strconv.ParseFloat(strings.TrimSpace(bRaw), 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil
	}
	// ONVIF Cartesian [-1..1] has any negative value, values > 1? No: >1 would be pixels,
	// while any negative OR t > b in [-1..1] indicates Cartesian y-up coordinates.
	if l < 0 || r < 0 || t < 0 || b < 0 || (l <= 1 && r <= 1 && t <= 1 && b <= 1 && t > b) {
		x1 := (min(l, r) + 1.0) / 2.0
		x2 := (max(l, r) + 1.0) / 2.0
		y1 := (1.0 - max(t, b)) / 2.0
		y2 := (1.0 - min(t, b)) / 2.0
		return clampNormalizedBox(x1, y1, x2-x1, y2-y1)
	}
	// Screen space [0..1] y-down:
	if l <= 1 && r <= 1 && t <= 1 && b <= 1 {
		x1, x2 := min(l, r), max(l, r)
		y1, y2 := min(t, b), max(t, b)
		return clampNormalizedBox(x1, y1, x2-x1, y2-y1)
	}
	return nil
}

func clampNormalizedBox(x, y, w, h float64) []float64 {
	if w <= 0 || h <= 0 || x >= 1 || y >= 1 || x+w <= 0 || y+h <= 0 {
		return nil
	}
	if x < 0 {
		w += x
		x = 0
	}
	if y < 0 {
		h += y
		y = 0
	}
	if x+w > 1 {
		w = 1 - x
	}
	if y+h > 1 {
		h = 1 - y
	}
	if w <= 0 || h <= 0 {
		return nil
	}
	return []float64{x, y, w, h}
}
