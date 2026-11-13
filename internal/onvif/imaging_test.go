package onvif

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type imagingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f imagingRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestImagingClientOperations(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}

	var lastBody string
	var setBody string
	setComplete := false
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)

		if strings.Contains(lastBody, "GetOptions") {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body><timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Brightness><tt:ColorSaturation><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:ColorSaturation><tt:Contrast><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Contrast><tt:Sharpness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Sharpness><tt:IrCutFilterModes>ON</tt:IrCutFilterModes><tt:IrCutFilterModes>OFF</tt:IrCutFilterModes><tt:IrCutFilterModes>AUTO</tt:IrCutFilterModes><tt:WideDynamicRange><tt:Mode>ON</tt:Mode><tt:Mode>OFF</tt:Mode><tt:Level><tt:Min>0</tt:Min><tt:Max>5</tt:Max></tt:Level></tt:WideDynamicRange></timg:ImagingOptions></timg:GetOptionsResponse></s:Body></s:Envelope>`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(xmlResp)), Header: make(http.Header)}, nil
		}
		if strings.Contains(lastBody, "GetImagingSettings") {
			settings := `<tt:Brightness>65.0</tt:Brightness><tt:ColorSaturation>70.0</tt:ColorSaturation><tt:Contrast>55.0</tt:Contrast><tt:Sharpness>45.0</tt:Sharpness><tt:IrCutFilter>AUTO</tt:IrCutFilter><tt:WideDynamicRange><tt:Mode>ON</tt:Mode><tt:Level>2.0</tt:Level></tt:WideDynamicRange>`
			if setComplete {
				settings = `<tt:Brightness>80</tt:Brightness><tt:ColorSaturation>50</tt:ColorSaturation><tt:Contrast>60</tt:Contrast><tt:Sharpness>50</tt:Sharpness><tt:IrCutFilter>ON</tt:IrCutFilter><tt:WideDynamicRange><tt:Mode>OFF</tt:Mode><tt:Level>0</tt:Level></tt:WideDynamicRange>`
			}
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<s:Body>
					<timg:GetImagingSettingsResponse>
						<timg:ImagingSettings>
							` + settings + `
						</timg:ImagingSettings>
					</timg:GetImagingSettingsResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		if strings.Contains(lastBody, "SetImagingSettings") {
			setBody = lastBody
			setComplete = true
			if !strings.Contains(lastBody, "80.000000") || !strings.Contains(lastBody, "IrCutFilter") {
				t.Errorf("SetImagingSettings missing supported controls: %s", lastBody)
			}
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl">
				<s:Body>
					<timg:SetImagingSettingsResponse/>
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
	imagingClient := NewImagingClient(client, endpoint)

	settings, err := imagingClient.GetImagingSettings(context.Background(), "VideoSource_1")
	if err != nil {
		t.Fatalf("GetImagingSettings failed: %v", err)
	}
	if settings.Brightness != 65.0 || settings.IrCutFilter != "AUTO" || settings.WideDynamicRange == nil || settings.WideDynamicRange.Mode != "ON" {
		t.Errorf("unexpected settings: %+v", settings)
	}

	err = imagingClient.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{
		Brightness:      float64ptr(80),
		Contrast:        float64ptr(60),
		ColorSaturation: float64ptr(50),
		Sharpness:       float64ptr(50),
		IrCutFilter:     stringptr("ON"),
		WideDynamicRange: &WideDynamicRangeUpdate{
			Mode:  stringptr("OFF"),
			Level: float64ptr(0),
		},
	})
	if err != nil {
		t.Fatalf("SetImagingSettings failed: %v", err)
	}
	if !strings.Contains(setBody, "SetImagingSettings") || !strings.Contains(setBody, "80.000000") || !strings.Contains(setBody, "IrCutFilter") || !strings.Contains(setBody, ">ON<") {
		t.Errorf("SetImagingSettings unexpected body: %s", setBody)
	}
}

func TestGetImagingOptions(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "GetOptions") {
			t.Fatalf("request does not contain GetOptions: %s", body)
		}
		response := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body><timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Brightness><tt:Contrast><tt:Min>0</tt:Min></tt:Contrast><tt:IrCutFilterModes>ON</tt:IrCutFilterModes><tt:IrCutFilterModes>AUTO</tt:IrCutFilterModes><tt:WideDynamicRange><tt:Mode>ON</tt:Mode><tt:Level><tt:Min>1</tt:Min><tt:Max>5</tt:Max></tt:Level></tt:WideDynamicRange></timg:ImagingOptions></timg:GetOptionsResponse></s:Body></s:Envelope>`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	options, err := client.GetImagingOptions(context.Background(), "VideoSource_1")
	if err != nil {
		t.Fatalf("GetImagingOptions failed: %v", err)
	}
	if options.Brightness == nil || options.Brightness.Min != 0 || options.Brightness.Max != 100 {
		t.Errorf("brightness range = %+v, want [0,100]", options.Brightness)
	}
	if options.Contrast != nil {
		t.Errorf("unsupported or incomplete contrast range must be absent, got %+v", options.Contrast)
	}
	if got := options.IrCutFilterModes; len(got) != 2 || got[0] != "ON" || got[1] != "AUTO" {
		t.Errorf("IR cut modes = %v, want [ON AUTO]", got)
	}
	if options.WideDynamicRange == nil || options.WideDynamicRange.Level == nil || options.WideDynamicRange.Level.Max != 5 || len(options.WideDynamicRange.Modes) != 1 || options.WideDynamicRange.Modes[0] != "ON" {
		t.Errorf("WDR options = %+v", options.WideDynamicRange)
	}
}

func TestSetImagingSettingsRequiresReadbackMatch(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	setCalls := 0
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "GetOptions"):
			return imagingSOAPResponse(`<timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Brightness></timg:ImagingOptions></timg:GetOptionsResponse>`), nil
		case strings.Contains(string(body), "SetImagingSettings"):
			setCalls++
			if !strings.Contains(string(body), "Brightness") || strings.Contains(string(body), "Contrast") {
				t.Errorf("set request must include advertised brightness only: %s", body)
			}
			return imagingSOAPResponse(`<timg:SetImagingSettingsResponse/>`), nil
		case strings.Contains(string(body), "GetImagingSettings"):
			response := `<timg:GetImagingSettingsResponse><timg:ImagingSettings><tt:Brightness>49</tt:Brightness></timg:ImagingSettings></timg:GetImagingSettingsResponse>`
			return imagingSOAPResponse(response), nil
		default:
			t.Fatalf("unexpected request: %s", body)
			return nil, nil
		}
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	err = client.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{Brightness: float64ptr(50)})
	if err == nil {
		t.Fatal("SetImagingSettings succeeded despite readback mismatch")
	}
	if setCalls != 1 {
		t.Errorf("SetImagingSettings calls = %d, want 1", setCalls)
	}
}

func TestSetImagingSettingsWritesOnlyRequestedControls(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	var write string
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "GetOptions"):
			return imagingSOAPResponse(`<timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Brightness><tt:Contrast><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Contrast><tt:Sharpness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Sharpness></timg:ImagingOptions></timg:GetOptionsResponse>`), nil
		case strings.Contains(string(body), "SetImagingSettings"):
			write = string(body)
			return imagingSOAPResponse(`<timg:SetImagingSettingsResponse/>`), nil
		default:
			return imagingSOAPResponse(`<timg:GetImagingSettingsResponse><timg:ImagingSettings><tt:Brightness>42</tt:Brightness></timg:ImagingSettings></timg:GetImagingSettingsResponse>`), nil
		}
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	if err := client.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{Brightness: float64ptr(42)}); err != nil {
		t.Fatalf("SetImagingSettings failed: %v", err)
	}
	if !strings.Contains(write, "Brightness") || strings.Contains(write, "Contrast") || strings.Contains(write, "Sharpness") {
		t.Errorf("write must contain only the requested control: %s", write)
	}
}

func TestSetImagingSettingsRejectsOutsideAdvertisedRangeBeforeWrite(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	setCalls := 0
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "GetOptions") {
			return imagingSOAPResponse(`<timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>10</tt:Min><tt:Max>90</tt:Max></tt:Brightness></timg:ImagingOptions></timg:GetOptionsResponse>`), nil
		}
		setCalls++
		return imagingSOAPResponse(`<timg:SetImagingSettingsResponse/>`), nil
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	if err := client.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{Brightness: float64ptr(100)}); err == nil {
		t.Fatal("SetImagingSettings accepted a value outside the advertised range")
	}
	if setCalls != 0 {
		t.Errorf("SetImagingSettings calls = %d, want 0", setCalls)
	}
}

func TestSetImagingSettingsFailsWhenReadbackCannotBeRead(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "GetOptions") {
			return imagingSOAPResponse(`<timg:GetOptionsResponse><timg:ImagingOptions><tt:Brightness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Brightness></timg:ImagingOptions></timg:GetOptionsResponse>`), nil
		}
		if strings.Contains(string(body), "SetImagingSettings") {
			return imagingSOAPResponse(`<timg:SetImagingSettingsResponse/>`), nil
		}
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	if err := client.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{Brightness: float64ptr(50)}); err == nil {
		t.Fatal("SetImagingSettings succeeded without a readable verification result")
	}
}

func TestSetImagingSettingsDoesNotWriteWhenNoOptionsAreAdvertised(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/imaging_service")
	if err != nil {
		t.Fatal(err)
	}
	setCalls := 0
	transport := imagingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "GetOptions") {
			return imagingSOAPResponse(`<timg:GetOptionsResponse><timg:ImagingOptions/></timg:GetOptionsResponse>`), nil
		}
		setCalls++
		return imagingSOAPResponse(`<timg:SetImagingSettingsResponse/>`), nil
	})
	client := NewImagingClient(NewClient(transport, Config{}), endpoint)
	if err := client.SetImagingSettings(context.Background(), "VideoSource_1", ImagingSettingsUpdate{Brightness: float64ptr(50)}); err == nil {
		t.Fatal("SetImagingSettings succeeded without any advertised controls")
	}
	if setCalls != 0 {
		t.Errorf("SetImagingSettings calls = %d, want 0", setCalls)
	}
}

func float64ptr(value float64) *float64 { return &value }
func stringptr(value string) *string    { return &value }

func imagingSOAPResponse(operation string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:timg="http://www.onvif.org/ver20/imaging/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>` + operation + `</s:Body></s:Envelope>`)),
		Header:     make(http.Header),
	}
}
