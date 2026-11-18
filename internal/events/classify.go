package events

import (
	"math"
	"strings"
)

// Detection is a normalized analytics object detection inside an Event.
type Detection struct {
	Type       string    `json:"type"`
	Class      string    `json:"class,omitempty"`
	Confidence *float64  `json:"confidence,omitempty"`
	BBox       []float64 `json:"bbox,omitempty"`
}

var typePriority = map[string]int{
	"lpr":             1,
	"face":            2,
	"person":          3,
	"vehicle":         4,
	"line_crossing":   5,
	"intrusion":       6,
	"loitering":       7,
	"people_counting": 8,
	"tamper":          9,
	"motion":          10,
}

var classAliases = map[string]struct {
	typ string
	cls string
}{
	// Person classes
	"person":     {"person", "person"},
	"persona":    {"person", "person"},
	"human":      {"person", "person"},
	"humans":     {"person", "person"},
	"people":     {"person", "person"},
	"pedestrian": {"person", "pedestrian"},
	"man":        {"person", "person"},
	"woman":      {"person", "person"},
	"child":      {"person", "person"},
	// Vehicle classes (preserving granular subtype in Class)
	"vehicle":    {"vehicle", "vehicle"},
	"vehiculo":   {"vehicle", "vehicle"},
	"vehice":     {"vehicle", "vehicle"},
	"car":        {"vehicle", "car"},
	"auto":       {"vehicle", "car"},
	"sedan":      {"vehicle", "car"},
	"suv":        {"vehicle", "suv"},
	"van":        {"vehicle", "van"},
	"truck":      {"vehicle", "truck"},
	"camion":     {"vehicle", "truck"},
	"bus":        {"vehicle", "bus"},
	"colectivo":  {"vehicle", "bus"},
	"motor":      {"vehicle", "motorcycle"},
	"motorcycle": {"vehicle", "motorcycle"},
	"motorbike":  {"vehicle", "motorcycle"},
	"moto":       {"vehicle", "motorcycle"},
	"bicycle":    {"vehicle", "bicycle"},
	"bike":       {"vehicle", "bicycle"},
	"bicicleta":  {"vehicle", "bicycle"},
	"tricycle":   {"vehicle", "bicycle"},
	"nonmotor":   {"vehicle", "bicycle"},
	// Face & LPR
	"face":          {"face", "face"},
	"facealarm":     {"face", "face"},
	"facedetection": {"face", "face"},
	"facematch":     {"face", "face"},
	"lpr":           {"lpr", "lpr"},
	"anpr":          {"lpr", "lpr"},
	"plate":         {"lpr", "lpr"},
	"licenseplate":  {"lpr", "lpr"},
	// Rule / analytic triggers
	"line_crossing":   {"line_crossing", "line_crossing"},
	"linecrossing":    {"line_crossing", "line_crossing"},
	"linecross":       {"line_crossing", "line_crossing"},
	"crossline":       {"line_crossing", "line_crossing"},
	"tripwire":        {"line_crossing", "line_crossing"},
	"pea":             {"intrusion", "perimeter"},
	"intrusion":       {"intrusion", "intrusion"},
	"intruder":        {"intrusion", "intrusion"},
	"perimeter":       {"intrusion", "perimeter"},
	"perimeteralarm":  {"intrusion", "perimeter"},
	"areaintrusion":   {"intrusion", "intrusion"},
	"fielddetector":   {"intrusion", "intrusion"},
	"objectinside":    {"intrusion", "intrusion"},
	"objectsinside":   {"intrusion", "intrusion"},
	"loitering":       {"loitering", "loitering"},
	"people_counting": {"people_counting", "people_counting"},
	"peoplecounting":  {"people_counting", "people_counting"},
	"peoplecount":     {"people_counting", "people_counting"},
	"crowd":           {"people_counting", "people_counting"},
	"tamper":          {"tamper", "tamper"},
	"tamperalarm":     {"tamper", "tamper"},
	"scenechange":     {"tamper", "scene_change"},
	"scenedetection":  {"tamper", "scene_change"},
	"clarityabnormal": {"tamper", "clarity_abnormal"},
	"colorabnormal":   {"tamper", "color_abnormal"},
	"motion":          {"motion", "motion"},
	"motionalarm":     {"motion", "motion"},
	"vmd":             {"motion", "motion"},
	"cellmotion":      {"motion", "motion"},
}

func cleanTokenKey(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == '_' {
			// preserve lookup keys that already include underscores if checked directly
		}
	}
	return b.String()
}

// NormalizeDetectionClass maps rawType and rawClass synonyms onto canonical
// analytics types and granular classes. Generic zone triggers (intrusion,
// line_crossing, loitering) promote to person/vehicle/face/lpr when a specific
// target class is known.
func NormalizeDetectionClass(rawType, rawClass string) (typ, cls string) {
	tTrim := strings.ToLower(strings.TrimSpace(rawType))
	cTrim := strings.TrimSpace(rawClass)
	tComp := cleanTokenKey(rawType)
	cComp := cleanTokenKey(rawClass)

	if entry, ok := classAliases[tTrim]; ok {
		typ = entry.typ
		cls = entry.cls
	} else if entry, ok := classAliases[tComp]; ok {
		typ = entry.typ
		cls = entry.cls
	} else if tTrim != "" {
		typ = tTrim
		cls = tTrim
	}

	if cTrim != "" {
		if entry, ok := classAliases[strings.ToLower(cTrim)]; ok {
			if typ == "" || typ == "intrusion" || typ == "line_crossing" || typ == "loitering" || typ == entry.typ {
				typ = entry.typ
			}
			cls = entry.cls
		} else if entry, ok := classAliases[cComp]; ok {
			if typ == "" || typ == "intrusion" || typ == "line_crossing" || typ == "loitering" || typ == entry.typ {
				typ = entry.typ
			}
			cls = entry.cls
		} else {
			// Preserve specific metadata values such as license plates, person names,
			// or multi-word vehicle descriptions ("Toyota · White").
			cls = cTrim
		}
	}
	if cls == "" {
		cls = typ
	}
	return typ, cls
}

// NormalizeBBoxSlice validates and clamps a normalized [x, y, w, h] box to [0, 1].
func NormalizeBBoxSlice(box []float64) []float64 {
	if len(box) != 4 {
		return nil
	}
	for _, v := range box {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	}
	x, y, w, h := box[0], box[1], box[2], box[3]
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

// NormalizeEvent normalizes an Event and its multi-object Detections, applies
// minConf filtering (when > 0 and detections carry confidence scores), and elects
// the highest-priority primary detection onto ev.Type / ev.Class / ev.Confidence / ev.BBox.
// Returns false if minConf filters out every detection.
func NormalizeEvent(ev *Event, minConf float64) bool {
	if ev == nil {
		return false
	}
	var out []Detection
	if len(ev.Detections) > 0 {
		for _, d := range ev.Detections {
			nt, nc := NormalizeDetectionClass(d.Type, d.Class)
			if nt == "" {
				continue
			}
			var conf *float64
			if d.Confidence != nil {
				c := clampConfidence(*d.Confidence)
				conf = &c
				if minConf > 0 && c < minConf {
					continue
				}
			}
			out = append(out, Detection{
				Type:       nt,
				Class:      nc,
				Confidence: conf,
				BBox:       NormalizeBBoxSlice(d.BBox),
			})
		}
		if len(out) == 0 && minConf > 0 {
			return false
		}
	}

	nt, nc := NormalizeDetectionClass(ev.Type, ev.Class)
	if nt != "" {
		ev.Type = nt
	}
	if nc != "" {
		ev.Class = nc
	}
	ev.BBox = NormalizeBBoxSlice(ev.BBox)
	if ev.Confidence != nil {
		c := clampConfidence(*ev.Confidence)
		ev.Confidence = &c
		if minConf > 0 && c < minConf && len(out) == 0 {
			return false
		}
	}

	if len(out) > 0 {
		best := out[0]
		for _, d := range out[1:] {
			if detectionBetter(d, best) {
				best = d
			}
		}
		ev.Detections = out
		ev.Type = best.Type
		if best.Class != "" {
			ev.Class = best.Class
		}
		if best.Confidence != nil {
			c := *best.Confidence
			ev.Confidence = &c
		}
		if len(best.BBox) == 4 {
			ev.BBox = best.BBox
		}
	} else if ev.Type != "" && len(ev.BBox) == 4 {
		var c *float64
		if ev.Confidence != nil {
			v := *ev.Confidence
			c = &v
		}
		ev.Detections = []Detection{{
			Type:       ev.Type,
			Class:      ev.Class,
			Confidence: c,
			BBox:       ev.BBox,
		}}
	}
	return ev.Type != ""
}

func clampConfidence(c float64) float64 {
	if c > 1.0 && c <= 100.0 {
		c /= 100.0
	}
	return max(0, min(1.0, c))
}

func detectionBetter(a, b Detection) bool {
	pa, pb := priorityRank(a.Type), priorityRank(b.Type)
	if pa != pb {
		return pa < pb
	}
	ca, cb := 0.0, 0.0
	if a.Confidence != nil {
		ca = *a.Confidence
	}
	if b.Confidence != nil {
		cb = *b.Confidence
	}
	if ca != cb {
		return ca > cb
	}
	return len(a.BBox) > len(b.BBox)
}

func priorityRank(t string) int {
	if r, ok := typePriority[t]; ok {
		return r
	}
	return 99
}
