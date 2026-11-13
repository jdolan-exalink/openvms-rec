package onvif

import (
	"sort"
	"strings"
	"unicode"
)

const (
	// AnalyticsSourceONVIFEvents marks a capability read from the event topics a device advertises.
	AnalyticsSourceONVIFEvents = "onvif_events"
	// AnalyticsAdvertised means the device lists the topic. It does not prove the device fires it:
	// that is only known once a real event arrives.
	AnalyticsAdvertised = "advertised"
)

// AnalyticsCapability is one kind of analytic a device says it can report, with the topics that say so.
type AnalyticsCapability struct {
	ID     string   `json:"id"`
	Source string   `json:"source"`
	Status string   `json:"status"`
	Topics []string `json:"topics"`
}

// analyticsKinds lists the capabilities OpenVMS understands, in the order they are reported. A topic
// belongs to a kind when its words (split on separators and camel case, lower-cased) match.
var analyticsKinds = []struct {
	id    string
	match func(words map[string]bool) bool
}{
	{"lpr", anyWord("plate", "licenseplate", "numberplate", "lpr", "anpr")},
	{"face", anyWord("face", "faces")},
	{"person", anyWord("human", "humans", "person", "persons", "people", "pedestrian")},
	{"vehicle", anyWord("vehicle", "vehicles", "car", "truck")},
	{"line_crossing", func(w map[string]bool) bool {
		return w["tripwire"] || w["linecross"] || w["linecrossing"] ||
			(w["line"] && (w["detector"] || w["crossed"] || w["crossing"] || w["cross"]))
	}},
	{"intrusion", func(w map[string]bool) bool { return w["intrusion"] || w["intruder"] || (w["field"] && w["detector"]) }},
	{"motion", anyWord("motion")},
	{"tamper", func(w map[string]bool) bool { return w["tamper"] || (w["scene"] && w["change"]) }},
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

// DetectAnalytics maps the event topics a device advertises to the analytics OpenVMS knows. Words
// are matched whole, so "Cardinal" is not a car and "Offline" is not a line. A device that lists no
// topics yields nil: absence of topics is not a statement that it has no analytics.
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
			out = append(out, AnalyticsCapability{ID: kind.id, Source: AnalyticsSourceONVIFEvents, Status: AnalyticsAdvertised, Topics: found})
		}
	}
	return out
}

// topicWords splits a topic into lower-case words at separators and at lower-to-upper camel case
// boundaries (LicensePlate -> license, plate; FACE_DETECTOR -> face, detector).
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
