package onvif

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestConnDeviceReadsStreamsAndOptions(t *testing.T) {
	d := newFakeMedia2Device(t)
	dev := ConnDevice{Conn: d.conn(t, nil)}
	streams, err := dev.Streams(context.Background())
	if err != nil || len(streams) == 0 {
		t.Fatalf("streams = %v, %v", streams, err)
	}
	opts, err := dev.Options(context.Background(), streams[0].Token)
	if err != nil || len(opts.Codecs) == 0 {
		t.Fatalf("options = %v, %v", opts, err)
	}
}

func TestDiscoverConnKeepsOnlySameHostServices(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><s:Body><tds:GetServicesResponse>`+
			`<tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>http://10.0.0.5/onvif/media</tds:XAddr></tds:Service>`+
			`<tds:Service><tds:Namespace>http://www.onvif.org/ver20/media/wsdl</tds:Namespace><tds:XAddr>http://203.0.113.9/onvif/media2</tds:XAddr></tds:Service>`+
			`</tds:GetServicesResponse></s:Body></s:Envelope>`), nil
	})
	conn, err := DiscoverConn(context.Background(), NewClient(transport, Config{Timeout: time.Second}), "http://10.0.0.5/onvif/device_service")
	if err != nil {
		t.Fatal(err)
	}
	if conn.Media.String() != "http://10.0.0.5/onvif/media" || conn.Media2.String() != "" {
		t.Fatalf("a service on another host was accepted: %+v", conn)
	}
}

// A camera (observed: Provision-ISR) advertises its media services with an explicit default port
// `:80` while its device endpoint omits it. That is the same authority and must be accepted; the
// host check still has to reject a genuinely different host.
func TestDiscoverConnAcceptsExplicitDefaultPortOnSameHost(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><s:Body><tds:GetServicesResponse>`+
			`<tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>http://10.0.0.5:80/onvif/media</tds:XAddr></tds:Service>`+
			`<tds:Service><tds:Namespace>http://www.onvif.org/ver20/media/wsdl</tds:Namespace><tds:XAddr>http://10.0.0.5:80/onvif/media2</tds:XAddr></tds:Service>`+
			`</tds:GetServicesResponse></s:Body></s:Envelope>`), nil
	})
	conn, err := DiscoverConn(context.Background(), NewClient(transport, Config{Timeout: time.Second}), "http://10.0.0.5/onvif/device_service")
	if err != nil {
		t.Fatalf("the explicit default port must match the device host: %v", err)
	}
	if conn.Media.String() != "http://10.0.0.5:80/onvif/media" || conn.Media2.String() != "http://10.0.0.5:80/onvif/media2" {
		t.Fatalf("same-host default-port services not accepted: %+v", conn)
	}
}

func TestPrivateAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.20": true, "10.1.2.3": true, "172.16.0.9": true, "100.64.0.1": true,
		"127.0.0.1": false, "169.254.1.1": false, "8.8.8.8": false, "0.0.0.0": false,
	} {
		if got := PrivateAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PrivateAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestPrivateNetworkTransportRefusesLoopback(t *testing.T) {
	d := newFakeMedia2Device(t) // listens on 127.0.0.1
	client := &http.Client{Transport: PrivateNetworkTransport(), Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, d.srv.URL, nil)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not a private address") {
		t.Fatalf("loopback was reachable: %v", err)
	}
}
