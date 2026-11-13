package onvif

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeMedia2Device is a minimal ONVIF Media2 device. It keeps one H265 profile whose video
// encoder can be rewritten by SetVideoEncoderConfiguration, records every operation, and can
// answer any operation with a SOAP fault.
type fakeMedia2Device struct {
	mu      sync.Mutex
	srv     *httptest.Server
	ops     []string
	bodies  []string
	fault   string // operation name that answers with a fault
	ignore  bool   // accept Set but keep the old values
	encoder string // Encoding
	width   int
	height  int
	fps     string
	bitrate string
	gov     string
	cbr     string
	audio   bool
}

func newFakeMedia2Device(t *testing.T) *fakeMedia2Device {
	t.Helper()
	d := &fakeMedia2Device{encoder: "H265", width: 2560, height: 1440, fps: "20", bitrate: "6144", gov: "50", cbr: "false", audio: true}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeMedia2Device) conn(t *testing.T, creds *Credentials) Conn {
	t.Helper()
	c, err := NewConn(NewClient(nil, Config{Credentials: creds}), "", d.srv.URL+"/media2")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (d *fakeMedia2Device) opCount(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, o := range d.ops {
		if o == name {
			n++
		}
	}
	return n
}

func (d *fakeMedia2Device) lastBody(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.ops) - 1; i >= 0; i-- {
		if d.ops[i] == name {
			return d.bodies[i]
		}
	}
	return ""
}

func (d *fakeMedia2Device) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	d.mu.Lock()
	defer d.mu.Unlock()
	op := ""
	for _, name := range []string{"GetProfiles", "GetVideoEncoderConfigurationOptions", "SetVideoEncoderConfiguration"} {
		if strings.Contains(body, ":"+name+" ") || strings.Contains(body, ":"+name+">") {
			op = name
		}
	}
	d.ops = append(d.ops, op)
	d.bodies = append(d.bodies, body)
	w.Header().Set("Content-Type", "application/soap+xml")
	if op == d.fault {
		fmt.Fprint(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><s:Fault><s:Code><s:Value>s:Sender</s:Value><s:Subcode><s:Value>ter:InvalidArgVal</s:Value></s:Subcode></s:Code><s:Reason><s:Text>secret-detail admin:hunter2</s:Text></s:Reason></s:Fault></s:Body></s:Envelope>`)
		return
	}
	switch op {
	case "GetProfiles":
		audio := ""
		if d.audio {
			audio = `<tr2:AudioEncoder token="aenc_1"><tt:Name>A</tt:Name></tr2:AudioEncoder>`
		}
		fmt.Fprintf(w, media2Envelope, fmt.Sprintf(`<tr2:GetProfilesResponse>
<tr2:Profiles token="prof_main" fixed="true"><tr2:Name>Main</tr2:Name><tr2:Configurations>
<tr2:VideoEncoder token="venc_1" GovLength="%s" Profile="Main"><tt:Name>Main</tt:Name><tt:UseCount>1</tt:UseCount><tt:Encoding>%s</tt:Encoding>
<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>
<tt:RateControl ConstantBitRate="%s"><tt:FrameRateLimit>%s</tt:FrameRateLimit><tt:BitrateLimit>%s</tt:BitrateLimit></tt:RateControl><tt:Quality>5</tt:Quality></tr2:VideoEncoder>%s
</tr2:Configurations></tr2:Profiles>
<tr2:Profiles token="prof_meta"><tr2:Name>Meta</tr2:Name><tr2:Configurations></tr2:Configurations></tr2:Profiles>
</tr2:GetProfilesResponse>`, d.gov, d.encoder, d.width, d.height, d.cbr, d.fps, d.bitrate, audio))
	case "GetVideoEncoderConfigurationOptions":
		fmt.Fprintf(w, media2Envelope, media2OptionsXML)
	case "SetVideoEncoderConfiguration":
		if !d.ignore {
			d.encoder = between(body, "<tt:Encoding>", "</tt:Encoding>")
			d.width = atoi(between(body, "<tt:Width>", "</tt:Width>"))
			d.height = atoi(between(body, "<tt:Height>", "</tt:Height>"))
			d.fps = between(body, "<tt:FrameRateLimit>", "</tt:FrameRateLimit>")
			d.bitrate = between(body, "<tt:BitrateLimit>", "</tt:BitrateLimit>")
			d.gov = between(body, `GovLength="`, `"`)
			if strings.Contains(body, `ConstantBitRate="true"`) {
				d.cbr = "true"
			} else {
				d.cbr = "false"
			}
		}
		fmt.Fprintf(w, media2Envelope, `<tr2:SetVideoEncoderConfigurationResponse/>`)
	default:
		http.Error(w, "unknown", http.StatusBadRequest)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return s
	}
	return s[:j]
}

func TestVideoEncoderOptionsParsesPerCodecSets(t *testing.T) {
	d := newFakeMedia2Device(t)
	opts, err := VideoEncoderOptions(context.Background(), d.conn(t, nil), "prof_main")
	if err != nil {
		t.Fatal(err)
	}
	h265, ok := opts.For("HEVC")
	if !ok {
		t.Fatal("H265 options missing")
	}
	h264, ok := opts.For("h264")
	if !ok {
		t.Fatal("H264 options missing")
	}
	if len(h265.Resolutions) != 2 || h265.Resolutions[0] != (VideoResolution{2560, 1440}) {
		t.Fatalf("H265 resolutions = %+v", h265.Resolutions)
	}
	if len(h264.Resolutions) != 2 || h264.Resolutions[1] != (VideoResolution{1280, 720}) {
		t.Fatalf("H264 resolutions = %+v", h264.Resolutions)
	}
	if h265.FPS != (FloatRange{5, 25}) || len(h265.FrameRates) != 4 || h264.FPS.Max != 25 || len(h264.FrameRates) != 5 {
		t.Fatalf("fps h265=%+v h264=%+v", h265, h264)
	}
	if h265.Bitrate != (IntRange{32, 16384}) || h264.Bitrate != (IntRange{64, 8192}) {
		t.Fatalf("bitrate h265=%+v h264=%+v", h265.Bitrate, h264.Bitrate)
	}
	if h265.GOP != (IntRange{1, 100}) || h264.GOP != (IntRange{1, 60}) {
		t.Fatalf("gop h265=%+v h264=%+v", h265.GOP, h264.GOP)
	}
	if h265.Quality != (FloatRange{1, 6}) || strings.Join(h265.Profiles, ",") != "Main,Main10" {
		t.Fatalf("quality/profiles %+v", h265)
	}
	if strings.Join(h265.BitrateModes, ",") != "constant,variable" {
		t.Fatalf("modes = %v", h265.BitrateModes)
	}
	if !strings.Contains(d.lastBody("GetVideoEncoderConfigurationOptions"), "venc_1") {
		t.Fatal("options must be requested for the profile's encoder configuration token")
	}
}

func TestVideoEncoderOptionsLeavesMissingItemsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "GetProfiles") {
			fmt.Fprintf(w, media2Envelope, `<tr2:GetProfilesResponse><tr2:Profiles token="p"><tr2:Configurations><tr2:VideoEncoder token="v"><tt:Encoding>H264</tt:Encoding></tr2:VideoEncoder></tr2:Configurations></tr2:Profiles></tr2:GetProfilesResponse>`)
			return
		}
		fmt.Fprintf(w, media2Envelope, `<tr2:GetVideoEncoderConfigurationOptionsResponse><tr2:Options><tt:Encoding>H264</tt:Encoding></tr2:Options></tr2:GetVideoEncoderConfigurationOptionsResponse>`)
	}))
	defer srv.Close()
	conn, _ := NewConn(NewClient(nil, Config{}), "", srv.URL)
	opts, err := VideoEncoderOptions(context.Background(), conn, "p")
	if err != nil {
		t.Fatal(err)
	}
	co, _ := opts.For("H264")
	if len(co.Resolutions) != 0 || len(co.FrameRates) != 0 || co.FPS != (FloatRange{}) || co.Bitrate != (IntRange{}) ||
		co.GOP != (IntRange{}) || co.Quality != (FloatRange{}) || len(co.Profiles) != 0 || len(co.BitrateModes) != 0 {
		t.Fatalf("absent fields must stay empty: %+v", co)
	}
}

func TestVideoEncoderOptionsMedia1(t *testing.T) {
	const ns = `xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "GetProfiles") {
			fmt.Fprintf(w, `<s:Envelope %s><s:Body><trt:GetProfilesResponse><trt:Profiles token="p1"><tt:Name>P</tt:Name><tt:VideoEncoderConfiguration token="v1"><tt:Encoding>H264</tt:Encoding></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse></s:Body></s:Envelope>`, ns)
			return
		}
		fmt.Fprintf(w, `<s:Envelope %s><s:Body><trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options>
<tt:QualityRange><tt:Min>1</tt:Min><tt:Max>10</tt:Max></tt:QualityRange>
<tt:JPEG><tt:ResolutionsAvailable><tt:Width>640</tt:Width><tt:Height>480</tt:Height></tt:ResolutionsAvailable><tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>15</tt:Max></tt:FrameRateRange></tt:JPEG>
<tt:H264><tt:ResolutionsAvailable><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:ResolutionsAvailable><tt:GovLengthRange><tt:Min>1</tt:Min><tt:Max>120</tt:Max></tt:GovLengthRange><tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>30</tt:Max></tt:FrameRateRange><tt:H264ProfilesSupported>Main</tt:H264ProfilesSupported></tt:H264>
<tt:Extension><tt:H264><tt:BitrateRange><tt:Min>128</tt:Min><tt:Max>8192</tt:Max></tt:BitrateRange></tt:H264></tt:Extension>
</trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse></s:Body></s:Envelope>`, ns)
	}))
	defer srv.Close()
	conn, _ := NewConn(NewClient(nil, Config{}), srv.URL, "")
	opts, err := VideoEncoderOptions(context.Background(), conn, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opts.For("H265"); ok {
		t.Fatal("Media1 must not invent H265")
	}
	h264, _ := opts.For("H264")
	if h264.GOP != (IntRange{1, 120}) || h264.FPS != (FloatRange{1, 30}) || h264.Bitrate != (IntRange{128, 8192}) || h264.Quality.Max != 10 {
		t.Fatalf("h264 = %+v", h264)
	}
	mj, ok := opts.For("MJPEG")
	if !ok || mj.Bitrate != (IntRange{}) || len(mj.Resolutions) != 1 {
		t.Fatalf("mjpeg = %+v", mj)
	}
}

func TestNewConnRejectsInvalidEndpoints(t *testing.T) {
	for _, bad := range []string{"ftp://x/y", "http://user:pw@host/x", "http://h/x?q=1"} {
		if _, err := NewConn(NewClient(nil, Config{}), bad, ""); !errors.Is(err, ErrInvalidEndpoint) {
			t.Fatalf("%q err = %v", bad, err)
		}
	}
	if _, err := NewConn(NewClient(nil, Config{}), "", ""); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("no endpoints err = %v", err)
	}
	if _, err := VideoEncoderOptions(context.Background(), Conn{}, "p"); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("zero conn err = %v", err)
	}
}
