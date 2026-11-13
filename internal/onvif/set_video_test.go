package onvif

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetVideoEncoderIsReadModifyWriteAndRereads(t *testing.T) {
	d := newFakeMedia2Device(t)
	conn := d.conn(t, nil)
	got, err := SetVideoEncoder(context.Background(), conn, "prof_main", VideoDesired{FPS: fp(15)})
	if err != nil {
		t.Fatal(err)
	}
	if got.FPS != 15 {
		t.Fatalf("re-read FPS = %v", got.FPS)
	}
	// Untouched fields survive the write.
	if got.Codec != CodecH265 || got.Resolution != (VideoResolution{2560, 1440}) || got.BitrateKbps != 6144 || got.GOP != 50 {
		t.Fatalf("untouched fields changed: %+v", got)
	}
	body := d.lastBody("SetVideoEncoderConfiguration")
	for _, want := range []string{`token="venc_1"`, `GovLength="50"`, `Profile="Main"`, "<tt:Name>Main</tt:Name>", "<tt:Encoding>H265</tt:Encoding>", "<tt:BitrateLimit>6144</tt:BitrateLimit>", "<tt:FrameRateLimit>15</tt:FrameRateLimit>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Set body missing %q:\n%s", want, body)
		}
	}
	// read (profiles) -> options -> write -> re-read (profiles)
	if d.opCount("GetProfiles") != 2 || d.opCount("GetVideoEncoderConfigurationOptions") != 1 || d.opCount("SetVideoEncoderConfiguration") != 1 {
		t.Fatalf("ops = %v", d.ops)
	}
	if d.ops[len(d.ops)-1] != "GetProfiles" {
		t.Fatalf("the last call must be the verifying re-read: %v", d.ops)
	}
}

func TestSetVideoEncoderChangesCodecAndPicksSupportedProfile(t *testing.T) {
	d := newFakeMedia2Device(t)
	got, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "prof_main", VideoDesired{Codec: "H264", Resolution: &VideoResolution{1280, 720}, GOP: ip(30), BitrateMode: "constant"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Codec != CodecH264 || got.Resolution != (VideoResolution{1280, 720}) || got.GOP != 30 || got.BitrateMode != BitrateModeConstant {
		t.Fatalf("got %+v", got)
	}
	// H264 options list Baseline Main High: the codec change must not carry the old profile blindly.
	if !strings.Contains(d.lastBody("SetVideoEncoderConfiguration"), `Profile="Main"`) {
		t.Fatalf("profile not chosen from the new codec's list:\n%s", d.lastBody("SetVideoEncoderConfiguration"))
	}
}

func TestSetVideoEncoderRejectsUnsupportedWithoutWriting(t *testing.T) {
	d := newFakeMedia2Device(t)
	_, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "prof_main", VideoDesired{Resolution: &VideoResolution{1280, 720}, FPS: fp(30)})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) != 2 {
		t.Fatalf("err = %v", err)
	}
	if d.opCount("SetVideoEncoderConfiguration") != 0 {
		t.Fatal("nothing may be written when the change is unsupported")
	}
}

func TestSetVideoEncoderDetectsIgnoredWrite(t *testing.T) {
	d := newFakeMedia2Device(t)
	d.ignore = true
	got, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "prof_main", VideoDesired{FPS: fp(10), BitrateKbps: ip(2048)})
	var ve *VerifyError
	if !errors.As(err, &ve) || strings.Join(ve.Fields, ",") != "fps,bitrate" {
		t.Fatalf("err = %v", err)
	}
	if got.FPS != 20 {
		t.Fatalf("the re-read stream must reflect the device: %+v", got)
	}
}

func TestSetVideoEncoderProfileErrors(t *testing.T) {
	d := newFakeMedia2Device(t)
	if _, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "nope", VideoDesired{FPS: fp(10)}); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "prof_meta", VideoDesired{FPS: fp(10)}); !errors.Is(err, ErrNoVideoEncoder) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetVideoEncoderSurfacesSanitizedFaultWithoutCredentials(t *testing.T) {
	d := newFakeMedia2Device(t)
	d.fault = "SetVideoEncoderConfiguration"
	creds := NewCredentials("admin", "hunter2")
	_, err := SetVideoEncoder(context.Background(), d.conn(t, creds), "prof_main", VideoDesired{FPS: fp(10)})
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != "soap_fault" || ce.Status != StatusFailed {
		t.Fatalf("err = %v", err)
	}
	for _, secret := range []string{"hunter2", "admin", "secret-detail", d.srv.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks %q: %v", secret, err)
		}
	}
	if strings.Contains(d.lastBody("SetVideoEncoderConfiguration"), "hunter2") {
		t.Fatal("the password must only travel as a WS-Security digest")
	}
}

func TestSetVideoEncoderFaultOnOptionsStopsBeforeWrite(t *testing.T) {
	d := newFakeMedia2Device(t)
	d.fault = "GetVideoEncoderConfigurationOptions"
	_, err := SetVideoEncoder(context.Background(), d.conn(t, nil), "prof_main", VideoDesired{FPS: fp(10)})
	var ce *CallError
	if !errors.As(err, &ce) || d.opCount("SetVideoEncoderConfiguration") != 0 {
		t.Fatalf("err = %v ops = %v", err, d.ops)
	}
}

func TestSetVideoEncoderMedia1KeepsH264GOPBlock(t *testing.T) {
	const ns = `xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"`
	fps := "25"
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		switch {
		case strings.Contains(body, "SetVideoEncoderConfiguration"):
			sent = body
			fps = between(body, "<tt:FrameRateLimit>", "</tt:FrameRateLimit>")
			fmt.Fprintf(w, `<s:Envelope %s><s:Body><trt:SetVideoEncoderConfigurationResponse/></s:Body></s:Envelope>`, ns)
		case strings.Contains(body, "GetProfiles"):
			fmt.Fprintf(w, `<s:Envelope %s><s:Body><trt:GetProfilesResponse><trt:Profiles token="p1"><tt:Name>P</tt:Name><tt:VideoEncoderConfiguration token="v1"><tt:Name>V</tt:Name><tt:UseCount>1</tt:UseCount><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution><tt:Quality>5</tt:Quality><tt:RateControl><tt:FrameRateLimit>%s</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl><tt:H264><tt:GovLength>40</tt:GovLength><tt:H264Profile>High</tt:H264Profile></tt:H264></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse></s:Body></s:Envelope>`, ns, fps)
		default:
			fmt.Fprintf(w, `<s:Envelope %s><s:Body><trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options><tt:H264><tt:ResolutionsAvailable><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:ResolutionsAvailable><tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>30</tt:Max></tt:FrameRateRange></tt:H264></trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse></s:Body></s:Envelope>`, ns)
		}
	}))
	defer srv.Close()
	conn, _ := NewConn(NewClient(nil, Config{}), srv.URL, "")
	got, err := SetVideoEncoder(context.Background(), conn, "p1", VideoDesired{FPS: fp(12)})
	if err != nil {
		t.Fatal(err)
	}
	if got.FPS != 12 || got.GOP != 40 || got.BitrateKbps != 4096 {
		t.Fatalf("got %+v", got)
	}
	for _, want := range []string{"<tt:GovLength>40</tt:GovLength>", "<tt:H264Profile>High</tt:H264Profile>", "<tt:EncodingInterval>1</tt:EncodingInterval>", "<trt:ForcePersistence>true"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("Media1 write lost %q:\n%s", want, sent)
		}
	}
}
