package onvif

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// CapabilityStatus is one of the 4 allowed hardware capability states in AGENTS.md / PRD M01.
type CapabilityStatus string

const (
	CapConfirmed   CapabilityStatus = "confirmed"
	CapUnsupported CapabilityStatus = "unsupported"
	CapUnknown     CapabilityStatus = "unknown"
	CapError       CapabilityStatus = "error"
)

// Capability records the verified status of one camera feature with evidence.
type Capability struct {
	Capability string           `json:"capability"`
	Status     CapabilityStatus `json:"status"`
	Evidence   string           `json:"evidence,omitempty"`
	Firmware   string           `json:"firmware,omitempty"`
	CheckedAt  time.Time        `json:"checked_at"`
}

// ProbeRequest asks to probe a camera over ONVIF.
type ProbeRequest struct {
	Host      string `json:"host"`
	ONVIFPort int    `json:"onvif_port"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	CameraID  string `json:"camera_id,omitempty"`
}

// ProbeResult is the complete outcome of an ONVIF camera probe.
type ProbeResult struct {
	Manufacturer  string         `json:"manufacturer"`
	Model         string         `json:"model"`
	Firmware      string         `json:"firmware"`
	Serial        string         `json:"serial"`
	HardwareID    string         `json:"hardware_id,omitempty"`
	ClockOffsetMS int64          `json:"clock_offset_ms"`
	MainURL       string         `json:"main_url,omitempty"`
	SubURL        string         `json:"sub_url,omitempty"`
	Profiles      []MediaProfile `json:"profiles,omitempty"`
	EventTopics   []string       `json:"event_topics,omitempty"`
	EventsService string         `json:"-"`
	Capabilities  []Capability   `json:"capabilities"`
}

// AnalyticsCapability maps advertised ONVIF topics to a known analytic kind.
type AnalyticsCapability struct {
	ID     string   `json:"id"`
	Topics []string `json:"topics"`
}

var analyticsKinds = []struct {
	id    string
	match func(words map[string]bool) bool
}{
	{"lpr", anyWord("plate", "licenseplate", "numberplate", "lpr", "anpr")},
	{"face", anyWord("face", "faces", "facematch", "facerecognition")},
	{"person", anyWord("human", "humans", "person", "persons", "people", "pedestrian")},
	{"vehicle", anyWord("vehicle", "vehicles", "car", "truck", "motorcycle", "motorbike", "bicycle", "bus")},
	{"line_crossing", func(w map[string]bool) bool {
		return w["tripwire"] || w["linecross"] || w["linecrossing"] || w["crossline"] ||
			(w["line"] && (w["detector"] || w["crossed"] || w["crossing"] || w["cross"]))
	}},
	{"intrusion", func(w map[string]bool) bool {
		return w["intrusion"] || w["intruder"] || w["perimeter"] || w["areadetector"] || w["objectinside"] || w["objectsinside"] || (w["field"] && w["detector"])
	}},
	{"loitering", anyWord("loitering", "wander")},
	{"people_counting", func(w map[string]bool) bool {
		return w["peoplecount"] || w["peoplecounting"] || w["crowd"] || (w["people"] && w["count"])
	}},
	{"motion", anyWord("motion")},
	{"tamper", func(w map[string]bool) bool {
		return w["tamper"] || (w["scene"] && w["change"]) || w["tooblurry"] || w["toodark"] || w["toobright"]
	}},
}

func anyWord(candidates ...string) func(map[string]bool) bool {
	return func(w map[string]bool) bool {
		for _, c := range candidates {
			if w[c] {
				return true
			}
		}
		return false
	}
}

// DetectAnalytics classifies advertised ONVIF event topics into known analytics.
// Note: an advertised topic is NOT confirmation that the camera fires it;
// Probe reports advertised analytics as "unknown" with the topic as evidence
// until a real event is observed.
func DetectAnalytics(topics []string) []AnalyticsCapability {
	if len(topics) == 0 {
		return nil
	}
	evidence := make(map[string][]string, len(analyticsKinds))
	for _, topic := range topics {
		words := topicWords(topic)
		for _, kind := range analyticsKinds {
			if kind.match(words) {
				evidence[kind.id] = append(evidence[kind.id], topic)
			}
		}
	}
	var out []AnalyticsCapability
	for _, kind := range analyticsKinds {
		if found := evidence[kind.id]; len(found) > 0 {
			sort.Strings(found)
			out = append(out, AnalyticsCapability{ID: kind.id, Topics: found})
		}
	}
	return out
}

func topicWords(topic string) map[string]bool {
	words := map[string]bool{}
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words[strings.ToLower(string(current))] = true
			current = current[:0]
		}
	}
	var prev rune
	for _, r := range topic {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			current = append(current, r)
		default:
			current = append(current, r)
		}
		prev = r
	}
	flush()
	return words
}

// ClassifiedEvent is a normalized detection derived from an ONVIF NotificationMessage.
type ClassifiedEvent struct {
	Type       string               `json:"type"`
	Class      string               `json:"class,omitempty"`
	Zone       string               `json:"zone,omitempty"`
	Confidence *float64             `json:"confidence,omitempty"`
	BBox       []float64            `json:"bbox,omitempty"`
	Detections []AnalyticsDetection `json:"detections,omitempty"`
	Active     bool                 `json:"active"`
}

func targetKindFromClass(raw string) string {
	w := topicWords(raw)
	switch {
	case w["plate"] || w["licenseplate"] || w["lpr"] || w["anpr"]:
		return "lpr"
	case w["face"]:
		return "face"
	case w["human"] || w["humans"] || w["person"] || w["persons"] || w["people"] || w["pedestrian"] || w["man"] || w["woman"]:
		return "person"
	case w["vehicle"] || w["vehicles"] || w["car"] || w["truck"] || w["bus"] || w["suv"] || w["van"] || w["motorcycle"] || w["motorbike"] || w["motor"] || w["bicycle"] || w["bike"]:
		return "vehicle"
	}
	return ""
}

// ClassifyNotification maps an ONVIF PullPoint notification to a detection event.
// Property deletion or boolean false ("false"/"0") notifications are marked Active=false
// so alarm-clear messages do not create false detections.
func ClassifyNotification(n EventNotification) ClassifiedEvent {
	if strings.EqualFold(n.Data["_property_op"], "Deleted") {
		return ClassifiedEvent{}
	}
	words := topicWords(n.Topic)
	evType := ""
	for _, kind := range analyticsKinds {
		if kind.match(words) {
			evType = kind.id
			break
		}
	}
	objType := firstNonEmpty(n.Data, "ObjectType", "TargetType", "Class", "VehicleType")
	if evType == "" && objType != "" {
		evType = targetKindFromClass(objType)
	}
	if evType == "" && len(n.Detections) > 0 {
		for _, d := range n.Detections {
			if k := targetKindFromClass(d.Class); k != "" {
				evType = k
				break
			}
		}
		if evType == "" {
			evType = "intrusion"
		}
	}
	if evType == "" {
		return ClassifiedEvent{}
	}

	// Check boolean state fields in SimpleItem data.
	stateKeys := []string{"IsMotion", "State", "LogicalState", "IsInside", "Active", "IsTamper", "Crossed"}
	hasStateKey := false
	isActive := false
	for _, k := range stateKeys {
		for dk, dv := range n.Data {
			if strings.EqualFold(dk, k) {
				hasStateKey = true
				v := strings.ToLower(strings.TrimSpace(dv))
				if v == "true" || v == "1" || v == "yes" || v == "active" {
					isActive = true
				}
			}
		}
	}
	// Stateless trigger topics (e.g. LineDetector/Crossed, PlateNumber, or VideoAnalytics objects) are active when not Deleted.
	if !hasStateKey {
		if words["crossed"] || len(n.Detections) > 0 || n.Data["PlateNumber"] != "" || n.Data["LicensePlate"] != "" || n.Data["ObjectId"] != "" {
			isActive = true
		}
	}
	if !isActive {
		return ClassifiedEvent{Type: evType, Active: false}
	}

	class := evType
	if p := firstNonEmpty(n.Data, "PlateNumber", "LicensePlate", "Plate"); p != "" {
		class = p
		evType = "lpr"
	} else if objType != "" {
		class = objType
	} else if len(n.Detections) > 0 && n.Detections[0].Class != "" {
		class = n.Detections[0].Class
	}
	// Promote generic line_crossing / intrusion rule alarms to person/vehicle/face
	// when ObjectType or Class identifies the target object.
	if evType == "line_crossing" || evType == "intrusion" || evType == "loitering" {
		if tk := targetKindFromClass(class); tk != "" {
			evType = tk
		}
	}
	zone := firstNonEmpty(n.Data, "RuleName", "Rule", "Region", "VideoSourceConfigurationToken", "Source")

	var conf *float64
	if rawC := firstNonEmpty(n.Data, "Confidence", "Likelihood", "Score"); rawC != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(rawC), 64); err == nil {
			if f > 1 && f <= 100 {
				f /= 100
			}
			conf = &f
		}
	}
	var bbox []float64
	dets := append([]AnalyticsDetection(nil), n.Detections...)
	if len(dets) > 0 {
		for _, d := range dets {
			if len(d.BBox) == 4 && len(bbox) == 0 {
				bbox = d.BBox
			}
			if d.Confidence != nil && conf == nil {
				c := *d.Confidence
				conf = &c
			}
		}
	}

	return ClassifiedEvent{
		Type:       evType,
		Class:      class,
		Zone:       zone,
		Confidence: conf,
		BBox:       bbox,
		Detections: dets,
		Active:     true,
	}
}

func firstNonEmpty(m map[string]string, keys ...string) string {
	for _, k := range keys {
		for mk, mv := range m {
			if strings.EqualFold(mk, k) && strings.TrimSpace(mv) != "" {
				return strings.TrimSpace(mv)
			}
		}
	}
	return ""
}

// Probe executes a real, read-only ONVIF probe against req.Host and builds the
// capability matrix with evidence.
func Probe(ctx context.Context, transport http.RoundTripper, req ProbeRequest, now time.Time) (ProbeResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	port := req.ONVIFPort
	if port <= 0 {
		port = 80
	}
	devEP, err := EndpointForHost(req.Host, port)
	if err != nil {
		return ProbeResult{}, err
	}

	client := NewClient(transport, Config{
		Timeout:     4 * time.Second,
		Credentials: NewCredentials(req.Username, req.Password),
		Now:         func() time.Time { return now },
	})
	dev := NewDeviceClient(client, devEP)

	var res ProbeResult
	// 1. Read system clock first (unauthenticated call works on most cameras) to measure offset.
	unauthClient := NewClient(transport, Config{Timeout: 3 * time.Second, Now: func() time.Time { return now }})
	if clk, clkErr := NewDeviceClient(unauthClient, devEP).GetSystemDateAndTime(ctx); clkErr == nil {
		if camUTC := clk.Time(); !camUTC.IsZero() {
			offset := camUTC.Sub(now)
			res.ClockOffsetMS = offset.Milliseconds()
			client.SetTimeOffset(offset)
		}
	}

	// 2. GetDeviceInformation (with clock-skew retry if needed).
	info, err := dev.GetDeviceInformation(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	res.Manufacturer = strings.TrimSpace(info.Manufacturer)
	res.Model = strings.TrimSpace(info.Model)
	res.Firmware = strings.TrimSpace(info.FirmwareVersion)
	res.Serial = strings.TrimSpace(info.SerialNumber)
	res.HardwareID = strings.TrimSpace(info.HardwareID)

	addCap := func(name string, st CapabilityStatus, ev any) {
		b, _ := json.Marshal(ev)
		res.Capabilities = append(res.Capabilities, Capability{
			Capability: name,
			Status:     st,
			Evidence:   string(b),
			Firmware:   res.Firmware,
			CheckedAt:  now,
		})
	}

	addCap("onvif.device.info", CapConfirmed, map[string]string{
		"op":           "GetDeviceInformation",
		"manufacturer": res.Manufacturer,
		"model":        res.Model,
		"firmware":     res.Firmware,
	})

	// 3. Enumerate services (GetServices, fallback GetCapabilities).
	services, svcErr := dev.GetServices(ctx)
	if svcErr != nil || len(services) == 0 {
		if caps, capErr := dev.GetCapabilities(ctx); capErr == nil && len(caps) > 0 {
			services = caps
		}
	}

	var media1EP, media2EP, eventsEP, searchEP, replayEP *Endpoint
	hasSearch := false
	hasReplay := false
	hasRecording := false
	for _, s := range services {
		ns := strings.ToLower(s.Namespace)
		ep, epErr := RewriteToHost(s.Endpoint, req.Host, port)
		if epErr != nil {
			continue
		}
		switch {
		case strings.Contains(ns, "/ver20/media"):
			e := ep
			media2EP = &e
		case strings.Contains(ns, "/ver10/media"):
			e := ep
			media1EP = &e
		case strings.Contains(ns, "/events"):
			e := ep
			eventsEP = &e
		case strings.Contains(ns, "/search"):
			hasSearch = true
			e := ep
			searchEP = &e
		case strings.Contains(ns, "/replay"):
			hasReplay = true
			e := ep
			replayEP = &e
		case strings.Contains(ns, "/recording"):
			hasRecording = true
		}
	}
	if media1EP == nil && media2EP == nil {
		// Default fallback endpoint on many cameras is /onvif/device_service or /onvif/media_service.
		e := devEP
		media1EP = &e
	}

	// 4. Probe Media2 first, fallback to Media1.
	var profiles []MediaProfile
	if media2EP != nil {
		m2 := NewMediaClient(client, *media2EP, true)
		profs, m2Err := m2.GetProfiles(ctx)
		if m2Err == nil && len(profs) > 0 {
			for i := range profs {
				if uri, uErr := m2.GetStreamURI(ctx, profs[i].Token); uErr == nil {
					profs[i].StreamURI = uri
				}
			}
			profiles = profs
			addCap("onvif.media2.profiles", CapConfirmed, map[string]any{
				"op":       "Media2.GetProfiles",
				"profiles": len(profs),
			})
		} else {
			addCap("onvif.media2.profiles", classifyErrStatus(m2Err), map[string]any{
				"op":    "Media2.GetProfiles",
				"error": errCode(m2Err),
			})
		}
	}
	if len(profiles) == 0 && media1EP != nil {
		m1 := NewMediaClient(client, *media1EP, false)
		profs, m1Err := m1.GetProfiles(ctx)
		if m1Err == nil && len(profs) > 0 {
			for i := range profs {
				if uri, uErr := m1.GetStreamURI(ctx, profs[i].Token); uErr == nil {
					profs[i].StreamURI = uri
				}
			}
			profiles = profs
			addCap("onvif.media.profiles", CapConfirmed, map[string]any{
				"op":       "Media.GetProfiles",
				"profiles": len(profs),
			})
		} else {
			addCap("onvif.media.profiles", classifyErrStatus(m1Err), map[string]any{
				"op":    "Media.GetProfiles",
				"error": errCode(m1Err),
			})
		}
	}

	res.Profiles = profiles
	res.MainURL, res.SubURL = PickMainAndSubStreams(profiles)
	if res.MainURL != "" {
		addCap("rtsp.stream_uri", CapConfirmed, map[string]any{
			"op":       "GetStreamUri",
			"has_main": res.MainURL != "",
			"has_sub":  res.SubURL != "",
		})
	}

	// Profile G (recording/search/replay): verify actual Search/Replay SOAP operations
	// when services are advertised, rather than guessing by manufacturer or GetServices alone.
	if hasSearch || hasReplay || hasRecording {
		gSearchOK := false
		gReplayOK := false
		var firstRecToken string
		recCount := 0
		if searchEP != nil {
			sc := NewSearchClient(client, *searchEP)
			if sum, sErr := sc.GetRecordingSummary(ctx); sErr == nil {
				gSearchOK = true
				recCount = sum.NumberRecordings
				addCap("onvif.profile_g.search", CapConfirmed, map[string]any{
					"op":                "GetRecordingSummary",
					"number_recordings": sum.NumberRecordings,
				})
				if recs, lErr := sc.ListRecordings(ctx, 5); lErr == nil && len(recs) > 0 {
					firstRecToken = recs[0].RecordingToken
					if recCount == 0 {
						recCount = len(recs)
					}
				}
			} else if recs, lErr := sc.ListRecordings(ctx, 5); lErr == nil {
				gSearchOK = true
				recCount = len(recs)
				if len(recs) > 0 {
					firstRecToken = recs[0].RecordingToken
				}
				addCap("onvif.profile_g.search", CapConfirmed, map[string]any{
					"op":                "FindRecordings",
					"number_recordings": recCount,
				})
			} else {
				addCap("onvif.profile_g.search", classifyErrStatus(sErr), map[string]any{
					"op":    "GetRecordingSummary",
					"error": errCode(sErr),
				})
			}
		}
		if replayEP != nil {
			if firstRecToken != "" {
				rc := NewReplayClient(client, *replayEP)
				if uri, rErr := rc.GetReplayURI(ctx, firstRecToken); rErr == nil && uri != "" {
					gReplayOK = true
					addCap("onvif.profile_g.replay", CapConfirmed, map[string]any{
						"op":  "GetReplayUri",
						"uri": uri,
					})
				} else {
					addCap("onvif.profile_g.replay", classifyErrStatus(rErr), map[string]any{
						"op":    "GetReplayUri",
						"error": errCode(rErr),
					})
				}
			} else {
				addCap("onvif.profile_g.replay", CapUnknown, map[string]any{
					"op":   "GetReplayUri",
					"note": "no recordings available on edge storage to probe GetReplayUri",
				})
			}
		}
		overallStatus := CapUnknown
		if gSearchOK && gReplayOK {
			overallStatus = CapConfirmed
		}
		addCap("onvif.profile_g", overallStatus, map[string]any{
			"search":            hasSearch,
			"replay":            hasReplay,
			"recording":         hasRecording,
			"search_ok":         gSearchOK,
			"replay_ok":         gReplayOK,
			"number_recordings": recCount,
		})
	} else if len(services) > 0 {
		addCap("onvif.profile_g", CapUnsupported, map[string]any{
			"op":   "GetServices",
			"note": "no recording/search/replay service advertised",
		})
	}

	// 5. Probe Events service: GetEventProperties + CreatePullPointSubscription check.
	if eventsEP == nil {
		e := devEP
		eventsEP = &e
	}
	res.EventsService = eventsEP.String()
	evClient := NewEventsClient(client, *eventsEP)
	topics, evErr := evClient.GetEventProperties(ctx)
	if evErr == nil {
		res.EventTopics = topics
		addCap("onvif.events.properties", CapConfirmed, map[string]any{
			"op":     "GetEventProperties",
			"topics": len(topics),
		})
		for _, a := range DetectAnalytics(topics) {
			// Rule 4: advertised topics are reported as "unknown" until a real event is observed.
			addCap("analytics."+a.ID, CapUnknown, map[string]any{
				"op":     "GetEventProperties",
				"note":   "topic advertised by camera; confirmed only when a real event arrives",
				"topics": a.Topics,
			})
		}
	} else {
		addCap("onvif.events.properties", classifyErrStatus(evErr), map[string]any{
			"op":    "GetEventProperties",
			"error": errCode(evErr),
		})
	}

	subAddr, subErr := evClient.CreatePullPointSubscription(ctx, "PT1M")
	if subErr == nil && subAddr != "" {
		if subEP, epErr := RewriteToHost(subAddr, req.Host, port); epErr == nil {
			uCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = evClient.Unsubscribe(uCtx, subEP)
			cancel()
		}
		addCap("onvif.events.pullpoint", CapConfirmed, map[string]any{
			"op": "CreatePullPointSubscription",
		})
	} else {
		addCap("onvif.events.pullpoint", classifyErrStatus(subErr), map[string]any{
			"op":    "CreatePullPointSubscription",
			"error": errCode(subErr),
		})
	}

	return res, nil
}

// PickMainAndSubStreams selects the highest-resolution RTSP URI as main and the
// next distinct RTSP URI as sub.
func PickMainAndSubStreams(profiles []MediaProfile) (mainURL, subURL string) {
	bestPixels := -1
	for _, p := range profiles {
		if p.StreamURI == "" {
			continue
		}
		px := p.Width * p.Height
		if mainURL == "" || px > bestPixels {
			mainURL = p.StreamURI
			bestPixels = px
		}
	}
	for _, p := range profiles {
		if p.StreamURI != "" && p.StreamURI != mainURL {
			subURL = p.StreamURI
			break
		}
	}
	return mainURL, subURL
}

func classifyErrStatus(err error) CapabilityStatus {
	var ce *CallError
	if errors.As(err, &ce) {
		if ce.Status == StatusUnsupported {
			return CapUnsupported
		}
	}
	return CapError
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	var ce *CallError
	if errors.As(err, &ce) {
		if ce.Reason != "" {
			return ce.Code + ": " + ce.Reason
		}
		return ce.Code
	}
	return err.Error()
}
