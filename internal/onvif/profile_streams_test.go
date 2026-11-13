package onvif

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProfileStreamsReportsVideoAndAudioPerProfile(t *testing.T) {
	d := newFakeMedia2Device(t)
	streams, err := ProfileStreams(context.Background(), d.conn(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %+v", streams)
	}
	m := streams[0]
	if m.Token != "prof_main" || m.Name != "Main" || m.Codec != CodecH265 || m.Resolution != (VideoResolution{2560, 1440}) ||
		m.FPS != 20 || m.BitrateKbps != 6144 || m.GOP != 50 || m.BitrateMode != BitrateModeVariable || !m.HasAudioEncoder || m.Service != "media2" {
		t.Fatalf("main = %+v", m)
	}
	meta := streams[1]
	if meta.Codec != "" || meta.EncoderToken != "" || meta.HasAudioEncoder || meta.GOP != 0 {
		t.Fatalf("a profile without encoders must stay empty: %+v", meta)
	}
}

func TestProfileStreamsNoAudio(t *testing.T) {
	d := newFakeMedia2Device(t)
	d.audio = false
	streams, err := ProfileStreams(context.Background(), d.conn(t, nil))
	if err != nil || streams[0].HasAudioEncoder {
		t.Fatalf("streams=%+v err=%v", streams, err)
	}
}

func TestProfileStreamsRequestsFullConfigurationsOnMedia2(t *testing.T) {
	d := newFakeMedia2Device(t)
	if _, err := ProfileStreams(context.Background(), d.conn(t, nil)); err != nil {
		t.Fatal(err)
	}
	if got := d.lastBody("GetProfiles"); !strings.Contains(got, "<tr2:Type>All</tr2:Type>") {
		t.Fatalf("Media2 GetProfiles must ask for all configurations: %s", got)
	}
}

// soapFaultEnvelope faults any call, as the Provision-ISR cameras do on Media2 GetProfiles with Type=All.
const soapFaultEnvelope = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><s:Fault><s:Code><s:Value>s:Sender</s:Value><s:Subcode><s:Value>ter:InvalidArgVal</s:Value></s:Subcode></s:Code><s:Reason><s:Text>Argument value invalid</s:Text></s:Reason></s:Fault></s:Body></s:Envelope>`

// media1ProfilesEnvelope is a Media1 GetProfiles answer carrying the full video encoder configuration.
const media1ProfilesEnvelope = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body><trt:GetProfilesResponse><trt:Profiles token="Profile_Main" fixed="true"><tt:Name>MainStream</tt:Name><tt:VideoEncoderConfiguration token="vec_main"><tt:Name>H264_Main</tt:Name><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse></s:Body></s:Envelope>`

// A camera that faults on Media2 GetProfiles (with Type=All) must still be read through Media1,
// which returns the full encoder configuration without a Type parameter.
func TestProfileStreamsFallsBackToMedia1WhenMedia2Faults(t *testing.T) {
	transport := mediaRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		isProfiles := strings.Contains(s, "GetProfiles")
		var payload string
		switch {
		case strings.Contains(r.URL.Path, "media2") && isProfiles:
			payload = soapFaultEnvelope
		case !strings.Contains(r.URL.Path, "media2") && isProfiles:
			payload = media1ProfilesEnvelope
		default:
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(payload)),
			Header:     http.Header{"Content-Type": []string{"application/soap+xml"}},
		}, nil
	})
	conn, err := NewConn(NewClient(transport, Config{}), "http://192.168.1.100/onvif/media_service", "http://192.168.1.100/onvif/media2_service")
	if err != nil {
		t.Fatal(err)
	}

	streams, err := ProfileStreams(context.Background(), conn)
	if err != nil {
		t.Fatalf("expected a Media1 fallback, got error: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("streams = %+v", streams)
	}
	s := streams[0]
	if s.Service != "media1" {
		t.Fatalf("service = %q, want media1", s.Service)
	}
	if s.Token != "Profile_Main" || s.Codec != CodecH264 || s.Resolution != (VideoResolution{1920, 1080}) || s.FPS != 25 || s.BitrateKbps != 4096 {
		t.Fatalf("stream = %+v", s)
	}
}

// A device offering only Media2 keeps its original behaviour: a fault surfaces instead of a silent fallback.
func TestProfileStreamsMedia2OnlyStillReportsTheFault(t *testing.T) {
	transport := mediaRoundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(soapFaultEnvelope)),
			Header:     http.Header{"Content-Type": []string{"application/soap+xml"}},
		}, nil
	})
	conn, err := NewConn(NewClient(transport, Config{}), "", "http://192.168.1.100/onvif/media2_service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProfileStreams(context.Background(), conn); err == nil {
		t.Fatal("a Media2-only fault must surface, not be swallowed")
	}
}
