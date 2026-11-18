package events

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

const (
	defaultTVTPollInterval = 3 * time.Second
	maxTVTResponseBytes    = 1 << 20 // 1 MiB
)

// TVTPerimeterSummary describes parsed Perimeter/Tripwire rule geometry status
// reported by a TVT / Provision-ISR HTTP XML endpoint.
type TVTPerimeterSummary struct {
	Enabled            bool     `json:"enabled"`
	PolygonsConfigured int      `json:"polygons_configured"`
	LineCount          int      `json:"lines_configured,omitempty"`
	Targets            []string `json:"targets,omitempty"`
}

// TVTProbeResult summarizes real TVT HTTP XML capability and DDA rule checks.
type TVTProbeResult struct {
	Supported    bool                 `json:"supported"`
	DeviceName   string               `json:"device_name,omitempty"`
	Firmware     string               `json:"firmware,omitempty"`
	AlarmStatus  bool                 `json:"alarm_status_supported"`
	Disk         *TVTDiskInfo         `json:"disk,omitempty"`
	Perimeter    *TVTPerimeterSummary `json:"perimeter,omitempty"`
	Tripwire     *TVTPerimeterSummary `json:"tripwire,omitempty"`
	Warnings     []string             `json:"warnings,omitempty"`
	Capabilities []onvif.Capability   `json:"capabilities,omitempty"`
}

// TVTDiskInfo describes the microSD / storage card status reported by /GetDiskInfo.
type TVTDiskInfo struct {
	Found      bool   `json:"found"`
	Count      int    `json:"count"`
	Status     string `json:"status,omitempty"`
	TotalSpace uint64 `json:"total_space"`
	FreeSpace  uint64 `json:"free_space"`
}

// ProbeTVTDisk queries read-only /GetDiskInfo on a TVT / Provision-ISR camera.
func ProbeTVTDisk(ctx context.Context, transport http.RoundTripper, host string, port int, username, password string) (TVTDiskInfo, bool) {
	if port <= 0 {
		port = 80
	}
	client := &tvtHTTPClient{
		http:     &http.Client{Transport: transport, Timeout: 4 * time.Second},
		scheme:   "http",
		hostPort: formatHostPort(host, port),
		username: username,
		password: password,
	}
	return probeTVTDiskClient(ctx, client)
}

func probeTVTDiskClient(ctx context.Context, client *tvtHTTPClient) (TVTDiskInfo, bool) {
	body, code, err := client.get(ctx, "/GetDiskInfo")
	if err != nil || code != http.StatusOK || !bytes.Contains(bytes.ToLower(body), []byte("diskinfo")) {
		return TVTDiskInfo{}, false
	}
	var doc struct {
		DiskInfo struct {
			Count int `xml:"count,attr"`
			Items []struct {
				TotalSpace uint64 `xml:"totalSpace"`
				FreeSpace  uint64 `xml:"freeSpace"`
				Status     string `xml:"status"`
			} `xml:"item"`
		} `xml:"diskInfo"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return TVTDiskInfo{}, false
	}
	info := TVTDiskInfo{Found: true, Count: doc.DiskInfo.Count}
	if len(doc.DiskInfo.Items) > 0 {
		it := doc.DiskInfo.Items[0]
		info.Status = strings.TrimSpace(it.Status)
		info.TotalSpace = it.TotalSpace
		info.FreeSpace = it.FreeSpace
		if info.Count == 0 {
			info.Count = len(doc.DiskInfo.Items)
		}
	}
	return info, true
}

// ProbeTVT queries read-only TVT / Provision-ISR HTTP XML endpoints to confirm
// active alarm polling and detect unconfigured DDA Perimeter/Tripwire zones.
func ProbeTVT(ctx context.Context, transport http.RoundTripper, host string, port int, username, password string, now time.Time) (TVTProbeResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if port <= 0 {
		port = 80
	}
	client := &tvtHTTPClient{
		http:     &http.Client{Transport: transport, Timeout: 4 * time.Second},
		scheme:   "http",
		hostPort: formatHostPort(host, port),
		username: username,
		password: password,
	}

	var res TVTProbeResult
	addCap := func(name string, st onvif.CapabilityStatus, ev any) {
		b, _ := json.Marshal(ev)
		res.Capabilities = append(res.Capabilities, onvif.Capability{
			Capability: name,
			Status:     st,
			Evidence:   string(b),
			Firmware:   res.Firmware,
			CheckedAt:  now,
		})
	}

	// 1. /GetDeviceInfo identifies TVT / Provision-ISR HTTP XML API.
	if body, code, err := client.get(ctx, "/GetDeviceInfo"); err == nil && code == http.StatusOK && bytes.Contains(bytes.ToLower(body), []byte("deviceinfo")) {
		res.Supported = true
		res.DeviceName, res.Firmware = parseTVTDeviceInfo(body)
		addCap("tvt.http_api", onvif.CapConfirmed, map[string]string{
			"op":          "GetDeviceInfo",
			"device_name": res.DeviceName,
			"firmware":    res.Firmware,
		})
	} else {
		// Fall back directly to /GetAlarmStatus before concluding unsupported.
		if abody, acode, aerr := client.get(ctx, "/GetAlarmStatus"); aerr == nil && acode == http.StatusOK && bytes.Contains(bytes.ToLower(abody), []byte("alarm")) {
			res.Supported = true
			res.AlarmStatus = true
			addCap("tvt.http_api", onvif.CapConfirmed, map[string]string{"op": "GetAlarmStatus"})
		} else {
			status := onvif.CapUnsupported
			note := "no TVT HTTP XML API detected"
			if aerr != nil {
				status = onvif.CapError
				note = aerr.Error()
			} else if acode == http.StatusUnauthorized || code == http.StatusUnauthorized {
				status = onvif.CapError
				note = "unauthorized on TVT HTTP endpoint"
			}
			addCap("tvt.http_api", status, map[string]any{"op": "GetDeviceInfo", "status": code, "note": note})
			// 5. /GetDiskInfo (read-only microSD status; never formats)
			if disk, ok := probeTVTDiskClient(ctx, client); ok {
				res.Disk = &disk
				st := strings.ToLower(disk.Status)
				switch {
				case (st == "read/write" || st == "read") && disk.TotalSpace > 0:
					addCap("tvt.sd_card", onvif.CapConfirmed, map[string]any{
						"op":          "GetDiskInfo",
						"status":      disk.Status,
						"total_space": disk.TotalSpace,
						"free_space":  disk.FreeSpace,
					})
				case st == "unformat":
					warn := "microSD detectada en cámara en estado sin formatear (unformat); requiere formateo manual en la interfaz web de la cámara"
					res.Warnings = append(res.Warnings, warn)
					addCap("tvt.sd_card", onvif.CapUnsupported, map[string]any{
						"op":          "GetDiskInfo",
						"status":      disk.Status,
						"total_space": disk.TotalSpace,
						"free_space":  disk.FreeSpace,
						"warning":     warn,
					})
				default:
					addCap("tvt.sd_card", onvif.CapUnsupported, map[string]any{
						"op":     "GetDiskInfo",
						"count":  disk.Count,
						"status": disk.Status,
					})
				}
			}

			return res, nil
		}
	}

	// 2. /GetAlarmStatus
	if !res.AlarmStatus {
		if body, code, err := client.get(ctx, "/GetAlarmStatus"); err == nil && code == http.StatusOK && bytes.Contains(bytes.ToLower(body), []byte("<")) {
			res.AlarmStatus = true
			addCap("tvt.alarm_status", onvif.CapConfirmed, map[string]string{"op": "GetAlarmStatus"})
		} else {
			addCap("tvt.alarm_status", onvif.CapUnsupported, map[string]any{"op": "GetAlarmStatus", "status": code})
		}
	} else {
		addCap("tvt.alarm_status", onvif.CapConfirmed, map[string]string{"op": "GetAlarmStatus"})
	}

	// 3. /GetPerimeterConfig (DDA Intrusion/Perimeter zone check)
	if body, code, err := client.get(ctx, "/GetPerimeterConfig"); err == nil && code == http.StatusOK && bytes.Contains(bytes.ToLower(body), []byte("<")) {
		ps := parseTVTRuleConfig(body, true)
		res.Perimeter = &ps
		switch {
		case ps.Enabled && ps.PolygonsConfigured > 0:
			addCap("tvt.analytics.perimeter", onvif.CapConfirmed, map[string]any{
				"op":                  "GetPerimeterConfig",
				"switch":              true,
				"polygons_configured": ps.PolygonsConfigured,
			})
		case ps.Enabled && ps.PolygonsConfigured == 0:
			warn := "Perímetro DDA habilitado en la cámara, pero sin polígonos de zona dibujados (count=0)"
			res.Warnings = append(res.Warnings, warn)
			addCap("tvt.analytics.perimeter", onvif.CapUnsupported, map[string]any{
				"op":                  "GetPerimeterConfig",
				"switch":              true,
				"polygons_configured": 0,
				"warning":             warn,
			})
		default:
			addCap("tvt.analytics.perimeter", onvif.CapUnsupported, map[string]any{
				"op":                  "GetPerimeterConfig",
				"switch":              false,
				"polygons_configured": ps.PolygonsConfigured,
			})
		}
	}

	// 4. /GetTripwireConfig (Line crossing rule check)
	if body, code, err := client.get(ctx, "/GetTripwireConfig"); err == nil && code == http.StatusOK && bytes.Contains(bytes.ToLower(body), []byte("<")) {
		ts := parseTVTRuleConfig(body, false)
		res.Tripwire = &ts
		switch {
		case ts.Enabled && ts.LineCount > 0:
			addCap("tvt.analytics.tripwire", onvif.CapConfirmed, map[string]any{
				"op":               "GetTripwireConfig",
				"switch":           true,
				"lines_configured": ts.LineCount,
			})
		case ts.Enabled && ts.LineCount == 0:
			warn := "Cruce de línea (Tripwire) habilitado en la cámara, pero sin líneas dibujadas"
			res.Warnings = append(res.Warnings, warn)
			addCap("tvt.analytics.tripwire", onvif.CapUnsupported, map[string]any{
				"op":               "GetTripwireConfig",
				"switch":           true,
				"lines_configured": 0,
				"warning":          warn,
			})
		default:
			addCap("tvt.analytics.tripwire", onvif.CapUnsupported, map[string]any{
				"op":               "GetTripwireConfig",
				"switch":           false,
				"lines_configured": ts.LineCount,
			})
		}
	}

	// 5. /GetDiskInfo (read-only microSD status; never formats)
	if disk, ok := probeTVTDiskClient(ctx, client); ok {
		res.Disk = &disk
		st := strings.ToLower(disk.Status)
		switch {
		case (st == "read/write" || st == "read") && disk.TotalSpace > 0:
			addCap("tvt.sd_card", onvif.CapConfirmed, map[string]any{
				"op":          "GetDiskInfo",
				"status":      disk.Status,
				"total_space": disk.TotalSpace,
				"free_space":  disk.FreeSpace,
			})
		case st == "unformat":
			warn := "microSD detectada en cámara en estado sin formatear (unformat); requiere formateo manual en la interfaz web de la cámara"
			res.Warnings = append(res.Warnings, warn)
			addCap("tvt.sd_card", onvif.CapUnsupported, map[string]any{
				"op":          "GetDiskInfo",
				"status":      disk.Status,
				"total_space": disk.TotalSpace,
				"free_space":  disk.FreeSpace,
				"warning":     warn,
			})
		default:
			addCap("tvt.sd_card", onvif.CapUnsupported, map[string]any{
				"op":     "GetDiskInfo",
				"count":  disk.Count,
				"status": disk.Status,
			})
		}
	}

	return res, nil
}

// TVTPoller actively polls /GetAlarmStatus on enabled Provision-ISR / TVT
// cameras so cameras without working HTTP POST push (or NAT/firewall blocks)
// still deliver normalized analytics events.
type TVTPoller struct {
	Store        *Store
	Cameras      *cameras.Registry
	Transport    http.RoundTripper
	PollInterval time.Duration
	Log          *slog.Logger

	mu      sync.Mutex
	runCtx  context.Context
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func NewTVTPoller(store *Store, cams *cameras.Registry, interval time.Duration, log *slog.Logger) *TVTPoller {
	if interval <= 0 {
		interval = defaultTVTPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &TVTPoller{
		Store:        store,
		Cameras:      cams,
		PollInterval: interval,
		Log:          log,
		workers:      make(map[string]context.CancelFunc),
	}
}

func (p *TVTPoller) Name() string { return "tvt_http_poll" }

// Start reconciles active TVT/Provision pollers and blocks until ctx is canceled.
func (p *TVTPoller) Start(ctx context.Context) {
	p.mu.Lock()
	p.runCtx = ctx
	p.mu.Unlock()

	p.Sync()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			for id, cancel := range p.workers {
				cancel()
				delete(p.workers, id)
			}
			p.mu.Unlock()
			p.wg.Wait()
			return
		case <-tick.C:
			p.Sync()
		}
	}
}

// RestartCamera restarts the active TVT HTTP poller for camera id.
func (p *TVTPoller) RestartCamera(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if cancel, ok := p.workers[id]; ok {
		cancel()
		delete(p.workers, id)
	}
	p.mu.Unlock()
	p.Sync()
}

// Sync reconciles active TVT workers against the camera registry.
func (p *TVTPoller) Sync() {
	if p == nil || p.Cameras == nil {
		return
	}
	p.mu.Lock()
	runCtx := p.runCtx
	p.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return
	}

	desired := map[string]cameras.Entry{}
	for _, e := range p.Cameras.List() {
		if !e.IsEnabled() || e.EffectiveHost() == "" {
			continue
		}
		if p.isTVTCamera(runCtx, e) {
			desired[e.ID] = e
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for id, cancel := range p.workers {
		if _, ok := desired[id]; !ok {
			cancel()
			delete(p.workers, id)
		}
	}
	for id, e := range desired {
		if _, ok := p.workers[id]; !ok {
			wctx, cancel := context.WithCancel(runCtx)
			p.workers[id] = cancel
			p.wg.Add(1)
			go func(entry cameras.Entry) {
				defer p.wg.Done()
				p.runCamera(wctx, entry)
			}(e)
		}
	}
}

func (p *TVTPoller) isTVTCamera(ctx context.Context, e cameras.Entry) bool {
	b := strings.ToLower(strings.TrimSpace(e.Brand))
	if b == "provisionisr" || b == "provision" || b == "tvt" || b == "vtv" {
		return true
	}
	if p.Store == nil {
		return false
	}
	caps, err := p.Store.ListCapabilities(ctx, e.ID)
	if err != nil {
		return false
	}
	for _, c := range caps {
		if (c.Capability == "tvt.alarm_status" || c.Capability == "tvt.http_api") && c.Status == onvif.CapConfirmed {
			return true
		}
	}
	return false
}

func (p *TVTPoller) runCamera(ctx context.Context, e cameras.Entry) {
	port := e.EffectiveONVIFPort()
	if port <= 0 {
		port = 80
	}
	var user, pass string
	if creds, ok := p.Cameras.Credentials(e.ID); ok {
		user, pass = creds.Username, creds.Password
	}
	client := &tvtHTTPClient{
		http:     &http.Client{Transport: p.Transport, Timeout: 4 * time.Second},
		scheme:   "http",
		hostPort: formatHostPort(e.EffectiveHost(), port),
		username: user,
		password: pass,
	}

	activeState := map[string]bool{}
	lastFired := map[string]time.Time{}
	backoff := 5 * time.Second

	for ctx.Err() == nil {
		body, code, err := client.get(ctx, "/GetAlarmStatus")
		if ctx.Err() != nil {
			return
		}
		if err != nil || code >= 400 {
			if err != nil {
				p.Log.Debug("tvt alarm poll failed", "camera", e.ID, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 60*time.Second)
			continue
		}
		backoff = 5 * time.Second

		now := time.Now().UTC()
		statuses := parseTVTAlarmStatusXML(body, now)
		for alarmKey, ev := range statuses {
			wasActive := activeState[alarmKey]
			activeState[alarmKey] = ev.Active
			if !ev.Active {
				continue
			}
			// Trigger only on rising edges or after a sustained cooldown (60s for motion, 15s for rule alarms).
			cooldown := 15 * time.Second
			if ev.Event.EventType == "motion" {
				cooldown = 60 * time.Second
			}
			if wasActive && now.Sub(lastFired[alarmKey]) < cooldown {
				continue
			}
			lastFired[alarmKey] = now

			stored := toStoredEvent(e.ID, ev.Event)
			stored.Source = "tvt_http_poll"
			stored.TimestampUTC = now
			// When local NVR ROI motion tracking is enabled on this camera, skip
			// blind boolean motion alerts that have no bounding box.
			if stored.Type == "motion" && len(stored.BBox) == 0 && e.NVRMotion {
				continue
			}
			if (stored.Type == "intrusion" || stored.Type == "line_crossing") && stored.Class == stored.Type && len(stored.BBox) == 0 {
				if handled, _ := p.Store.EnrichRecentPushRule(ctx, e.ID, stored.Type, now, 35*time.Second); handled {
					lastFired[alarmKey] = now
					continue
				}
			}
			dedupeKey := fmt.Sprintf("tvt:%s:%s:%d", e.ID, alarmKey, now.Unix()/5)
			if inserted, insErr := p.Store.Insert(ctx, stored, dedupeKey); insErr == nil && inserted {
				p.Log.Info("tvt event recorded", "camera", e.ID, "type", stored.Type, "class", stored.Class)
				capEv, _ := json.Marshal(map[string]string{"op": "GetAlarmStatus", "tag": alarmKey})
				_ = p.Store.SaveCapabilities(ctx, e.ID, []onvif.Capability{{
					Capability: "analytics." + stored.Type,
					Status:     onvif.CapConfirmed,
					Evidence:   string(capEv),
					CheckedAt:  now,
				}})
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(p.PollInterval):
		}
	}
}

type tvtAlarmState struct {
	Active bool
	Event  ProvisionEvent
}

func parseTVTAlarmStatusXML(body []byte, now time.Time) map[string]tvtAlarmState {
	out := map[string]tvtAlarmState{}
	trimmed := bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
	trimmed = bytes.TrimRight(trimmed, "\x00 \t\r\n")
	if !looksLikeXML(trimmed) {
		return out
	}

	var base ProvisionEvent
	base.EventTime = now
	_, _ = parseProvisionXML(trimmed, &base)

	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		kl := strings.ToLower(se.Name.Local)
		if !isAlarmStateTag(kl) {
			continue
		}
		var val string
		if dec.DecodeElement(&val, &se) != nil {
			continue
		}
		active := isXMLBoolTrue(val)
		evType := alarmTagToEventType(kl)
		if evType == "" {
			continue
		}
		ev := base
		ev.EventType = evType
		if (evType == "intrusion" || evType == "line_crossing") && base.VehicleType != nil {
			if promoted := eventTypeFromTarget(base.VehicleType); promoted != "" {
				if ev.Direction == nil {
					z := evType
					ev.Direction = &z
				}
				ev.EventType = promoted
			}
		}
		out[kl] = tvtAlarmState{Active: active, Event: ev}
	}
	return out
}

func isAlarmStateTag(kl string) bool {
	return isMotionKey(kl) || isPerimeterAlarmTag(kl) || isLineCrossingKey(kl) ||
		isFaceKey(kl) || isHumanKey(kl) || isVehicleAlarmKey(kl) ||
		isTamperKey(kl) || isPeopleCountAlarmKey(kl)
}

func isPerimeterAlarmTag(kl string) bool {
	return isIntrusionKey(kl)
}

func alarmTagToEventType(kl string) string {
	switch {
	case isFaceKey(kl):
		return "face"
	case isHumanKey(kl):
		return "person"
	case isVehicleAlarmKey(kl):
		return "vehicle"
	case isLineCrossingKey(kl):
		return "line_crossing"
	case isPerimeterAlarmTag(kl):
		return "intrusion"
	case isPeopleCountAlarmKey(kl):
		return "people_counting"
	case isTamperKey(kl):
		return "tamper"
	case isMotionKey(kl):
		return "motion"
	default:
		return ""
	}
}

func parseTVTDeviceInfo(body []byte) (name, firmware string) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(se.Name.Local) {
		case "devicename", "model":
			var s string
			if dec.DecodeElement(&s, &se) == nil && name == "" {
				name = strings.TrimSpace(s)
			}
		case "firmwareversion", "softwareversion":
			var s string
			if dec.DecodeElement(&s, &se) == nil && firmware == "" {
				firmware = strings.TrimSpace(s)
			}
		}
	}
	return name, firmware
}

func parseTVTRuleConfig(body []byte, isPerimeter bool) TVTPerimeterSummary {
	var out TVTPerimeterSummary
	dec := xml.NewDecoder(bytes.NewReader(body))
	var stack []string
	hasParent := func(name string) bool {
		for _, s := range stack {
			if s == name {
				return true
			}
		}
		return false
	}
	directParent := func() string {
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1]
	}

	lineHasCoords := false
	inLineItem := false
	polyItemCounted := false

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			kl := strings.ToLower(t.Name.Local)
			switch kl {
			case "switch":
				var s string
				if dec.DecodeElement(&s, &t) == nil {
					p := directParent()
					on := isXMLBoolTrue(s)
					// Top-level rule switch (<perimeter><switch> or <tripwire><switch>), not nested mail/ftp/push switches.
					if (p == "perimeter" || p == "tripwire") && on {
						out.Enabled = true
					} else if hasParent("objectfilter") && on && (p == "person" || p == "car" || p == "motor") {
						out.Targets = append(out.Targets, p)
					}
				}
				continue
			case "point", "pointgroup":
				countAttr := getXMLAttr(t.Attr, "count")
				cnt, _ := strconv.Atoi(countAttr)
				if cnt > 0 && isPerimeter && (hasParent("boundary") || hasParent("regioninfo")) {
					out.PolygonsConfigured++
					polyItemCounted = true
				}
			case "x", "y":
				var s string
				if dec.DecodeElement(&s, &t) == nil {
					if v, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64); perr == nil && v > 0 {
						if inLineItem {
							lineHasCoords = true
						} else if isPerimeter && (hasParent("boundary") || hasParent("regioninfo")) && !polyItemCounted {
							out.PolygonsConfigured++
							polyItemCounted = true
						}
					}
				}
				continue
			case "item":
				p := directParent()
				if !isPerimeter && p == "line" {
					inLineItem = true
					lineHasCoords = false
				} else if isPerimeter && (p == "boundary" || p == "regioninfo") {
					polyItemCounted = false
				}
			}
			stack = append(stack, kl)
		case xml.EndElement:
			kl := strings.ToLower(t.Name.Local)
			if kl == "item" && inLineItem {
				if lineHasCoords {
					out.LineCount++
				}
				inLineItem = false
				lineHasCoords = false
			}
			if len(stack) > 0 && stack[len(stack)-1] == kl {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return out
}

// tvtHTTPClient handles Basic and RFC 2617/7616 HTTP Digest auth without ever
// leaking credentials into errors.
type tvtHTTPClient struct {
	http     *http.Client
	scheme   string
	hostPort string
	username string
	password string
}

func (c *tvtHTTPClient) get(ctx context.Context, path string) ([]byte, int, error) {
	u := fmt.Sprintf("%s://%s%s", c.scheme, c.hostPort, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxTVTResponseBytes))
	resp.Body.Close()
	if rerr != nil {
		return nil, resp.StatusCode, rerr
	}
	if resp.StatusCode != http.StatusUnauthorized || c.username == "" {
		return body, resp.StatusCode, nil
	}

	var authVals []string
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "WWW-Authenticate") {
			authVals = append(authVals, vs...)
		}
	}
	hdr, ok := buildAuthHeader(authVals, http.MethodGet, req.URL.RequestURI(), c.username, c.password)
	if !ok {
		return body, resp.StatusCode, nil
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req2.Header.Set("Authorization", hdr)
	resp2, err := c.http.Do(req2)
	if err != nil {
		return nil, 0, err
	}
	defer resp2.Body.Close()
	body2, err := io.ReadAll(io.LimitReader(resp2.Body, maxTVTResponseBytes))
	return body2, resp2.StatusCode, err
}

func formatHostPort(host string, port int) string {
	host = strings.TrimSpace(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func buildAuthHeader(challenges []string, method, uri, user, pass string) (string, bool) {
	for _, ch := range challenges {
		if len(ch) >= 6 && strings.EqualFold(ch[:6], "Digest") {
			if hdr, ok := buildDigestHeader(ch[6:], method, uri, user, pass); ok {
				return hdr, true
			}
		}
	}
	for _, ch := range challenges {
		if len(ch) >= 5 && strings.EqualFold(ch[:5], "Basic") {
			tok := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			return "Basic " + tok, true
		}
	}
	if user != "" {
		tok := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		return "Basic " + tok, true
	}
	return "", false
}

func buildDigestHeader(paramStr, method, uri, user, pass string) (string, bool) {
	params := parseAuthParams(paramStr)
	realm := params["realm"]
	nonce := params["nonce"]
	if realm == "" || nonce == "" {
		return "", false
	}
	alg := strings.ToUpper(params["algorithm"])
	useSHA256 := strings.HasPrefix(alg, "SHA-256")
	hashFn := func(s string) string {
		if useSHA256 {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		}
		sum := md5.Sum([]byte(s))
		return hex.EncodeToString(sum[:])
	}

	ha1 := hashFn(user + ":" + realm + ":" + pass)
	var nb [8]byte
	_, _ = rand.Read(nb[:])
	cnonce := hex.EncodeToString(nb[:])
	if strings.HasSuffix(alg, "-SESS") {
		ha1 = hashFn(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := hashFn(method + ":" + uri)

	qop := pickAuthQOP(params["qop"])
	nc := "00000001"
	var response string
	if qop != "" {
		response = hashFn(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		response = hashFn(ha1 + ":" + nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		escapeAuthVal(user), escapeAuthVal(realm), escapeAuthVal(nonce), escapeAuthVal(uri), response)
	if alg != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, alg)
	}
	if op := params["opaque"]; op != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, escapeAuthVal(op))
	}
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, qop, nc, cnonce)
	}
	return b.String(), true
}

func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, ", \t\r\n")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		k := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimSpace(s[eq+1:])
		var v string
		if strings.HasPrefix(s, `"`) {
			s = s[1:]
			end := strings.IndexByte(s, '"')
			if end < 0 {
				v = s
				s = ""
			} else {
				v = s[:end]
				s = s[end+1:]
			}
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				v = strings.TrimSpace(s)
				s = ""
			} else {
				v = strings.TrimSpace(s[:comma])
				s = s[comma+1:]
			}
		}
		out[k] = v
	}
	return out
}

func pickAuthQOP(raw string) string {
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(strings.ToLower(part)) == "auth" {
			return "auth"
		}
	}
	return ""
}

func escapeAuthVal(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}
