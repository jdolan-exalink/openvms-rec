package provisionnvr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

// CapState says how sure OpenVMS is that a recorder can do something.
type CapState string

const (
	// CapConfirmed: observed working on this recorder.
	CapConfirmed CapState = "confirmed"
	// CapUnknown: the recorder claims it, or advertises it, but it was not exercised. Never shown as available.
	CapUnknown CapState = "unknown"
	// CapUnsupported: the recorder reports it off, or the probe shows it is absent.
	CapUnsupported CapState = "unsupported"
)

// Capability names. They are the keys the UI matrix reads.
const (
	CapWebAPI        = "web_api"
	CapChannels      = "channels"
	CapChannelStatus = "channel_status"
	CapStorageHealth = "storage_health"
	CapONVIF         = "onvif"
	CapRTSP          = "rtsp"
	CapPTZ           = "ptz"
	CapPlayback      = "playback"
	CapEvents        = "events"
	CapAnalytics     = "analytics"
	CapNativeAPI     = "native_api"
	CapSNMP          = "snmp"
	CapPlateMatch    = "lpr_match"
	CapFaceMatch     = "face_match"
)

// Capability is one row of a recorder's capability matrix.
type Capability struct {
	Name  string
	State CapState
	// Source is the command or protocol that produced the state.
	Source string
	// Detail is a short, human-readable reason. It never carries credentials.
	Detail string
}

// OnvifFacts is what an ONVIF probe of the recorder found.
type OnvifFacts struct {
	// Services are the ONVIF service names the recorder advertises (media, ptz, events...).
	Services []string
	// Profiles is the number of media profiles.
	Profiles int
	// PTZProfiles is how many of them have a PTZ configuration.
	PTZProfiles int
	// StreamURIsOK is true when GetStreamUri answered for the profiles that were tried.
	StreamURIsOK bool
	// SampleStreamPath is the path (and query) of one stream URI, without host or credentials.
	SampleStreamPath string
}

// DetectedChannel is a channel with its live state.
type DetectedChannel struct {
	Channel
	Status *ChannelStatus
}

// Detection is everything a recorder reveals about itself, read-only.
type Detection struct {
	Device     DeviceInfo
	SystemCaps SystemCaps
	Services   Services
	Channels   []DetectedChannel
	Storage    Storage
	ONVIF      *OnvifFacts
	// RTSP is what a DESCRIBE of the first channel's main stream answered; nil when it was not tried or failed.
	RTSP         *RTSPFacts
	Capabilities []Capability
}

// DetectInput is what the caller knows about the recorder. The password is used for this call only.
type DetectInput struct {
	// BaseURL is http(s)://host[:port] of the recorder's web service.
	BaseURL  string
	Username string
	Password string
	// Transport carries every request; nil verifies TLS certificates with the system roots.
	Transport http.RoundTripper
	// ProbeONVIF replaces the ONVIF probe; nil uses the real one. For tests.
	ProbeONVIF func(ctx context.Context, in DetectInput) (*OnvifFacts, error)
	// ProbeRTSP replaces the RTSP check; nil uses ProbeRTSP. For tests.
	ProbeRTSP func(ctx context.Context, host string, port int, user, password string, channel int, stream Stream) (*RTSPFacts, error)
}

// ErrNotActivated reports a recorder that answers but has not been set up, so it has no usable account.
var ErrNotActivated = errors.New("the recorder is not activated")

// Detect recognises the recorder, logs in once, and reads what it reports. A capability that cannot be
// read does not fail the detection (the matrix says why); failing to reach the recorder, to be
// recognised as an NVMS-9000 unit, to log in, or to list channels does.
func Detect(ctx context.Context, in DetectInput) (*Detection, error) {
	c, err := NewClient(in.BaseURL, NewCredentials(in.Username, in.Password), Config{Transport: in.Transport})
	if err != nil {
		return nil, err
	}
	activated, err := c.Activated(ctx)
	if err != nil {
		return nil, err
	}
	if !activated {
		return nil, &Error{Class: ClassUnsupported, Command: "queryActivationStatus", Err: ErrNotActivated}
	}
	defer func() { _ = c.Logout(ctx) }()

	d := &Detection{}
	if d.Device, err = c.DeviceInfo(ctx); err != nil {
		return nil, err
	}
	chans, err := c.Channels(ctx)
	if err != nil {
		return nil, err
	}
	soft := map[string]error{}
	var statuses []ChannelStatus
	if statuses, err = c.ChannelStatuses(ctx); err != nil {
		soft[CapChannelStatus] = err
	}
	byID := map[string]*ChannelStatus{}
	for i := range statuses {
		byID[statuses[i].ID] = &statuses[i]
	}
	for _, ch := range chans {
		d.Channels = append(d.Channels, DetectedChannel{Channel: ch, Status: byID[ch.ID]})
	}
	if d.Storage, err = c.Storage(ctx); err != nil {
		soft[CapStorageHealth] = err
	}
	if d.SystemCaps, err = c.SystemCaps(ctx); err != nil {
		soft["system_caps"] = err
	}
	if d.Services, err = c.Services(ctx); err != nil {
		soft["services"] = err
	}
	if d.Services.ONVIFSwitch {
		probe := in.ProbeONVIF
		if probe == nil {
			probe = probeONVIF
		}
		if facts, err := probe(ctx, in); err != nil {
			soft[CapONVIF] = err
		} else {
			d.ONVIF = facts
		}
	}
	if facts, err := probeFirstStream(ctx, in, d); err != nil {
		soft[CapRTSP] = err
	} else {
		d.RTSP = facts
	}
	d.Capabilities = deriveCapabilities(d, soft)
	return d, nil
}

// probeFirstStream checks the recorder's RTSP server with one DESCRIBE of the main stream of the first
// channel whose number is known. It returns nil, nil when there is nothing to try (server off, no channel).
func probeFirstStream(ctx context.Context, in DetectInput, d *Detection) (*RTSPFacts, error) {
	if !d.Services.RTSPOn || d.Services.RTSPPort <= 0 {
		return nil, nil
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil {
		return nil, err
	}
	probe := in.ProbeRTSP
	if probe == nil {
		probe = ProbeRTSP
	}
	for _, ch := range d.Channels {
		if n, ok := ChannelNumber(ch.ID); ok {
			return probe(ctx, u.Hostname(), d.Services.RTSPPort, in.Username, in.Password, n, StreamMain)
		}
	}
	return nil, nil
}

func deriveCapabilities(d *Detection, soft map[string]error) []Capability {
	var out []Capability
	add := func(name string, state CapState, source, detail string) {
		out = append(out, Capability{Name: name, State: state, Source: source, Detail: detail})
	}
	why := func(key string) string {
		if err := soft[key]; err != nil {
			if class, ok := ClassOf(err); ok {
				return "could not be read: " + string(class)
			}
			return "could not be read"
		}
		return ""
	}

	add(CapWebAPI, CapConfirmed, "doLogin", "login and read commands answered")
	add(CapChannels, CapConfirmed, "queryNodeList", fmt.Sprintf("%d channels", len(d.Channels)))
	if soft[CapChannelStatus] != nil {
		add(CapChannelStatus, CapUnknown, "queryChlStatus", why(CapChannelStatus))
	} else {
		add(CapChannelStatus, CapConfirmed, "queryChlStatus", "online, motion and recording state per channel")
	}
	if soft[CapStorageHealth] != nil {
		add(CapStorageHealth, CapUnknown, "queryStorageDevInfo", why(CapStorageHealth))
	} else {
		add(CapStorageHealth, CapConfirmed, "queryStorageDevInfo", fmt.Sprintf("%d disks", len(d.Storage.Disks)))
	}

	switch {
	case soft["services"] != nil:
		add(CapONVIF, CapUnknown, "queryOnvifCfg", why("services"))
	case !d.Services.ONVIFSwitch:
		add(CapONVIF, CapUnsupported, "queryOnvifCfg", "the ONVIF server is switched off on the recorder")
	case soft[CapONVIF] != nil:
		add(CapONVIF, CapUnknown, "onvif", "the switch is on but the recorder did not answer ONVIF: "+why(CapONVIF))
	default:
		add(CapONVIF, CapConfirmed, "onvif", fmt.Sprintf("%d media profiles", d.ONVIF.Profiles))
	}

	switch {
	case d.RTSP != nil:
		codec := d.RTSP.Codec
		if codec == "" {
			codec = "codec not announced"
		}
		add(CapRTSP, CapConfirmed, "rtsp DESCRIBE", fmt.Sprintf("channel %d main stream answered (%s); playing it inside OpenVMS is not wired yet", d.RTSP.Channel, codec))
	case d.ONVIF != nil && d.ONVIF.StreamURIsOK:
		add(CapRTSP, CapUnknown, "onvif GetStreamUri", "the recorder reports the stream URL path "+d.ONVIF.SampleStreamPath+"; playback not exercised (REQUIRES REAL DEVICE TEST)"+rtspFailure(soft))
	case d.Services.RTSPOn:
		add(CapRTSP, CapUnknown, "queryRTSPServer", fmt.Sprintf("RTSP server on port %d (%s auth); the URL format needs ONVIF or a real device test", d.Services.RTSPPort, d.Services.RTSPAuth)+rtspFailure(soft))
	default:
		add(CapRTSP, CapUnsupported, "queryRTSPServer", "the RTSP server is off")
	}

	switch {
	case d.ONVIF == nil:
		add(CapPTZ, CapUnknown, "onvif", "needs ONVIF to tell")
	case !has(d.ONVIF.Services, "ptz"):
		add(CapPTZ, CapUnsupported, "onvif", "no ONVIF PTZ service")
	case d.ONVIF.PTZProfiles == 0:
		add(CapPTZ, CapUnsupported, "onvif GetProfiles", "no media profile has a PTZ configuration")
	default:
		add(CapPTZ, CapUnknown, "onvif GetProfiles", fmt.Sprintf("%d profiles have a PTZ configuration; movement not exercised", d.ONVIF.PTZProfiles))
	}

	advertised := func(name, service string) {
		switch {
		case d.ONVIF == nil:
			add(name, CapUnknown, "onvif", "needs ONVIF to tell")
		case has(d.ONVIF.Services, service):
			add(name, CapUnknown, "onvif GetServices", "advertised by the recorder, not exercised (REQUIRES REAL DEVICE TEST)")
		default:
			add(name, CapUnsupported, "onvif GetServices", "not advertised by the recorder")
		}
	}
	advertised(CapPlayback, "replay")
	advertised(CapEvents, "events")
	advertised(CapAnalytics, "analytics")

	if soft["services"] != nil {
		add(CapNativeAPI, CapUnknown, "queryApiServer", why("services"))
	} else if !d.Services.APIServerOn {
		add(CapNativeAPI, CapUnsupported, "queryApiServer", "the API server is switched off; its protocol is undocumented (REQUIRES SDK DOCUMENTATION)")
	} else {
		add(CapNativeAPI, CapUnknown, "queryApiServer", "the API server is on; its protocol is undocumented (REQUIRES SDK DOCUMENTATION)")
	}

	if soft["system_caps"] != nil {
		add(CapSNMP, CapUnknown, "querySystemCaps", why("system_caps"))
		add(CapPlateMatch, CapUnknown, "querySystemCaps", why("system_caps"))
		add(CapFaceMatch, CapUnknown, "querySystemCaps", why("system_caps"))
	} else {
		claim := func(name string, on bool) {
			if on {
				add(name, CapUnknown, "querySystemCaps", "declared by the recorder, not exercised")
			} else {
				add(name, CapUnsupported, "querySystemCaps", "the recorder reports it unsupported")
			}
		}
		claim(CapSNMP, d.SystemCaps.SNMP)
		claim(CapPlateMatch, d.SystemCaps.PlateMatch)
		claim(CapFaceMatch, d.SystemCaps.FaceMatch)
	}
	return out
}

// rtspFailure says why the RTSP check did not confirm anything, when it was tried and failed.
func rtspFailure(soft map[string]error) string {
	if err := soft[CapRTSP]; err != nil {
		if class, ok := ClassOf(err); ok {
			return "; the RTSP check failed: " + string(class)
		}
		return "; the RTSP check failed"
	}
	return ""
}

func has(list []string, v string) bool { return slices.Contains(list, v) }

// onvifServiceName turns an ONVIF namespace into the short service name, "" when it is not one.
func onvifServiceName(ns string) string {
	rest, ok := strings.CutPrefix(ns, "http://www.onvif.org/")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "/") // ver10/media/wsdl
	if len(parts) != 3 || parts[2] != "wsdl" {
		return ""
	}
	if parts[1] == "deviceIO" {
		return "deviceio"
	}
	return parts[1]
}

// Media1 and the device service live on the recorder's own web port (observed: /onvif/device_service).
const onvifDevicePath = "/onvif/device_service"

// sameHost keeps ONVIF requests on the recorder: an advertised address is used only for its path, so a
// recorder (or whoever answers for it) cannot send OpenVMS's credentials to another host.
func sameHost(baseURL, advertised string) (string, bool) {
	b, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	a, err := url.Parse(advertised)
	if err != nil || a.Path == "" {
		return "", false
	}
	return b.Scheme + "://" + b.Host + a.Path, true
}

func probeONVIF(ctx context.Context, in DetectInput) (*OnvifFacts, error) {
	c := onvif.NewClient(in.Transport, onvif.Config{Timeout: 8 * time.Second, Credentials: onvif.NewCredentials(in.Username, in.Password)})
	base := strings.TrimRight(in.BaseURL, "/")
	devEP, err := onvif.ParseEndpoint(base + onvifDevicePath)
	if err != nil {
		return nil, err
	}
	services, err := onvif.NewDeviceClient(c, devEP).GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetServices: %w", err)
	}
	facts := &OnvifFacts{}
	var mediaURL string
	for _, s := range services {
		name := onvifServiceName(s.Namespace)
		if name == "" {
			continue
		}
		facts.Services = append(facts.Services, name)
		if s.Namespace == "http://www.onvif.org/ver10/media/wsdl" {
			if u, ok := sameHost(base, s.Endpoint); ok {
				mediaURL = u
			}
		}
	}
	sort.Strings(facts.Services)
	facts.Services = slices.Compact(facts.Services)
	if mediaURL == "" {
		return facts, nil
	}
	mediaEP, err := onvif.ParseEndpoint(mediaURL)
	if err != nil {
		return facts, nil
	}
	media := onvif.NewMediaClient(c, mediaEP)
	profiles, err := media.GetProfiles(ctx)
	if err != nil {
		return facts, nil
	}
	facts.Profiles = len(profiles)
	for _, p := range profiles {
		if p.PTZConfiguration != nil {
			facts.PTZProfiles++
		}
	}
	if len(profiles) > 0 {
		if raw, err := media.GetStreamURI(ctx, profiles[0].Token); err == nil {
			if u, perr := url.Parse(raw); perr == nil && u.Scheme == "rtsp" {
				facts.StreamURIsOK = true
				facts.SampleStreamPath = u.EscapedPath()
				if u.RawQuery != "" {
					facts.SampleStreamPath += "?" + u.RawQuery
				}
			}
		}
	}
	return facts, nil
}
