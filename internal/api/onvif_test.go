package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

func TestONVIFDiscoverAndProbe(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg, err := cameras.Open(context.Background(), cameras.Options{DB: d, CredsFile: filepath.Join(dir, "c.yaml"),
		Static: []config.Camera{{ID: "patio", MainURL: "rtsp://10.1.1.225:554/profile1"}}})
	if err != nil {
		t.Fatal(err)
	}
	var gotEP, gotUser, gotPass string
	s := &Server{Cfg: config.Default(), DB: d, Prober: fakeProber{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Cameras: reg,
		Discover: func(_ context.Context, o onvif.LANOptions) ([]onvif.LANDevice, []onvif.LANInterface, error) {
			if !o.Sweep {
				t.Error("sweep not passed")
			}
			return []onvif.LANDevice{
				{Address: "10.1.1.225", XAddrs: []string{"http://10.1.1.225/onvif/device_service"}, Via: "multicast"},
				{Address: "10.1.1.80", XAddrs: []string{"http://10.1.1.80/onvif/device_service"}, Via: "sweep"},
			}, []onvif.LANInterface{{Name: "eth0", Address: "10.1.1.10", Prefix: "10.1.1.0/24", Swept: true}}, nil
		},
		ONVIFProbe: func(_ context.Context, ep onvif.Endpoint, u, p string) (onvif.ProbeResult, error) {
			gotEP, gotUser, gotPass = ep.String(), u, p
			if p != "pw" {
				return onvif.ProbeResult{}, onvif.ErrProbeAuth
			}
			return onvif.ProbeResult{Manufacturer: "Provision-ISR", Model: "I4", MainURL: "rtsp://10.1.1.80:554/profile1", SubURL: "rtsp://10.1.1.80:554/profile2",
				Capabilities: []onvif.ProbeCapability{{Capability: "onvif.media.stream_uri", Status: "confirmed"}}}, nil
		}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	var disc struct {
		Devices []DiscoveredDTO `json:"devices"`
	}
	if c := send(t, "POST", ts.URL+"/api/v1/cameras/discover", `{"sweep":true}`, &disc); c != 200 || len(disc.Devices) != 2 {
		t.Fatalf("discover %d %+v", c, disc)
	}
	if disc.Devices[0].CameraID != "patio" || !disc.Devices[0].Allowed || disc.Devices[1].CameraID != "" {
		t.Fatalf("mapping %+v", disc.Devices)
	}

	var pr ProbeResponse
	if c := send(t, "POST", ts.URL+"/api/v1/cameras/probe", `{"xaddr":"http://10.1.1.80/onvif/device_service","username":"admin","password":"pw"}`, &pr); c != 200 {
		t.Fatalf("probe %d", c)
	}
	if gotEP != "http://10.1.1.80/onvif/device_service" || gotUser != "admin" || gotPass != "pw" {
		t.Fatalf("probe args %q %q", gotEP, gotUser)
	}
	if pr.Suggested.MainURL != "rtsp://10.1.1.80:554/profile1" || !strings.HasPrefix(pr.Suggested.Name, "Provision-ISR I4") || pr.Capabilities[0].CheckedAt == "" {
		t.Fatalf("probe result %+v", pr)
	}

	if c := send(t, "POST", ts.URL+"/api/v1/cameras/probe", `{"host":"10.1.1.80","onvif_port":8080,"username":"admin","password":"bad"}`, nil); c != 422 {
		t.Fatalf("auth failure %d", c)
	}
	if gotEP != "http://10.1.1.80:8080/onvif/device_service" {
		t.Fatalf("host+port endpoint %q", gotEP)
	}
	for _, body := range []string{`{"host":"8.8.8.8"}`, `{"xaddr":"http://1.1.1.1/onvif/device_service"}`, `{"xaddr":"file:///etc/passwd"}`, `{}`} {
		if c := send(t, "POST", ts.URL+"/api/v1/cameras/probe", body, nil); c != 422 {
			t.Fatalf("%s: %d", body, c)
		}
	}
}
