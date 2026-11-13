package onvif

import (
	"net/netip"
	"testing"
)

func TestPickStreams(t *testing.T) {
	ps := []ProbeProfile{
		{Token: "sub", Width: 640, Height: 360, StreamURI: "rtsp://10.0.0.5/sub"},
		{Token: "main", Width: 2560, Height: 1440, StreamURI: "rtsp://10.0.0.5/main"},
		{Token: "third", Width: 352, Height: 288, StreamURI: "rtsp://10.0.0.5/third"},
		{Token: "nouri", Width: 3840, Height: 2160},
	}
	m, s := pickStreams(ps)
	if m != "rtsp://10.0.0.5/main" || s != "rtsp://10.0.0.5/sub" {
		t.Fatalf("got %q %q", m, s)
	}
	m, s = pickStreams(ps[1:2])
	if m != "rtsp://10.0.0.5/main" || s != "" {
		t.Fatalf("single: %q %q", m, s)
	}
}

func TestSanitizeStreamURI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rtsp://admin:secret@10.1.1.225:554/profile1", "rtsp://10.1.1.225:554/profile1"},
		{"rtsp://0.0.0.0:554/ch1", "rtsp://10.1.1.225:554/ch1"},
		{"rtsp://10.1.1.9:554/chID=1&streamType=main&linkType=tcp", "rtsp://10.1.1.9:554/chID=1&streamType=main&linkType=tcp"},
		{"rtsp://10.1.1.9/live?channel=1&user=admin&password=secret", "rtsp://10.1.1.9/live?channel=1"},
		{"rtsp://10.1.1.9/secret/live", ""},
		{"http://10.1.1.9/live", ""},
	}
	for _, c := range cases {
		if got := sanitizeStreamURI(c.in, "10.1.1.225", "secret"); got != c.want {
			t.Errorf("%s: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestMergeMatchScopes(t *testing.T) {
	ep, _ := ParseEndpoint("http://10.1.1.225/onvif/device_service")
	m := parsedProbeMatch{EndpointReference: "urn:uuid:1", XAddrs: []Endpoint{ep},
		ScopeHints: []string{"onvif://www.onvif.org/name/IPC%20Patio", "onvif://www.onvif.org/hardware/I4-320IPSN", "onvif://www.onvif.org/MfrName/Provision"}}
	by := map[string]*LANDevice{}
	src := netip.MustParseAddr("10.1.1.225")
	mergeMatch(by, m, src, "eth0", "sweep")
	mergeMatch(by, m, src, "eth0", "multicast")
	d := by["urn:uuid:1"]
	if d == nil || d.Name != "IPC Patio" || d.Hardware != "I4-320IPSN" || d.Manufacturer != "Provision" || len(d.XAddrs) != 1 || d.Via != "multicast" {
		t.Fatalf("%+v", d)
	}
}

func TestHostCount(t *testing.T) {
	if hostCount(netip.MustParsePrefix("10.1.1.0/24")) != 256 || hostCount(netip.MustParsePrefix("10.0.0.0/8")) <= 1024 {
		t.Fatal("hostCount")
	}
}
