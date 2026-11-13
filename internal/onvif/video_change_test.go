package onvif

import (
	"strings"
	"testing"
)

func testOptions() VideoOptions {
	return VideoOptions{Codecs: []CodecOptions{
		{Codec: CodecH265, Resolutions: []VideoResolution{{2560, 1440}, {1920, 1080}}, FrameRates: []float64{5, 10, 15, 25}, FPS: FloatRange{5, 25},
			Bitrate: IntRange{32, 16384}, GOP: IntRange{1, 100}, Quality: FloatRange{1, 6}, BitrateModes: []string{"constant", "variable"}},
		{Codec: CodecH264, Resolutions: []VideoResolution{{1920, 1080}, {1280, 720}}, FPS: FloatRange{1, 30},
			Bitrate: IntRange{64, 8192}, GOP: IntRange{1, 60}},
	}}
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

func fields(vs []Violation) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Field+":"+v.Code)
	}
	return strings.Join(out, ",")
}

func TestValidateVideoChangeAcceptsSupportedCombination(t *testing.T) {
	d := VideoDesired{Codec: "H265", Resolution: &VideoResolution{1920, 1080}, FPS: fp(15), BitrateKbps: ip(4096), GOP: ip(50), BitrateMode: "constant"}
	if v := ValidateVideoChange(testOptions(), d); len(v) != 0 {
		t.Fatalf("unexpected violations: %+v", v)
	}
}

func TestValidateVideoChangeReturnsEveryViolation(t *testing.T) {
	d := VideoDesired{Codec: "H265", Resolution: &VideoResolution{1280, 720}, FPS: fp(30), BitrateKbps: ip(99999), GOP: ip(500), Quality: fp(9), BitrateMode: "constant"}
	got := fields(ValidateVideoChange(testOptions(), d))
	for _, want := range []string{"resolution:unsupported_resolution", "fps:fps_out_of_range", "bitrate:bitrate_out_of_range", "gop:gop_out_of_range", "quality:quality_out_of_range"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	// 1280x720 exists for H264 but not for H265: the codec/resolution combo is what is rejected.
	if v := ValidateVideoChange(testOptions(), VideoDesired{Codec: "H264", Resolution: &VideoResolution{1280, 720}}); len(v) != 0 {
		t.Fatalf("H264 720p must be valid: %+v", v)
	}
}

func TestValidateVideoChangeUnsupportedCodecAndModeAndInvalid(t *testing.T) {
	got := fields(ValidateVideoChange(testOptions(), VideoDesired{Codec: "MJPEG"}))
	if got != "codec:unsupported_codec" {
		t.Fatalf("got %s", got)
	}
	// H264 does not report modes: asking for one is refused rather than guessed.
	got = fields(ValidateVideoChange(testOptions(), VideoDesired{Codec: "H264", BitrateMode: "constant"}))
	if got != "bitrate_mode:unsupported_bitrate_mode" {
		t.Fatalf("got %s", got)
	}
	got = fields(ValidateVideoChange(testOptions(), VideoDesired{Codec: "H264", FPS: fp(0), BitrateKbps: ip(-1), Resolution: &VideoResolution{}}))
	if strings.Count(got, "invalid_value") != 3 {
		t.Fatalf("got %s", got)
	}
}

func TestValidateVideoChangeUsesCurrentCodecAndSkipsUnreportedLimits(t *testing.T) {
	d := VideoDesired{CurrentCodec: "H265", Resolution: &VideoResolution{1280, 720}}
	if got := fields(ValidateVideoChange(testOptions(), d)); got != "resolution:unsupported_resolution" {
		t.Fatalf("got %s", got)
	}
	opts := VideoOptions{Codecs: []CodecOptions{{Codec: CodecH264}}}
	if v := ValidateVideoChange(opts, VideoDesired{Codec: "H264", Resolution: &VideoResolution{800, 600}, FPS: fp(12), BitrateKbps: ip(10), GOP: ip(7)}); len(v) != 0 {
		t.Fatalf("unreported limits must not be invented: %+v", v)
	}
	if got := fields(ValidateVideoChange(VideoOptions{}, VideoDesired{Codec: "H264"})); got != "codec:options_unavailable" {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyImpact(t *testing.T) {
	cur := ProfileStream{Codec: CodecH265, Resolution: VideoResolution{2560, 1440}, FPS: 20, BitrateKbps: 6144, GOP: 50, Quality: 5}
	for name, d := range map[string]VideoDesired{
		"codec":      {Codec: "H264"},
		"resolution": {Resolution: &VideoResolution{1920, 1080}},
		"fps":        {FPS: fp(15)},
		"bitrate":    {BitrateKbps: ip(4096)},
		"gop":        {GOP: ip(25)},
	} {
		got := ClassifyImpact(cur, d)
		if len(got) != 1 || got[0].Kind != ImpactStreamRestart || got[0].Fields[0] != name {
			t.Fatalf("%s: %+v", name, got)
		}
		if got[0].Message != "Este cambio reiniciará el stream durante aproximadamente algunos segundos." {
			t.Fatalf("message = %q", got[0].Message)
		}
	}
	same := VideoDesired{Codec: "hevc", Resolution: &VideoResolution{2560, 1440}, FPS: fp(20), BitrateKbps: ip(6144), GOP: ip(50)}
	if got := ClassifyImpact(cur, same); got != nil {
		t.Fatalf("unchanged values must not impact: %+v", got)
	}
	multi := ClassifyImpact(cur, VideoDesired{FPS: fp(10), GOP: ip(10)})
	if len(multi) != 1 || strings.Join(multi[0].Fields, ",") != "fps,gop" {
		t.Fatalf("multi = %+v", multi)
	}
}
