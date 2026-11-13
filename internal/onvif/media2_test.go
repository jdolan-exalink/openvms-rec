package onvif

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const media2Envelope = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tr2="http://www.onvif.org/ver20/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>%s</s:Body></s:Envelope>`

const media2ConfigsXML = `<tr2:GetVideoEncoderConfigurationsResponse>
  <tr2:Configurations token="VideoEncoder_1" GovLength="50" Profile="Main">
    <tt:Name>Main &amp; Stream</tt:Name>
    <tt:UseCount>1</tt:UseCount>
    <tt:Encoding>H265</tt:Encoding>
    <tt:Resolution><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:Resolution>
    <tt:RateControl ConstantBitRate="false"><tt:FrameRateLimit>20</tt:FrameRateLimit><tt:BitrateLimit>6144</tt:BitrateLimit></tt:RateControl>
    <tt:Multicast><tt:Address><tt:Type>IPv4</tt:Type><tt:IPv4Address>0.0.0.0</tt:IPv4Address></tt:Address><tt:Port>0</tt:Port><tt:TTL>1</tt:TTL><tt:AutoStart>false</tt:AutoStart></tt:Multicast>
    <tt:Quality>5</tt:Quality>
  </tr2:Configurations>
  <tr2:Configurations token="VideoEncoder_2" GovLength="25" Profile="High">
    <tt:Name>Sub</tt:Name>
    <tt:UseCount>1</tt:UseCount>
    <tt:Encoding>H264</tt:Encoding>
    <tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution>
    <tt:RateControl ConstantBitRate="true"><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>512</tt:BitrateLimit></tt:RateControl>
    <tt:Quality>3</tt:Quality>
  </tr2:Configurations>
</tr2:GetVideoEncoderConfigurationsResponse>`

const media2OptionsXML = `<tr2:GetVideoEncoderConfigurationOptionsResponse>
  <tr2:Options GovLengthRange="1 100" FrameRatesSupported="5 10 15 25" ProfilesSupported="Main Main10" ConstantBitRateSupported="true">
    <tt:Encoding>H265</tt:Encoding>
    <tt:QualityRange><tt:Min>1</tt:Min><tt:Max>6</tt:Max></tt:QualityRange>
    <tt:ResolutionsAvailable><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:ResolutionsAvailable>
    <tt:ResolutionsAvailable><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:ResolutionsAvailable>
    <tt:BitrateRange><tt:Min>32</tt:Min><tt:Max>16384</tt:Max></tt:BitrateRange>
  </tr2:Options>
  <tr2:Options GovLengthRange="1 60" FrameRatesSupported="5 10 15 20 25" ProfilesSupported="Baseline Main High" ConstantBitRateSupported="true">
    <tt:Encoding>H264</tt:Encoding>
    <tt:QualityRange><tt:Min>1</tt:Min><tt:Max>6</tt:Max></tt:QualityRange>
    <tt:ResolutionsAvailable><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:ResolutionsAvailable>
    <tt:ResolutionsAvailable><tt:Width>1280</tt:Width><tt:Height>720</tt:Height></tt:ResolutionsAvailable>
    <tt:BitrateRange><tt:Min>64</tt:Min><tt:Max>8192</tt:Max></tt:BitrateRange>
  </tr2:Options>
</tr2:GetVideoEncoderConfigurationOptionsResponse>`

type media2Recorder struct {
	requests []string
	actions  []string
}

func (m *media2Recorder) client(t *testing.T, responses map[string]string) *Media2Client {
	t.Helper()
	endpoint, err := ParseEndpoint("http://192.168.1.50/onvif/media2")
	if err != nil {
		t.Fatal(err)
	}
	transport := mediaRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		m.requests = append(m.requests, string(body))
		m.actions = append(m.actions, r.Header.Get("SOAPAction")+"|"+r.Header.Get("Content-Type"))
		for op, resp := range responses {
			if strings.Contains(string(body), "<tr2:"+op) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Replace(media2Envelope, "%s", resp, 1))), Header: make(http.Header)}, nil
			}
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	return NewMedia2Client(NewClient(transport, Config{}), endpoint)
}

func TestMedia2GetVideoEncoderConfigurations(t *testing.T) {
	rec := &media2Recorder{}
	c := rec.client(t, map[string]string{"GetVideoEncoderConfigurations": media2ConfigsXML})
	cfgs, err := c.GetVideoEncoderConfigurations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configurations", len(cfgs))
	}
	main := cfgs[0]
	if main.Token != "VideoEncoder_1" || main.Name != "Main & Stream" || main.Encoding != "H265" || main.Profile != "Main" || main.GovLength != 50 {
		t.Errorf("main = %+v", main)
	}
	if main.Resolution.Width != 2560 || main.Resolution.Height != 1440 || main.RateControl.FrameRateLimit != 20 || main.RateControl.BitrateLimit != 6144 || main.RateControl.ConstantBitRate || main.Quality != 5 {
		t.Errorf("main details = %+v", main)
	}
	if cfgs[1].Encoding != "H264" || !cfgs[1].RateControl.ConstantBitRate {
		t.Errorf("sub = %+v", cfgs[1])
	}
	if !strings.Contains(rec.actions[0], "http://www.onvif.org/ver20/media/wsdl/GetVideoEncoderConfigurations") {
		t.Errorf("SOAPAction = %q", rec.actions[0])
	}
}

func TestMedia2GetProfilesAndStreamURI(t *testing.T) {
	rec := &media2Recorder{}
	c := rec.client(t, map[string]string{
		"GetProfiles":  `<tr2:GetProfilesResponse><tr2:Profiles token="main-profile" fixed="true"><tt:Name>Main</tt:Name><tt:VideoSourceConfiguration token="source-main"/><tt:VideoEncoderConfiguration token="encoder-main"><tt:Name>Main encoder</tt:Name><tt:Encoding>H265</tt:Encoding><tt:Resolution><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:BitrateLimit>6144</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration><tt:PTZConfiguration token="ptz-main"/></tr2:Profiles></tr2:GetProfilesResponse>`,
		"GetStreamUri": `<tr2:GetStreamUriResponse><tr2:Uri>rtsp://192.0.2.10/main</tr2:Uri></tr2:GetStreamUriResponse>`,
	})
	profiles, err := c.GetProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Token != "main-profile" || profiles[0].Name != "Main" || !profiles[0].Fixed {
		t.Fatalf("profiles = %+v", profiles)
	}
	if profiles[0].VideoSourceConfiguration == nil || profiles[0].VideoSourceConfiguration.Token != "source-main" || profiles[0].PTZConfiguration == nil || profiles[0].PTZConfiguration.Token != "ptz-main" {
		t.Fatalf("profile configuration tokens = %+v", profiles[0])
	}
	if cfg := profiles[0].VideoEncoderConfiguration; cfg == nil || cfg.Token != "encoder-main" || cfg.Encoding != "H265" || cfg.Resolution != (VideoResolution{Width: 2560, Height: 1440}) {
		t.Fatalf("video encoder configuration = %+v", cfg)
	}
	uri, err := c.GetStreamURI(context.Background(), "main-profile")
	if err != nil || uri != "rtsp://192.0.2.10/main" {
		t.Fatalf("stream URI = %q, err = %v", uri, err)
	}
	if !strings.Contains(rec.actions[0], media2Namespace+"/GetProfiles") || !strings.Contains(rec.actions[1], media2Namespace+"/GetStreamUri") {
		t.Fatalf("Media2 actions = %+v", rec.actions)
	}
}

func TestMedia2GetVideoEncoderConfigurationOptions(t *testing.T) {
	rec := &media2Recorder{}
	c := rec.client(t, map[string]string{"GetVideoEncoderConfigurationOptions": media2OptionsXML})
	opts, err := c.GetVideoEncoderConfigurationOptions(context.Background(), "VideoEncoder_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.requests[0], "<tr2:ConfigurationToken>VideoEncoder_1</tr2:ConfigurationToken>") {
		t.Errorf("the options must be asked for that configuration: %s", rec.requests[0])
	}
	if len(opts) != 2 || opts[0].Encoding != "H265" || opts[1].Encoding != "H264" {
		t.Fatalf("options = %+v", opts)
	}
	h265 := opts[0]
	if got := strings.Join(h265.ProfilesSupported, ","); got != "Main,Main10" {
		t.Errorf("profiles = %s", got)
	}
	if h265.GovLength.Min != 1 || h265.GovLength.Max != 100 || h265.Bitrate.Min != 32 || h265.Bitrate.Max != 16384 || h265.Quality.Min != 1 || h265.Quality.Max != 6 {
		t.Errorf("ranges = %+v", h265)
	}
	if len(h265.FrameRates) != 4 || h265.FrameRates[3] != 25 || len(h265.Resolutions) != 2 || h265.Resolutions[1].Width != 1920 {
		t.Errorf("lists = %+v", h265)
	}
}

func TestMedia2SetVideoEncoderConfigurationSendsTheWholeConfiguration(t *testing.T) {
	rec := &media2Recorder{}
	c := rec.client(t, map[string]string{"SetVideoEncoderConfiguration": `<tr2:SetVideoEncoderConfigurationResponse/>`})
	// #nosec G101 -- Token is the ONVIF encoder configuration token, not a credential.
	err := c.SetVideoEncoderConfiguration(context.Background(), VideoEncoder2Configuration{
		Token: "VideoEncoder_1", Name: `Main <&> "x"`, UseCount: 1, Encoding: "H264", Profile: "High", GovLength: 50,
		Resolution:  VideoResolution{Width: 1920, Height: 1080},
		RateControl: VideoRateControl2{FrameRateLimit: 20, BitrateLimit: 4096},
		Quality:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := rec.requests[0]
	for _, want := range []string{
		`<tr2:SetVideoEncoderConfiguration`, `token="VideoEncoder_1"`, `Profile="High"`, `GovLength="50"`,
		`<tt:Encoding>H264</tt:Encoding>`, `<tt:Width>1920</tt:Width>`, `<tt:Height>1080</tt:Height>`,
		`<tt:FrameRateLimit>20</tt:FrameRateLimit>`, `<tt:BitrateLimit>4096</tt:BitrateLimit>`, `<tt:Quality>5</tt:Quality>`,
		`Main &lt;&amp;&gt; &#34;x&#34;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ForcePersistence") || strings.Contains(body, "Multicast") {
		t.Errorf("Media2 has no ForcePersistence and the multicast block is not re-sent:\n%s", body)
	}
}

func TestMedia2SetFailureIsReported(t *testing.T) {
	rec := &media2Recorder{}
	c := rec.client(t, map[string]string{})
	if err := c.SetVideoEncoderConfiguration(context.Background(), VideoEncoder2Configuration{Token: "t", Encoding: "H264"}); err == nil {
		t.Fatal("a camera error must surface")
	}
}

func TestPlanEncodingChange(t *testing.T) {
	// #nosec G101 -- Token is the ONVIF encoder configuration token, not a credential.
	current := VideoEncoder2Configuration{
		Token: "VideoEncoder_1", Name: "Main", UseCount: 1, Encoding: "H265", Profile: "Main", GovLength: 50,
		Resolution:  VideoResolution{Width: 2560, Height: 1440},
		RateControl: VideoRateControl2{FrameRateLimit: 20, BitrateLimit: 6144},
		Quality:     5,
	}
	opts := []VideoEncoder2Options{
		{Encoding: "H265", ProfilesSupported: []string{"Main", "Main10"}, GovLength: IntRange{1, 100}, FrameRates: []float64{5, 10, 15, 25}, Resolutions: []VideoResolution{{2560, 1440}, {1920, 1080}}, Bitrate: IntRange{32, 16384}, Quality: FloatRange{1, 6}},
		{Encoding: "H264", ProfilesSupported: []string{"Baseline", "Main", "High"}, GovLength: IntRange{1, 60}, FrameRates: []float64{5, 10, 15, 20, 25}, Resolutions: []VideoResolution{{1920, 1080}, {1280, 720}}, Bitrate: IntRange{64, 8192}, Quality: FloatRange{1, 6}},
	}

	t.Run("keeps what the new encoding allows and moves the rest to the nearest allowed value", func(t *testing.T) {
		got, err := PlanEncodingChange(current, opts, "h264")
		if err != nil {
			t.Fatal(err)
		}
		if got.Encoding != "H264" || got.Token != "VideoEncoder_1" || got.Name != "Main" {
			t.Errorf("identity: %+v", got)
		}
		if got.Profile != "Main" || got.GovLength != 50 || got.RateControl.FrameRateLimit != 20 || got.Quality != 5 {
			t.Errorf("kept: %+v", got)
		}
		if got.Resolution != (VideoResolution{Width: 1920, Height: 1080}) {
			t.Errorf("2560x1440 is not offered for H264, the nearest one is 1920x1080, got %+v", got.Resolution)
		}
		if got.RateControl.BitrateLimit != 6144 {
			t.Errorf("a bitrate inside the range is kept, got %d", got.RateControl.BitrateLimit)
		}
	})

	t.Run("picks Main when the current profile does not exist for the target", func(t *testing.T) {
		c := current
		c.Profile = "Main10"
		got, err := PlanEncodingChange(c, opts, "H264")
		if err != nil || got.Profile != "Main" {
			t.Fatalf("profile = %q, err = %v", got.Profile, err)
		}
	})

	t.Run("falls back to the first supported profile when Main is not offered", func(t *testing.T) {
		o := []VideoEncoder2Options{{Encoding: "H264", ProfilesSupported: []string{"Baseline", "High"}, Resolutions: []VideoResolution{{2560, 1440}}}}
		c := current
		c.Profile = "Main10"
		got, err := PlanEncodingChange(c, o, "H264")
		if err != nil || got.Profile != "Baseline" {
			t.Fatalf("profile = %q, err = %v", got.Profile, err)
		}
	})

	t.Run("clamps ranges and snaps the frame rate down", func(t *testing.T) {
		c := current
		c.GovLength, c.RateControl.BitrateLimit, c.RateControl.FrameRateLimit, c.Quality = 200, 20000, 22, 9
		got, err := PlanEncodingChange(c, opts, "H264")
		if err != nil {
			t.Fatal(err)
		}
		if got.GovLength != 60 || got.RateControl.BitrateLimit != 8192 || got.RateControl.FrameRateLimit != 20 || got.Quality != 6 {
			t.Errorf("clamped: %+v", got)
		}
	})

	t.Run("an encoding the camera did not list is refused", func(t *testing.T) {
		if _, err := PlanEncodingChange(current, opts[:1], "H264"); !errors.Is(err, ErrEncodingUnsupported) {
			t.Fatalf("err = %v, want ErrEncodingUnsupported", err)
		}
		if _, err := PlanEncodingChange(current, opts, "AV1"); !errors.Is(err, ErrEncodingUnsupported) {
			t.Fatalf("err = %v, want ErrEncodingUnsupported", err)
		}
	})

	t.Run("changing to the encoding it already has is refused", func(t *testing.T) {
		if _, err := PlanEncodingChange(current, opts, "H265"); !errors.Is(err, ErrEncodingUnchanged) {
			t.Fatalf("err = %v, want ErrEncodingUnchanged", err)
		}
	})

	t.Run("does not touch the configuration it was given", func(t *testing.T) {
		before := current
		_, _ = PlanEncodingChange(current, opts, "H264")
		if current != before {
			t.Fatalf("input mutated: %+v", current)
		}
	})
}
