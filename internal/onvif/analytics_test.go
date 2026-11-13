package onvif

import (
	"reflect"
	"testing"
)

func capabilityIDs(caps []AnalyticsCapability) []string {
	ids := make([]string, 0, len(caps))
	for _, c := range caps {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestDetectAnalyticsMapsAdvertisedTopicsToCapabilities(t *testing.T) {
	caps := DetectAnalytics([]string{
		"RuleEngine/LineDetector/Crossed",
		"RuleEngine/FieldDetector/ObjectsInside",
		"RuleEngine/FaceDetector/FaceDetected",
		"RuleEngine/CellMotionDetector/Motion",
		"VideoSource/MotionAlarm",
		"RuleEngine/TamperDetector/Tamper",
		"RuleEngine/ObjectDetector/LicensePlate",
		"RuleEngine/ObjectDetector/Human",
		"RuleEngine/ObjectDetector/Vehicle",
		"Device/HardwareFailure/StorageFailure",
	})
	want := []string{"lpr", "face", "person", "vehicle", "line_crossing", "intrusion", "motion", "tamper"}
	if got := capabilityIDs(caps); !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %v, want %v (stable order, unrelated topics ignored)", got, want)
	}
	for _, c := range caps {
		if c.Source != AnalyticsSourceONVIFEvents || c.Status != AnalyticsAdvertised || len(c.Topics) == 0 {
			t.Fatalf("capability %+v must say it is only advertised over ONVIF events and carry its evidence", c)
		}
	}
	motion := caps[6]
	if !reflect.DeepEqual(motion.Topics, []string{"RuleEngine/CellMotionDetector/Motion", "VideoSource/MotionAlarm"}) {
		t.Fatalf("motion evidence = %v, want both motion topics", motion.Topics)
	}
}

func TestDetectAnalyticsMatchesWholeWordsOnly(t *testing.T) {
	// "Cardinal", "Planet" and "Offline" contain car, plate-like and line text but name nothing.
	caps := DetectAnalytics([]string{"Device/Cardinal/Direction", "Device/Planet/Alignment", "Device/Offline/State", "Media/Interface/Surface"})
	if len(caps) != 0 {
		t.Fatalf("capabilities = %v, want none: substrings inside other words are not analytics", capabilityIDs(caps))
	}
}

func TestDetectAnalyticsSplitsCamelCaseAndIsCaseInsensitive(t *testing.T) {
	caps := DetectAnalytics([]string{"tns1:RuleEngine/licenseplate/Recognized", "RuleEngine/FACE_DETECTOR/found", "RuleEngine/PeopleCounter/Count"})
	if got, want := capabilityIDs(caps), []string{"lpr", "face", "person"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestDetectAnalyticsReturnsNothingForNoTopics(t *testing.T) {
	if caps := DetectAnalytics(nil); caps != nil {
		t.Fatalf("capabilities = %v, want nil", caps)
	}
}
