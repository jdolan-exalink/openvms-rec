package events

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

const maxProvisionBodyBytes = 10 << 20 // 10 MiB

var (
	ErrEmptyPayload   = errors.New("empty provision payload")
	ErrNoEventFound   = errors.New("no event or plate found in payload")
	labeledPlateRegex = regexp.MustCompile(`(?i)(?:plate|patente|matricula|licence|license)[^\w]*([A-Z0-9]{5,10})`)
)

// ProvisionEvent is the normalized representation of a Provision-ISR / TVT push event.
type ProvisionEvent struct {
	EventType         string
	CameraID          string
	CameraIP          string
	Plate             string
	EventTime         time.Time
	Confidence        *float64
	ListStatus        *string
	VehicleColor      *string
	VehicleBrand      *string
	VehicleModel      *string
	VehicleType       *string
	Speed             *float64
	Direction         *string
	BBox              []float64
	Detections        []Detection
	SnapshotData      []byte
	PersonName        *string
	FaceScore         *float64
	EnterCount        *int
	ExitCount         *int
	PassCount         *int
	ChannelID         *int
	SubscribeRelation string
	HasGenericPEA     bool
}

// NormalizePlate sanitizes license plate strings.
func NormalizePlate(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	var sb strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ParseProvisionRequest parses HTTP POST payloads from Provision-ISR / TVT cameras
// (XML, JSON, Multipart, or URL-encoded forms).
func ParseProvisionRequest(r *http.Request, body []byte, receivedAt time.Time) (ProvisionEvent, error) {
	body = bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
	body = bytes.TrimRight(body, "\x00 \t\r\n")
	if len(bytes.TrimSpace(body)) == 0 && r.URL.RawQuery == "" {
		return ProvisionEvent{}, ErrEmptyPayload
	}
	ev := ProvisionEvent{
		EventTime: receivedAt.UTC(),
		CameraIP:  extractClientIP(r),
	}

	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var parsed bool
	var err error
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		parsed, err = parseProvisionMultipart(params["boundary"], body, &ev)
	case mediaType == "application/json" || looksLikeJSON(body):
		parsed, err = parseProvisionJSON(body, &ev)
	case mediaType == "application/xml" || mediaType == "text/xml" || looksLikeXML(body):
		parsed, err = parseProvisionXML(body, &ev)
	case mediaType == "application/x-www-form-urlencoded" || r.URL.RawQuery != "":
		parsed, err = parseProvisionForm(r, body, &ev)
	default:
		if looksLikeJSON(body) {
			parsed, _ = parseProvisionJSON(body, &ev)
		} else if looksLikeXML(body) {
			parsed, _ = parseProvisionXML(body, &ev)
		}
	}
	if err != nil && !parsed {
		return ProvisionEvent{}, err
	}

	if ev.Plate == "" {
		if m := labeledPlateRegex.FindSubmatch(body); len(m) > 1 {
			ev.Plate = NormalizePlate(string(m[1]))
		}
	}

	if ev.Plate != "" && !isPlaceholderPlate(ev.Plate) {
		ev.Plate = NormalizePlate(ev.Plate)
		if ev.EventType == "" {
			ev.EventType = "lpr"
		}
	} else if strings.EqualFold(strings.TrimSpace(ev.Plate), "COUNT") || ev.EnterCount != nil || ev.ExitCount != nil || ev.PassCount != nil {
		ev.Plate = ""
		if ev.EventType == "" {
			ev.EventType = "people_counting"
		}
	} else if ev.EventType == "" {
		if t := eventTypeFromTarget(ev.VehicleType); t != "" {
			ev.EventType = t
		} else {
			return ProvisionEvent{}, ErrNoEventFound
		}
	}
	return ev, nil
}

func normalizeTVTTargetString(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1":
		return "person"
	case "2":
		return "car"
	case "3":
		return "motorcycle"
	case "4":
		return "bicycle"
	default:
		return strings.TrimSpace(raw)
	}
}

func eventTypeFromTarget(v *string) string {
	if v == nil {
		return ""
	}
	switch strings.ToLower(normalizeTVTTargetString(*v)) {
	case "person", "human", "pedestrian", "people":
		return "person"
	case "car", "vehicle", "vehice", "motor", "motorcycle", "bike", "bicycle", "truck", "bus":
		return "vehicle"
	}
	return ""
}

// ProvisionHandler receives HTTP POST analytics and keepalives from Provision-ISR cameras.
type ProvisionHandler struct {
	Store   *Store
	Cameras *cameras.Registry
	Log     *slog.Logger

	mu            sync.Mutex
	dedup         map[string]time.Time
	photoPushSeen map[string]time.Time
	lastGC        time.Time
}

func NewProvisionHandler(store *Store, cams *cameras.Registry, log *slog.Logger) *ProvisionHandler {
	if log == nil {
		log = slog.Default()
	}
	return &ProvisionHandler{
		Store:         store,
		Cameras:       cams,
		Log:           log,
		dedup:         make(map[string]time.Time),
		photoPushSeen: make(map[string]time.Time),
	}
}

func (h *ProvisionHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		w.Header().Set("Allow", "POST, GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	body, err := io.ReadAll(io.LimitReader(req.Body, maxProvisionBodyBytes+1))
	if err != nil || int64(len(body)) > maxProvisionBodyBytes {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Reply 200 quickly so the camera never enters a retry storm.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))

	h.Log.Info("provision http hit", "method", req.Method, "path", req.URL.Path, "peer", req.RemoteAddr, "bytes", len(body))

	if len(bytes.TrimSpace(body)) == 0 && req.URL.RawQuery == "" {
		return // keepalive
	}

	pev, err := ParseProvisionRequest(req, body, now)
	if err != nil {
		h.Log.Info("provision payload unparsed", "path", req.URL.Path, "content_type", req.Header.Get("Content-Type"), "bytes", len(body), "err", err, "summary", summarizeProvisionTags(body))
		return
	}

	camID := h.resolveCameraID(pev.CameraIP, pev.CameraID)
	if camID == "" {
		h.Log.Info("provision push from unknown camera ignored", "reported_id", pev.CameraID, "type", pev.EventType)
		return
	}

	dedupSub := pev.Plate
	if dedupSub == "" {
		dedupSub = pev.EventType
	}
	metaKey := camID + "|" + dedupSub
	if len(pev.SnapshotData) > 0 {
		h.markPhotoPushSeen(camID, now)
		// Allow one photo-bearing frame even if a metadata-only packet landed
		// earlier, and suppress trailing metadata-only packets afterward.
		if h.isDuplicate(camID+"|photo|"+dedupSub, now) {
			return
		}
		h.markSeen(metaKey, now)
	} else {
		if strings.EqualFold(pev.SubscribeRelation, "FEATURE_RESULT") && h.hasRecentPhotoPush(camID, now, time.Hour) {
			return
		}
		if h.isDuplicate(metaKey, now) {
			return
		}
	}

	ev := toStoredEvent(camID, pev)
	if pev.HasGenericPEA && (ev.Zone == "" || ev.Zone == "intrusion") {
		ev.Zone = h.resolvePEAZone(context.Background(), camID, ev.Zone)
	}
	dedupeKind := ""
	if len(ev.SnapshotData) > 0 {
		dedupeKind = ":photo"
	}
	dedupeKey := fmt.Sprintf("provision:%s%s:%s:%s:%d", camID, dedupeKind, ev.Type, ev.Class, ev.TimestampUTC.Unix()/15)
	inserted, err := h.Store.Insert(context.Background(), ev, dedupeKey)
	if err != nil {
		h.Log.Warn("store provision event", "camera", camID, "err", err)
		return
	}
	if inserted {
		h.Log.Info("provision event recorded", "camera", camID, "type", ev.Type, "class", ev.Class, "boxes", len(ev.Detections), "summary", summarizeProvisionTags(body))
		evJSON, _ := json.Marshal(map[string]string{"op": "http_push", "type": ev.Type})
		_ = h.Store.SaveCapabilities(context.Background(), camID, []onvif.Capability{{
			Capability: "analytics." + ev.Type,
			Status:     onvif.CapConfirmed,
			Evidence:   string(evJSON),
			CheckedAt:  now,
		}})
	}
}

func (h *ProvisionHandler) resolveCameraID(peerIP, reportedID string) string {
	if h.Cameras == nil {
		return ""
	}
	list := h.Cameras.List()
	for _, c := range list {
		if c.EffectiveHost() != "" && c.EffectiveHost() == peerIP {
			return c.ID
		}
	}
	if reportedID != "" {
		for _, c := range list {
			if strings.EqualFold(c.ID, reportedID) || strings.EqualFold(c.Name, reportedID) {
				return c.ID
			}
		}
	}
	return ""
}

func (h *ProvisionHandler) isDuplicate(key string, now time.Time) bool {
	const window = 15 * time.Second
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev, ok := h.dedup[key]; ok && now.Sub(prev) < window {
		return true
	}
	h.dedup[key] = now
	if now.Sub(h.lastGC) > time.Minute {
		for k, t := range h.dedup {
			if now.Sub(t) >= window {
				delete(h.dedup, k)
			}
		}
		h.lastGC = now
	}
	return false
}

func (h *ProvisionHandler) markSeen(key string, now time.Time) {
	h.mu.Lock()
	if h.dedup == nil {
		h.dedup = map[string]time.Time{}
	}
	h.dedup[key] = now
	h.mu.Unlock()
}

func (h *ProvisionHandler) markPhotoPushSeen(camID string, now time.Time) {
	h.mu.Lock()
	if h.photoPushSeen == nil {
		h.photoPushSeen = map[string]time.Time{}
	}
	h.photoPushSeen[camID] = now
	h.mu.Unlock()
}

func (h *ProvisionHandler) hasRecentPhotoPush(camID string, now time.Time, ttl time.Duration) bool {
	h.mu.Lock()
	t, ok := h.photoPushSeen[camID]
	h.mu.Unlock()
	if ok && now.Sub(t) <= ttl {
		return true
	}
	if !ok && h.Store != nil && h.Store.HasProvisionPhotoPush(context.Background(), camID) {
		h.markPhotoPushSeen(camID, now)
		return true
	}
	return false
}

func (h *ProvisionHandler) resolvePEAZone(ctx context.Context, camID, currentZone string) string {
	if h.Store == nil {
		return currentZone
	}
	caps, err := h.Store.ListCapabilities(ctx, camID)
	if err != nil {
		return currentZone
	}
	var tripConfirmed, perimConfirmed bool
	for _, c := range caps {
		if c.Capability == "tvt.analytics.tripwire" && c.Status == onvif.CapConfirmed {
			tripConfirmed = true
		}
		if c.Capability == "tvt.analytics.perimeter" && c.Status == onvif.CapConfirmed {
			perimConfirmed = true
		}
	}
	if tripConfirmed && !perimConfirmed {
		return "line_crossing"
	}
	return currentZone
}

func toStoredEvent(camID string, p ProvisionEvent) Event {
	ev := Event{
		CameraID:     camID,
		TimestampUTC: p.EventTime.UTC(),
		Type:         p.EventType,
		Confidence:   p.Confidence,
		BBox:         p.BBox,
		Source:       "provisionISR",
		SnapshotData: p.SnapshotData,
	}
	switch p.EventType {
	case "lpr":
		parts := []string{p.Plate}
		if p.VehicleBrand != nil && *p.VehicleBrand != "" {
			parts = append(parts, *p.VehicleBrand)
		}
		if p.VehicleColor != nil && *p.VehicleColor != "" {
			parts = append(parts, *p.VehicleColor)
		}
		ev.Class = strings.Join(parts, " · ")
	case "face":
		if p.PersonName != nil && *p.PersonName != "" {
			ev.Class = *p.PersonName
		} else {
			ev.Class = "face"
		}
		if p.FaceScore != nil && ev.Confidence == nil {
			ev.Confidence = p.FaceScore
		}
	case "vehicle", "person":
		if p.VehicleType != nil && *p.VehicleType != "" {
			ev.Class = *p.VehicleType
		} else {
			ev.Class = p.EventType
		}
	case "people_counting":
		var parts []string
		if p.EnterCount != nil {
			parts = append(parts, fmt.Sprintf("in=%d", *p.EnterCount))
		}
		if p.ExitCount != nil {
			parts = append(parts, fmt.Sprintf("out=%d", *p.ExitCount))
		}
		if p.PassCount != nil {
			parts = append(parts, fmt.Sprintf("pass=%d", *p.PassCount))
		}
		ev.Class = strings.Join(parts, " ")
	default:
		if p.VehicleType != nil && *p.VehicleType != "" {
			ev.Class = *p.VehicleType
		} else {
			ev.Class = p.EventType
		}
	}
	if len(p.Detections) > 0 {
		ev.Detections = append([]Detection(nil), p.Detections...)
	}
	if p.Direction != nil && *p.Direction != "" {
		ev.Zone = *p.Direction
	}
	return ev
}

// --- Parsing helpers adapted from openvms/internal/agent/provisionlpr ---

func parseProvisionMultipart(boundary string, body []byte, ev *ProvisionEvent) (bool, error) {
	if boundary == "" {
		return false, errors.New("missing multipart boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	found := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return found, err
		}
		partData, err := io.ReadAll(io.LimitReader(part, 10<<20))
		if err != nil {
			continue
		}
		mt, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(mt, "image/") || bytes.HasPrefix(partData, []byte("\xFF\xD8\xFF")) || bytes.HasPrefix(partData, []byte("\x89PNG")) {
			// Keep the largest image part (scene snapshot rather than small crop).
			if len(partData) > len(ev.SnapshotData) {
				ev.SnapshotData = partData
			}
			continue
		}
		if looksLikeJSON(partData) {
			if ok, _ := parseProvisionJSON(partData, ev); ok {
				found = true
			}
		} else if looksLikeXML(partData) {
			if ok, _ := parseProvisionXML(partData, ev); ok {
				found = true
			}
		}
	}
	return found, nil
}

func parseProvisionJSON(data []byte, ev *ProvisionEvent) (bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, err
	}
	extractProvisionMap(raw, ev)
	return ev.Plate != "" || ev.EventType != "", nil
}

func parseProvisionXML(data []byte, ev *ProvisionEvent) (bool, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	activeAlarms := map[string]bool{}
	var picW, picH float64
	var smartStatus, subRel string

	// Track per-target flat coordinates (<targetId>, <x1>, <y1>, <x2>, <y2>, <targetType>)
	// emitted by TVT PEA FEATURE_RESULT payloads without a wrapping <rect> node.
	type flatBox struct {
		targetID   string
		targetType string
		x1, y1     float64
		x2, y2     float64
		hasX1      bool
		hasY1      bool
		hasX2      bool
		hasY2      bool
	}
	var curBox flatBox
	targetOrder := []string{}
	byTarget := map[string]Detection{}
	anonIdx := 0

	flushFlatBox := func() {
		defer func() {
			curBox = flatBox{targetID: curBox.targetID}
		}()
		var box []float64
		if curBox.hasX1 && curBox.hasY1 && curBox.hasX2 && curBox.hasY2 && curBox.x2 > curBox.x1 && curBox.y2 > curBox.y1 {
			box = normalizeBBox(curBox.x1, curBox.y1, curBox.x2-curBox.x1, curBox.y2-curBox.y1, picW, picH)
		}
		if len(box) != 4 && curBox.targetType == "" {
			return
		}
		normCls := normalizeTVTTargetString(curBox.targetType)
		nt, nc := NormalizeDetectionClass("", normCls)
		key := curBox.targetID
		if key == "" {
			anonIdx++
			key = fmt.Sprintf("_anon_%d", anonIdx)
		}
		if prev, exists := byTarget[key]; exists {
			if len(box) == 4 {
				prev.BBox = box
			}
			if nt != "" {
				prev.Type = nt
				prev.Class = nc
			}
			byTarget[key] = prev
			return
		}
		targetOrder = append(targetOrder, key)
		byTarget[key] = Detection{Type: nt, Class: nc, BBox: box}
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		name := strings.ToLower(se.Name.Local)
		switch {
		case name == "picwidth" || name == "pic_width" || name == "imgwidth" || name == "imagewidth" || name == "width":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v > 0 && picW == 0 {
					picW = v
				}
			}
		case name == "picheight" || name == "pic_height" || name == "imgheight" || name == "imageheight" || name == "height":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v > 0 && picH == 0 {
					picH = v
				}
			}
		case name == "status":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				smartStatus = strings.ToUpper(strings.TrimSpace(s))
			}
		case name == "targetid" || name == "target_id":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				id := strings.TrimSpace(s)
				if id != "" {
					if curBox.hasX1 || curBox.hasX2 || curBox.targetType != "" {
						flushFlatBox()
					}
					curBox.targetID = id
				}
			}
		case name == "x1":
			if curBox.hasX1 && curBox.hasY1 && curBox.hasX2 && curBox.hasY2 {
				flushFlatBox()
			}
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					curBox.x1, curBox.hasX1 = v, true
				}
			}
		case name == "y1":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					curBox.y1, curBox.hasY1 = v, true
				}
			}
		case name == "x2":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					curBox.x2, curBox.hasX2 = v, true
				}
			}
		case name == "y2":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					curBox.y2, curBox.hasY2 = v, true
				}
			}
		case name == "sourcebase64data" || name == "sourceimagebase64" || name == "snapbase64":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if img := decodeBase64Image(s); len(img) > len(ev.SnapshotData) {
					ev.SnapshotData = img
				}
			}
		case isPeopleCountAlarmKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["people_counting"] = true
			}
		case isEnterCountKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					ev.EnterCount = &n
				}
			}
		case isExitCountKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					ev.ExitCount = &n
				}
			}
		case isPassCountKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					ev.PassCount = &n
				}
			}
		case isPersonNameKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && s != "" {
				ev.PersonName = &s
			}
		case isFaceScoreKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					if f > 1.0 {
						f /= 100.0
					}
					ev.FaceScore = &f
				}
			}
		case isRectKey(name):
			if box := parseProvisionXMLRect(dec, se, picW, picH); len(box) == 4 {
				if len(ev.BBox) == 0 {
					ev.BBox = box
				}
				detType := ""
				switch {
				case strings.Contains(name, "human") || strings.Contains(name, "person"):
					detType = "person"
				case strings.Contains(name, "vehicle") || strings.Contains(name, "car"):
					detType = "vehicle"
				case strings.Contains(name, "plate"):
					detType = "lpr"
				}
				if detType != "" {
					activeAlarms[detType] = true
					ev.Detections = append(ev.Detections, Detection{Type: detType, Class: detType, BBox: box})
				}
			}
		case isTargetNodeKey(name):
			if det, ok := parseProvisionXMLTarget(dec, se, picW, picH); ok {
				ev.Detections = append(ev.Detections, det)
				if t, _ := NormalizeDetectionClass(det.Type, det.Class); t != "" {
					activeAlarms[t] = true
				}
				if len(ev.BBox) == 0 && len(det.BBox) == 4 {
					ev.BBox = det.BBox
				}
				if ev.Confidence == nil && det.Confidence != nil {
					c := *det.Confidence
					ev.Confidence = &c
				}
			}
		case name == "subscriberelation" || name == "subscribe_relation":
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				subRel = strings.ToUpper(strings.TrimSpace(s))
				ev.SubscribeRelation = subRel
			}
		case name == "tripwireinfo" || name == "tripwire" || name == "lineinfo" || name == "startpoint" || name == "endpoint":
			activeAlarms["line_crossing"] = true
		case isEventTypeScalarKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if strings.EqualFold(strings.TrimSpace(s), "PEA") {
					ev.HasGenericPEA = true
				}
				if mapped := mapScalarProvisionEventType(s); mapped != "" {
					activeAlarms[mapped] = true
				}
			}
		case name == "rulename" || name == "rule_name" || name == "zonename" || name == "zone_name":
			var s string
			if dec.DecodeElement(&s, &se) == nil && strings.TrimSpace(s) != "" && ev.Direction == nil {
				z := strings.TrimSpace(s)
				ev.Direction = &z
			}
		case isTargetTypeKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && strings.TrimSpace(s) != "" {
				v := normalizeTVTTargetString(s)
				if curBox.targetType != "" && (curBox.hasX1 || curBox.hasY1 || curBox.hasX2 || curBox.hasY2) {
					flushFlatBox()
				}
				curBox.targetType = v
				if ev.VehicleType == nil {
					ev.VehicleType = &v
				}
			}
		case isLineCrossingKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["line_crossing"] = true
			}
		case isIntrusionKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["intrusion"] = true
			}
		case isFaceKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["face"] = true
			}
		case isHumanKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["person"] = true
			}
		case isVehicleAlarmKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["vehicle"] = true
			}
		case isTamperKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["tamper"] = true
			}
		case isMotionKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && isXMLBoolTrue(s) {
				activeAlarms["motion"] = true
			}
		case isPlateKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && s != "" {
				ev.Plate = s
			}
		case isTimeKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				if t, err := parseProvisionTime(s); err == nil {
					ev.EventTime = t.UTC()
				}
			}
		case isCameraKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && s != "" {
				ev.CameraID = s
			}
		case isConfidenceKey(name):
			if cAttr := getXMLAttr(se.Attr, "count"); cAttr != "" {
				if c, err := strconv.ParseFloat(cAttr, 64); err == nil {
					c = max(0.001, min(1.0, c/100.0))
					ev.Confidence = &c
				}
			} else {
				var c float64
				if dec.DecodeElement(&c, &se) == nil {
					if c > 1.0 {
						c /= 100.0
					}
					ev.Confidence = &c
				}
			}
		case isColorKey(name):
			if cAttr := getXMLAttr(se.Attr, "count"); cAttr != "" && !strings.Contains(name, "plate") {
				colors := map[string]string{"1": "Blanco", "2": "Gris", "3": "Amarillo", "4": "Rosa", "5": "Rojo", "6": "Verde", "7": "Azul", "8": "Negro"}
				if c, ok := colors[cAttr]; ok {
					ev.VehicleColor = &c
				}
			} else {
				var s string
				if dec.DecodeElement(&s, &se) == nil && s != "" {
					ev.VehicleColor = &s
				}
			}
		case isBrandKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && s != "" {
				ev.VehicleBrand = &s
			}
		case isDirectionKey(name):
			var s string
			if dec.DecodeElement(&s, &se) == nil && s != "" {
				ev.Direction = &s
			}
		}
	}
	flushFlatBox()
	for _, key := range targetOrder {
		d := byTarget[key]
		if len(d.BBox) == 4 || d.Type != "" {
			ev.Detections = append(ev.Detections, d)
			if d.Type != "" {
				activeAlarms[d.Type] = true
			}
		}
	}
	if (smartStatus == "SMART_STOP" || subRel == "FEATURE_RESULT") && len(ev.Detections) == 0 && len(ev.BBox) == 0 && len(ev.SnapshotData) == 0 && ev.VehicleType == nil {
		return false, nil
	}
	priority := []string{"face", "vehicle", "person", "line_crossing", "intrusion", "people_counting", "tamper", "motion"}
	for _, p := range priority {
		if activeAlarms[p] {
			ev.EventType = p
			break
		}
	}
	if (ev.EventType == "" || ev.EventType == "line_crossing" || ev.EventType == "intrusion" || ev.EventType == "loitering") && ev.VehicleType != nil {
		if t := eventTypeFromTarget(ev.VehicleType); t != "" {
			if ev.EventType != "" && ev.Direction == nil {
				z := ev.EventType
				ev.Direction = &z
			}
			ev.EventType = t
		}
	}
	if ev.Direction == nil && (ev.EventType == "person" || ev.EventType == "vehicle" || ev.EventType == "face") {
		for _, z := range []string{"line_crossing", "intrusion", "loitering"} {
			if activeAlarms[z] {
				zz := z
				ev.Direction = &zz
				break
			}
		}
	}
	if len(ev.Detections) > 0 {
		for i := range ev.Detections {
			if ev.Detections[i].Type == "" && ev.EventType != "" {
				ev.Detections[i].Type = ev.EventType
			}
		}
		if len(ev.BBox) == 0 {
			for _, d := range ev.Detections {
				if len(d.BBox) == 4 {
					ev.BBox = d.BBox
					break
				}
			}
		}
	}
	return ev.Plate != "" || ev.EventType != "" || len(ev.Detections) > 0, nil
}

func parseProvisionForm(r *http.Request, body []byte, ev *ProvisionEvent) (bool, error) {
	vals, err := url.ParseQuery(string(body))
	if err != nil || len(vals) == 0 {
		vals = r.URL.Query()
	}
	for k, v := range vals {
		if len(v) == 0 {
			continue
		}
		kl := strings.ToLower(k)
		switch {
		case isPlateKey(kl):
			ev.Plate = v[0]
		case isTimeKey(kl):
			if t, err := parseProvisionTime(v[0]); err == nil {
				ev.EventTime = t.UTC()
			}
		case isCameraKey(kl):
			ev.CameraID = v[0]
		case isConfidenceKey(kl):
			if c, err := strconv.ParseFloat(v[0], 64); err == nil {
				if c > 1.0 {
					c /= 100.0
				}
				ev.Confidence = &c
			}
		}
	}
	return ev.Plate != "", nil
}

func extractProvisionMap(m map[string]any, ev *ProvisionEvent) {
	for k, v := range m {
		kl := strings.ToLower(k)
		switch val := v.(type) {
		case string:
			switch {
			case kl == "event_type":
				ev.EventType = strings.ToLower(strings.TrimSpace(val))
			case isPlateKey(kl):
				ev.Plate = val
			case isTimeKey(kl):
				if t, err := parseProvisionTime(val); err == nil {
					ev.EventTime = t.UTC()
				}
			case isCameraKey(kl):
				ev.CameraID = val
			case isColorKey(kl):
				ev.VehicleColor = &val
			case isBrandKey(kl):
				ev.VehicleBrand = &val
			case isDirectionKey(kl):
				ev.Direction = &val
			case isPersonNameKey(kl):
				ev.PersonName = &val
			case isTargetTypeKey(kl) || kl == "class":
				s := strings.TrimSpace(val)
				if s != "" {
					ev.VehicleType = &s
				}
			}
		case float64:
			switch {
			case isConfidenceKey(kl):
				c := val
				if c > 1.0 {
					c /= 100.0
				}
				ev.Confidence = &c
			case isFaceScoreKey(kl):
				f := val
				if f > 1.0 {
					f /= 100.0
				}
				ev.FaceScore = &f
			}
		case []any:
			if isRectKey(kl) && len(val) == 4 {
				var box [4]float64
				ok := true
				for i, item := range val {
					n, isNum := item.(float64)
					if !isNum {
						ok = false
						break
					}
					box[i] = n
				}
				if ok && box[2] > 0 && box[3] > 0 {
					ev.BBox = normalizeBBox(box[0], box[1], box[2], box[3], 0, 0)
				}
			} else if kl == "detections" || kl == "targets" || kl == "objects" || kl == "items" {
				for _, item := range val {
					subMap, ok := item.(map[string]any)
					if !ok {
						continue
					}
					sub := ProvisionEvent{EventTime: ev.EventTime}
					extractProvisionMap(subMap, &sub)
					rawT := sub.EventType
					rawC := ""
					if sub.VehicleType != nil {
						rawC = *sub.VehicleType
					}
					nt, nc := NormalizeDetectionClass(rawT, rawC)
					if nt == "" && len(sub.BBox) == 0 {
						continue
					}
					ev.Detections = append(ev.Detections, Detection{
						Type:       nt,
						Class:      nc,
						Confidence: sub.Confidence,
						BBox:       sub.BBox,
					})
					if ev.EventType == "" && nt != "" {
						ev.EventType = nt
					}
					if len(ev.BBox) == 0 && len(sub.BBox) == 4 {
						ev.BBox = sub.BBox
					}
					if ev.Confidence == nil && sub.Confidence != nil {
						c := *sub.Confidence
						ev.Confidence = &c
					}
				}
			}
		case map[string]any:
			if isRectKey(kl) {
				if box := parseProvisionMapRect(val); len(box) == 4 {
					ev.BBox = box
					continue
				}
			}
			extractProvisionMap(val, ev)
		}
	}
}

func parseProvisionMapRect(m map[string]any) []float64 {
	var x, y, w, h, picW, picH float64
	var x1, y1, x2, y2 float64
	var hasX2, hasY2 bool
	for k, v := range m {
		n, ok := v.(float64)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "x":
			x = n
		case "y":
			y = n
		case "w", "width":
			w = n
		case "h", "height":
			h = n
		case "left", "x1":
			x1 = n
		case "top", "y1":
			y1 = n
		case "right", "x2":
			x2 = n
			hasX2 = true
		case "bottom", "y2":
			y2 = n
			hasY2 = true
		case "picwidth", "pic_width", "imgwidth":
			picW = n
		case "picheight", "pic_height", "imgheight":
			picH = n
		}
	}
	if w <= 0 && hasX2 && x2 > x1 {
		x = x1
		w = x2 - x1
	}
	if h <= 0 && hasY2 && y2 > y1 {
		y = y1
		h = y2 - y1
	}
	return normalizeBBox(x, y, w, h, picW, picH)
}

func parseProvisionXMLRect(dec *xml.Decoder, se xml.StartElement, defaultPicW, defaultPicH float64) []float64 {
	var x, y, w, h float64
	var x1, y1, x2, y2 float64
	var hasX2, hasY2 bool
	picW, picH := defaultPicW, defaultPicH

	applyField := func(key string, v float64) {
		switch strings.ToLower(key) {
		case "x":
			x = v
		case "y":
			y = v
		case "w", "width":
			w = v
		case "h", "height":
			h = v
		case "left", "x1":
			x1 = v
		case "top", "y1":
			y1 = v
		case "right", "x2":
			x2 = v
			hasX2 = true
		case "bottom", "y2":
			y2 = v
			hasY2 = true
		case "picwidth", "pic_width", "imgwidth", "imagewidth":
			picW = v
		case "picheight", "pic_height", "imgheight", "imageheight":
			picH = v
		}
	}

	for _, a := range se.Attr {
		if v, err := strconv.ParseFloat(strings.TrimSpace(a.Value), 64); err == nil {
			applyField(a.Name.Local, v)
		}
	}

	depth := 1
	var curTag string
	var curText strings.Builder
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			curTag = t.Name.Local
			curText.Reset()
			for _, a := range t.Attr {
				if v, err := strconv.ParseFloat(strings.TrimSpace(a.Value), 64); err == nil {
					applyField(a.Name.Local, v)
				}
			}
		case xml.CharData:
			if curTag != "" {
				curText.Write(t)
			}
		case xml.EndElement:
			if curTag != "" && strings.EqualFold(t.Name.Local, curTag) {
				if v, err := strconv.ParseFloat(strings.TrimSpace(curText.String()), 64); err == nil {
					applyField(curTag, v)
				}
				curTag = ""
				curText.Reset()
			}
			depth--
		}
	}

	if w <= 0 && hasX2 && x2 > x1 {
		x = x1
		w = x2 - x1
	}
	if h <= 0 && hasY2 && y2 > y1 {
		y = y1
		h = y2 - y1
	}
	return normalizeBBox(x, y, w, h, picW, picH)
}

func normalizeBBox(x, y, w, h, picW, picH float64) []float64 {
	if w <= 0 || h <= 0 {
		return nil
	}
	if x <= 1 && y <= 1 && w <= 1 && h <= 1 {
		return NormalizeBBoxSlice([]float64{x, y, w, h})
	}
	// TVT / Provision-ISR smart event payloads report <width>/<height> for the
	// snapshot JPEG (e.g. 1280x720) while bounding boxes <x1><y1><x2><y2> use
	// a 0..10000 normalized grid. Only use picW/picH when the box fits inside them.
	if picW > 0 && picH > 0 && x+w <= picW && y+h <= picH {
		return NormalizeBBoxSlice([]float64{x / picW, y / picH, w / picW, h / picH})
	}
	if x > 1920 || y > 1080 || x+w > 1920 || y+h > 1080 {
		if x+w <= 10000 && y+h <= 10000 {
			return NormalizeBBoxSlice([]float64{x / 10000.0, y / 10000.0, w / 10000.0, h / 10000.0})
		}
		return NormalizeBBoxSlice([]float64{x / 2560.0, y / 1440.0, w / 2560.0, h / 1440.0})
	}
	if picW > 0 && picH > 0 {
		return NormalizeBBoxSlice([]float64{x / picW, y / picH, w / picW, h / picH})
	}
	return NormalizeBBoxSlice([]float64{x / 1920.0, y / 1080.0, w / 1920.0, h / 1080.0})
}

func extractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().WithZone("").String()
	}
	return host
}

func looksLikeJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 1 && ((t[0] == '{' && t[len(t)-1] == '}') || (t[0] == '[' && t[len(t)-1] == ']'))
}

func looksLikeXML(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 1 && t[0] == '<' && t[len(t)-1] == '>'
}

func getXMLAttr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

func parseProvisionTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, f := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "02/01/2006 15:04:05"} {
		if t, err := time.Parse(f, raw); err == nil {
			return t, nil
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e14 {
			return time.UnixMicro(n), nil
		}
		if n > 1e11 {
			return time.UnixMilli(n), nil
		}
		return time.Unix(n, 0), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", raw)
}

func isPlaceholderPlate(p string) bool {
	p = strings.ToUpper(strings.TrimSpace(p))
	return p == "COUNT" || p == "NOPLATE" || p == "UNKNOWN" || p == "NONE" || p == "NULL"
}
func isXMLBoolTrue(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}
func isPlateKey(k string) bool {
	return k == "plate" || k == "plate_num" || k == "plateno" || k == "license_plate" || k == "licenseplate" || k == "platenumber" || k == "carnum"
}
func isTimeKey(k string) bool {
	return k == "time" || k == "event_time" || k == "eventtime" || k == "timestamp" || k == "snaptime" || k == "datetime" || k == "datatime"
}
func isCameraKey(k string) bool {
	return k == "camera_id" || k == "cameraid" || k == "device_id" || k == "deviceid" || k == "devicename" || k == "device_name" || k == "cameraname"
}
func isConfidenceKey(k string) bool {
	return k == "confidence" || k == "accuracy" || k == "score" || k == "plateconfidence" || k == "plate_confidence"
}
func isColorKey(k string) bool {
	return k == "color" || k == "vehicle_color" || k == "vehiclecolor" || k == "vehicecolor" || k == "carcolor"
}
func isBrandKey(k string) bool {
	return k == "brand" || k == "vehicle_brand" || k == "vehiclebrand" || k == "make"
}
func isDirectionKey(k string) bool { return k == "direction" || k == "heading" }
func isRectKey(k string) bool {
	return k == "rect" || k == "plate_rect" || k == "platerect" || k == "bbox" ||
		k == "targetrect" || k == "target_rect" || k == "objectrect" || k == "object_rect" ||
		k == "humanrect" || k == "human_rect" || k == "vehiclerect" || k == "vehicle_rect" ||
		k == "carrect" || k == "car_rect"
}
func isTargetTypeKey(k string) bool {
	return k == "targettype" || k == "target_type" || k == "objecttype" || k == "object_type" ||
		k == "vehicletype" || k == "vehicle_type"
}
func isPersonNameKey(k string) bool {
	return k == "personname" || k == "person_name" || k == "facename" || k == "face_name"
}
func isFaceScoreKey(k string) bool {
	return k == "facescore" || k == "face_score" || k == "similarity" || k == "matchscore"
}
func isPeopleCountAlarmKey(k string) bool {
	return k == "peoplecountalarm" || k == "peoplecount" || k == "targetcountalarm" || k == "flowcountalarm"
}
func isEnterCountKey(k string) bool { return k == "entercount" || k == "enter_count" || k == "incount" }
func isExitCountKey(k string) bool  { return k == "exitcount" || k == "exit_count" || k == "outcount" }
func isPassCountKey(k string) bool  { return k == "passcount" || k == "pass_count" || k == "totalcount" }
func isLineCrossingKey(k string) bool {
	return k == "linecrossalarm" || k == "linecross" || k == "linecrossing" || k == "tripwirealarm" || k == "tripwire"
}
func isIntrusionKey(k string) bool {
	return k == "intrusionalarm" || k == "intrusion" || k == "fielddetector" || k == "areaintrusion" || k == "areaintrusionalarm" || k == "perimeteralarm" || k == "perimeter"
}
func isFaceKey(k string) bool {
	return k == "facealarm" || k == "facedetectionalarm" || k == "facematchalarm" || k == "face" || k == "facedetection"
}
func isHumanKey(k string) bool {
	return k == "humanalarm" || k == "peoplealarm" || k == "personalarm" || k == "pedestrianalarm" || k == "human" || k == "person"
}
func isVehicleAlarmKey(k string) bool {
	return k == "vehicealarm" || k == "vehiclealarm" || k == "caralarm" || k == "vehice" || k == "vehicle"
}
func isMotionKey(k string) bool { return k == "motionalarm" || k == "motion" }
func isTamperKey(k string) bool {
	return k == "tamperalarm" || k == "scenedetectionalarm" || k == "tamper" || k == "scenechange" || k == "scenechangealarm" || k == "clarityabnormal" || k == "colorabnormal"
}
func isTargetNodeKey(k string) bool {
	return k == "target" || k == "targetinfo" || k == "object" || k == "humaninfo" || k == "vehicleinfo"
}
func isEventTypeScalarKey(k string) bool {
	return k == "eventtype" || k == "event_type" || k == "alarmtype" || k == "alarm_type" || k == "smarttype" || k == "smart_type" || k == "ruletype" || k == "rule_type"
}

func mapScalarProvisionEventType(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == "smartdata" || s == "alarmdata" || s == "keepalive" || s == "none" {
		return ""
	}
	if s == "avd" || strings.HasPrefix(s, "smart_avd_") {
		return "tamper"
	}
	if t, _ := NormalizeDetectionClass(s, ""); t != "" {
		return t
	}
	return ""
}

func parseProvisionXMLTarget(dec *xml.Decoder, se xml.StartElement, defaultPicW, defaultPicH float64) (Detection, bool) {
	var det Detection
	var rawType, rawClass string
	picW, picH := defaultPicW, defaultPicH
	depth := 1
	var curTag string
	var curText strings.Builder

	for _, a := range se.Attr {
		kl := strings.ToLower(a.Name.Local)
		if isTargetTypeKey(kl) || kl == "type" || kl == "class" {
			rawClass = strings.TrimSpace(a.Value)
		} else if isConfidenceKey(kl) {
			if f, err := strconv.ParseFloat(strings.TrimSpace(a.Value), 64); err == nil {
				c := clampConfidence(f)
				det.Confidence = &c
			}
		}
	}

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			kl := strings.ToLower(t.Name.Local)
			if isRectKey(kl) {
				if box := parseProvisionXMLRect(dec, t, picW, picH); len(box) == 4 {
					det.BBox = box
				}
				continue
			}
			depth++
			curTag = kl
			curText.Reset()
		case xml.CharData:
			if curTag != "" {
				curText.Write(t)
			}
		case xml.EndElement:
			if curTag != "" && strings.EqualFold(t.Name.Local, curTag) {
				v := strings.TrimSpace(curText.String())
				switch {
				case curTag == "picwidth" || curTag == "pic_width" || curTag == "imgwidth":
					if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
						picW = n
					}
				case curTag == "picheight" || curTag == "pic_height" || curTag == "imgheight":
					if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
						picH = n
					}
				case isTargetTypeKey(curTag) || curTag == "type" || curTag == "class":
					if v != "" {
						rawClass = v
					}
				case isEventTypeScalarKey(curTag):
					if mapped := mapScalarProvisionEventType(v); mapped != "" {
						rawType = mapped
					}
				case isConfidenceKey(curTag) || isFaceScoreKey(curTag):
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						c := clampConfidence(f)
						det.Confidence = &c
					}
				}
				curTag = ""
				curText.Reset()
			}
			depth--
		}
	}
	nt, nc := NormalizeDetectionClass(rawType, rawClass)
	if nt == "" && len(det.BBox) == 0 {
		return Detection{}, false
	}
	det.Type = nt
	det.Class = nc
	return det, true
}

// summarizeProvisionTags builds a bounded summary of XML element names and short
// scalar values (omitting image blobs and network/credential fields) for diagnostics.
func summarizeProvisionTags(body []byte) string {
	trimmed := bytes.TrimRight(body, "\x00 \t\r\n")
	if !looksLikeXML(trimmed) {
		return ""
	}
	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	var parts []string
	var curTag string
	var text strings.Builder
	for len(parts) < 64 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			curTag = t.Name.Local
			text.Reset()
		case xml.CharData:
			if curTag != "" && text.Len() < 48 {
				text.Write(t)
			}
		case xml.EndElement:
			if curTag != "" && strings.EqualFold(t.Name.Local, curTag) {
				v := strings.TrimSpace(text.String())
				low := strings.ToLower(curTag)
				if v != "" && len(v) <= 24 && !strings.Contains(low, "ip") && !strings.Contains(low, "pass") && !strings.Contains(low, "user") && !strings.Contains(low, "mac") {
					parts = append(parts, curTag+"="+v)
				} else {
					parts = append(parts, curTag)
				}
				curTag = ""
			}
		}
	}
	return strings.Join(parts, ",")
}

func decodeBase64Image(raw string) []byte {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if idx := strings.Index(s, "base64,"); idx >= 0 && idx < 48 {
		s = s[idx+len("base64,"):]
	}
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	if len(s) < 16 || len(s) > 16<<20 {
		return nil
	}
	dec, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		dec, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
	}
	isJPEG := len(dec) >= 3 && dec[0] == 0xFF && dec[1] == 0xD8 && dec[2] == 0xFF
	isPNG := len(dec) >= 8 && string(dec[:8]) == "\x89PNG\r\n\x1a\n"
	if !isJPEG && !isPNG {
		return nil
	}
	return dec
}
