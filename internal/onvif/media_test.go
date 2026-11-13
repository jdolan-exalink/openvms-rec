package onvif

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mediaRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f mediaRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestMediaClientGetProfilesAndStreamUri(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/media_service")
	if err != nil {
		t.Fatal(err)
	}

	transport := mediaRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		sBody := string(body)

		if strings.Contains(sBody, "GetProfiles") {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<s:Body>
					<trt:GetProfilesResponse>
						<trt:Profiles token="Profile_Main" fixed="true">
							<tt:Name>MainStream</tt:Name>
							<tt:VideoEncoderConfiguration token="vec_main">
								<tt:Name>H264_Main</tt:Name>
								<tt:Encoding>H264</tt:Encoding>
								<tt:Resolution><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution>
								<tt:RateControl><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl>
							</tt:VideoEncoderConfiguration>
						</trt:Profiles>
						<trt:Profiles token="Profile_Sub" fixed="true">
							<tt:Name>SubStream</tt:Name>
							<tt:VideoEncoderConfiguration token="vec_sub">
								<tt:Name>H264_Sub</tt:Name>
								<tt:Encoding>H264</tt:Encoding>
								<tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution>
								<tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>512</tt:BitrateLimit></tt:RateControl>
							</tt:VideoEncoderConfiguration>
						</trt:Profiles>
					</trt:GetProfilesResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		if strings.Contains(sBody, "GetStreamUri") {
			uri := "rtsp://192.168.1.100:554/live/ch0"
			if strings.Contains(sBody, "Profile_Sub") {
				uri = "rtsp://192.168.1.100:554/live/ch1"
			}
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<s:Body>
					<trt:GetStreamUriResponse>
						<trt:MediaUri>
							<tt:Uri>` + uri + `</tt:Uri>
							<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>
							<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>
							<tt:Timeout>PT0S</tt:Timeout>
						</trt:MediaUri>
					</trt:GetStreamUriResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	client := NewClient(transport, Config{})
	mediaClient := NewMediaClient(client, endpoint)

	profiles, err := mediaClient.GetProfiles(context.Background())
	if err != nil {
		t.Fatalf("GetProfiles failed: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}
	if profiles[0].Token != "Profile_Main" || profiles[0].VideoEncoderConfiguration.Resolution.Width != 1920 {
		t.Errorf("unexpected profile[0]: %+v", profiles[0])
	}
	if profiles[1].Token != "Profile_Sub" || profiles[1].VideoEncoderConfiguration.Resolution.Width != 640 {
		t.Errorf("unexpected profile[1]: %+v", profiles[1])
	}

	mainURI, err := mediaClient.GetStreamURI(context.Background(), "Profile_Main")
	if err != nil {
		t.Fatalf("GetStreamURI main failed: %v", err)
	}
	if mainURI != "rtsp://192.168.1.100:554/live/ch0" {
		t.Errorf("mainURI = %q, want rtsp://192.168.1.100:554/live/ch0", mainURI)
	}

	subURI, err := mediaClient.GetStreamURI(context.Background(), "Profile_Sub")
	if err != nil {
		t.Fatalf("GetStreamURI sub failed: %v", err)
	}
	if subURI != "rtsp://192.168.1.100:554/live/ch1" {
		t.Errorf("subURI = %q, want rtsp://192.168.1.100:554/live/ch1", subURI)
	}
}

func TestMediaClientGetSnapshotURI(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/media_service")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reads URI with escaped profile token", func(t *testing.T) {
		var requestBody string
		var requestContentType string
		transport := mediaRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			requestBody = string(body)
			requestContentType = r.Header.Get("Content-Type")
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<s:Body>
					<trt:GetSnapshotUriResponse>
						<trt:MediaUri><tt:Uri>http://192.168.1.100/snapshot.jpg</tt:Uri></trt:MediaUri>
					</trt:GetSnapshotUriResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(xmlResp))}, nil
		})
		mediaClient := NewMediaClient(NewClient(transport, Config{}), endpoint)

		got, err := mediaClient.GetSnapshotURI(context.Background(), "main<&>")
		if err != nil {
			t.Fatalf("GetSnapshotURI failed: %v", err)
		}
		if got != "http://192.168.1.100/snapshot.jpg" {
			t.Fatalf("snapshot URI = %q", got)
		}
		if !strings.Contains(requestBody, "<trt:GetSnapshotUri") || !strings.Contains(requestBody, "<trt:ProfileToken>main&lt;&amp;&gt;</trt:ProfileToken>") {
			t.Fatalf("GetSnapshotUri request missing action or escaped profile token: %s", requestBody)
		}
		if !strings.Contains(requestContentType, `action="http://www.onvif.org/ver10/media/wsdl/GetSnapshotUri"`) {
			t.Fatalf("SOAP action = %q", requestContentType)
		}
	})

	t.Run("rejects malformed SOAP response", func(t *testing.T) {
		transport := mediaRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl"><s:Body><trt:GetStreamUriResponse/></s:Body></s:Envelope>`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(xmlResp))}, nil
		})
		mediaClient := NewMediaClient(NewClient(transport, Config{}), endpoint)

		_, err := mediaClient.GetSnapshotURI(context.Background(), "main")
		var callErr *CallError
		if !errors.As(err, &callErr) || callErr.Code != "invalid_snapshot_uri" {
			t.Fatalf("GetSnapshotURI error = %v, want invalid_snapshot_uri CallError", err)
		}
	})
}

func TestMediaClientVideoEncoderConfiguration(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/media_service")
	if err != nil {
		t.Fatal(err)
	}

	var setBodyReceived string
	transport := mediaRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		sBody := string(body)

		if strings.Contains(sBody, "GetVideoEncoderConfigurations") {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<s:Body>
					<trt:GetVideoEncoderConfigurationsResponse>
						<trt:Configurations token="vec_cfg_1">
							<tt:Name>DefaultH264</tt:Name>
							<tt:Encoding>H264</tt:Encoding>
							<tt:Resolution><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution>
							<tt:RateControl><tt:FrameRateLimit>30</tt:FrameRateLimit><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl>
						</trt:Configurations>
					</trt:GetVideoEncoderConfigurationsResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		if strings.Contains(sBody, "SetVideoEncoderConfiguration") {
			setBodyReceived = sBody
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl">
				<s:Body>
					<trt:SetVideoEncoderConfigurationResponse/>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	client := NewClient(transport, Config{})
	mediaClient := NewMediaClient(client, endpoint)

	cfgs, err := mediaClient.GetVideoEncoderConfigurations(context.Background())
	if err != nil {
		t.Fatalf("GetVideoEncoderConfigurations failed: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Token != "vec_cfg_1" {
		t.Fatalf("unexpected cfgs: %+v", cfgs)
	}

	// #nosec G101 -- test fixture parameters.
	err = mediaClient.SetVideoEncoderConfiguration(context.Background(), VideoEncoderConfiguration{
		Token:    "vec_cfg_1",
		Name:     "DefaultH264",
		Encoding: "H264",
		Resolution: VideoResolution{
			Width:  2560,
			Height: 1440,
		},
		RateControl: VideoRateControl{
			FrameRateLimit: 25,
			BitrateLimit:   6144,
		},
	})
	if err != nil {
		t.Fatalf("SetVideoEncoderConfiguration failed: %v", err)
	}
	if !strings.Contains(setBodyReceived, "2560") || !strings.Contains(setBodyReceived, "6144") {
		t.Errorf("SetVideoEncoderConfiguration payload missing updated params: %s", setBodyReceived)
	}
}
