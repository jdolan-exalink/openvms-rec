package provisionnvr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reply(w io.Writer, body string) { _, _ = io.WriteString(w, body) }

const soap = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>%s</s:Body></s:Envelope>`

// A recorder that advertises its media service on another host must not make us send credentials there.
func TestProbeONVIFStaysOnTheRecorderHost(t *testing.T) {
	var hosts []string // request paths, all served by this one test server
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "application/soap+xml")
		switch {
		case strings.Contains(body, "GetServices"):
			reply(w, strings.Replace(soap, "%s", `<tds:GetServicesResponse>`+
				`<tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>http://203.0.113.9/onvif/Media1</tds:XAddr></tds:Service>`+
				`<tds:Service><tds:Namespace>http://www.onvif.org/ver20/media/wsdl</tds:Namespace><tds:XAddr>`+srv.URL+`/onvif/Media2</tds:XAddr></tds:Service>`+
				`<tds:Service><tds:Namespace>http://www.onvif.org/ver20/ptz/wsdl</tds:Namespace><tds:XAddr>`+srv.URL+`/onvif/PTZ</tds:XAddr></tds:Service>`+
				`<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>`+srv.URL+`/onvif/Replay</tds:XAddr></tds:Service>`+
				`</tds:GetServicesResponse>`, 1))
		case strings.Contains(body, "GetProfiles"):
			reply(w, strings.Replace(soap, "%s", `<trt:GetProfilesResponse><trt:Profiles token="Profile_10_0" fixed="true"><tt:Name>Profile_10_0</tt:Name></trt:Profiles>`+
				`<trt:Profiles token="Profile_10_1" fixed="true"><tt:Name>Profile_10_1</tt:Name><tt:PTZConfiguration token="p"/></trt:Profiles></trt:GetProfilesResponse>`, 1))
		case strings.Contains(body, "GetStreamUri"):
			reply(w, strings.Replace(soap, "%s", `<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://user:pw@10.0.0.9:554/chID=10&amp;streamType=main&amp;linkType=tcp</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`, 1))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	facts, err := probeONVIF(context.Background(), DetectInput{BaseURL: srv.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Profiles != 2 || facts.PTZProfiles != 1 || !facts.StreamURIsOK {
		t.Fatalf("facts = %+v", facts)
	}
	if got := strings.Join(facts.Services, ","); got != "media,ptz,replay" {
		t.Fatalf("services = %q", got)
	}
	if facts.SampleStreamPath != "/chID=10&streamType=main&linkType=tcp" || strings.ContainsAny(facts.SampleStreamPath, "@:") {
		t.Fatalf("sample stream path = %q: it must keep neither host nor credentials", facts.SampleStreamPath)
	}
	// GetServices, GetProfiles and GetStreamUri all reached this server: the media service the recorder
	// advertised on 203.0.113.9 was used for its path only.
	if want := "/onvif/device_service,/onvif/Media1,/onvif/Media1"; strings.Join(hosts, ",") != want {
		t.Fatalf("requests = %v, want %s", hosts, want)
	}
}

func TestOnvifServiceName(t *testing.T) {
	for ns, want := range map[string]string{
		"http://www.onvif.org/ver10/media/wsdl":    "media",
		"http://www.onvif.org/ver20/media/wsdl":    "media",
		"http://www.onvif.org/ver10/deviceIO/wsdl": "deviceio",
		"http://www.onvif.org/ver10/search/wsdl":   "search",
		"http://example.com/ver10/media/wsdl":      "",
		"http://www.onvif.org/ver10/schema":        "",
	} {
		if got := onvifServiceName(ns); got != want {
			t.Fatalf("onvifServiceName(%q) = %q, want %q", ns, got, want)
		}
	}
}
