package onvif

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fakeEnv = `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>%s</s:Body></s:Envelope>`

// fakeCamera answers like a Media1-only camera whose clock is one hour ahead.
// Every call but GetSystemDateAndTime requires a WS-Security header.
func fakeCamera(t *testing.T, wantUser string) *httptest.Server {
	camNow := time.Now().UTC().Add(time.Hour)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/soap+xml")
		reply := func(inner string) { fmt.Fprintf(w, fakeEnv, inner) }
		if strings.Contains(body, "GetSystemDateAndTime") {
			reply(fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:DateTimeType>NTP</tt:DateTimeType><tt:UTCDateTime><tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time><tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`,
				camNow.Hour(), camNow.Minute(), camNow.Second(), camNow.Year(), int(camNow.Month()), camNow.Day()))
			return
		}
		if !strings.Contains(body, "<wsse:Username>"+wantUser+"</wsse:Username>") && !strings.Contains(body, ">"+wantUser+"</") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(body, "GetDeviceInformation"):
			reply(`<tds:GetDeviceInformationResponse><tds:Manufacturer>Provision-ISR</tds:Manufacturer><tds:Model>I4-320IPSN-VF</tds:Model><tds:FirmwareVersion>1.2.3</tds:FirmwareVersion><tds:SerialNumber>SN1</tds:SerialNumber><tds:HardwareId>HW</tds:HardwareId></tds:GetDeviceInformationResponse>`)
		case strings.Contains(body, "GetServices"):
			reply(`<tds:GetServicesResponse><tds:Service><tds:Namespace>http://www.onvif.org/ver10/device/wsdl</tds:Namespace><tds:XAddr>` + srv.URL + `/onvif/device_service</tds:XAddr><tds:Version><tt:Major>2</tt:Major><tt:Minor>6</tt:Minor></tds:Version></tds:Service><tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>` + srv.URL + `/onvif/media</tds:XAddr><tds:Version><tt:Major>2</tt:Major><tt:Minor>6</tt:Minor></tds:Version></tds:Service></tds:GetServicesResponse>`)
		case strings.Contains(body, "GetProfiles"):
			reply(`<trt:GetProfilesResponse><trt:Profiles token="p1"><tt:Name>Sub</tt:Name><tt:VideoEncoderConfiguration token="e1"><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>512</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration></trt:Profiles><trt:Profiles token="p0"><tt:Name>Main</tt:Name><tt:VideoEncoderConfiguration token="e0"><tt:Encoding>H265</tt:Encoding><tt:Resolution><tt:Width>2592</tt:Width><tt:Height>1520</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse>`)
		case strings.Contains(body, "GetStreamUri") && strings.Contains(body, ">p0<"):
			reply(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://admin:pw@0.0.0.0:554/profile1</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`)
		case strings.Contains(body, "GetStreamUri"):
			reply(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://127.0.0.1:554/profile2</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeDevice(t *testing.T) {
	srv := fakeCamera(t, "viewer")
	ep, err := ParseEndpoint(srv.URL + "/onvif/device_service")
	if err != nil {
		t.Fatal(err)
	}
	res, err := ProbeDevice(context.Background(), http.DefaultTransport, ep, "viewer", "pw-secret")
	if err != nil {
		t.Fatal(err)
	}
	if res.Manufacturer != "Provision-ISR" || res.Model != "I4-320IPSN-VF" || res.Firmware != "1.2.3" {
		t.Fatalf("info %+v", res)
	}
	if res.ClockOffsetMS < 3500_000 || res.ClockOffsetMS > 3700_000 {
		t.Fatalf("offset %d", res.ClockOffsetMS)
	}
	if res.MainURL != "rtsp://127.0.0.1:554/profile1" || res.SubURL != "rtsp://127.0.0.1:554/profile2" {
		t.Fatalf("streams %q %q", res.MainURL, res.SubURL)
	}
	if len(res.Profiles) != 2 || res.Profiles[1].Encoding != "H265" || res.Profiles[1].Width != 2592 {
		t.Fatalf("profiles %+v", res.Profiles)
	}
	found := false
	for _, c := range res.Capabilities {
		if c.Capability == "onvif.media.stream_uri" && c.Status == CapConfirmed {
			found = true
		}
	}
	if !found {
		t.Fatalf("caps %+v", res.Capabilities)
	}

	if _, err := ProbeDevice(context.Background(), http.DefaultTransport, ep, "intruder", "x"); err != ErrProbeAuth {
		t.Fatalf("want auth error, got %v", err)
	}
}
