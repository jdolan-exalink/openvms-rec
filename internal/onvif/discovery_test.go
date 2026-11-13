package onvif

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeDatagrams struct {
	sent    [][]byte
	packets [][]byte
	reads   int
	onSend  func([]byte)
	receive func(context.Context) ([]byte, error)
}

type fakeDatagramSession struct{ transport *fakeDatagrams }

func (f *fakeDatagrams) Open(_ context.Context, _ string) (DatagramSession, error) {
	return fakeDatagramSession{f}, nil
}
func (s fakeDatagramSession) Send(ctx context.Context, p []byte) error {
	return s.transport.Send(ctx, "", p)
}
func (s fakeDatagramSession) Receive(ctx context.Context) ([]byte, error) {
	return s.transport.Receive(ctx, "")
}
func (s fakeDatagramSession) Close() error { return nil }

func (f *fakeDatagrams) Send(_ context.Context, _ string, p []byte) error {
	f.sent = append(f.sent, append([]byte(nil), p...))
	if f.onSend != nil {
		f.onSend(p)
	}
	return nil
}

func TestUDPTransportRequiresBoundedInterfaceSession(t *testing.T) {
	tr := NewUDPTransport()
	if _, err := tr.Open(context.Background(), ""); err == nil {
		t.Fatal("empty interface accepted")
	}
}
func (f *fakeDatagrams) Receive(ctx context.Context, _ string) ([]byte, error) {
	if f.receive != nil {
		return f.receive(ctx)
	}
	if f.reads >= len(f.packets) {
		return nil, ErrDiscoveryTimeout
	}
	p := f.packets[f.reads]
	f.reads++
	return p, nil
}

func TestDiscoveryProbeParsesCorrelatedMatchesAndDeduplicates(t *testing.T) {
	transport := &fakeDatagrams{}
	d := NewDiscovery(transport, DiscoveryConfig{Timeout: time.Second, MaxResults: 4})
	ctx := context.Background()
	// The transport provides a response after observing the request MessageID.
	transport.onSend = func(p []byte) {
		id := xmlValue(t, p, "MessageID")
		transport.packets = [][]byte{probeMatch(id, "urn:uuid:camera-1", "http://camera.local/onvif/device_service", "onvif://www.onvif.org/type/video_encoder"), probeMatch(id, "urn:uuid:camera-1", "http://camera.local/onvif/device_service", "onvif://www.onvif.org/type/video_encoder")}
	}
	devices, err := d.Probe(ctx, "eth-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].EndpointReference != "urn:uuid:camera-1" || len(devices[0].XAddrs) != 1 {
		t.Fatalf("unexpected devices: %+v", devices)
	}
	if !strings.Contains(string(transport.sent[0]), "http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe") || !strings.Contains(string(transport.sent[0]), "tds:Device") {
		t.Fatalf("not an ONVIF Probe: %s", transport.sent[0])
	}
	if !strings.Contains(string(transport.sent[0]), `xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"`) || !strings.Contains(string(transport.sent[0]), `xmlns:wsadis="http://schemas.xmlsoap.org/ws/2004/08/addressing"`) || !strings.Contains(string(transport.sent[0]), `xmlns:tds="http://www.onvif.org/ver10/device/wsdl"`) || !strings.Contains(string(transport.sent[0]), `<wsadis:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsadis:To>`) || !strings.Contains(string(transport.sent[0]), `http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous`) {
		t.Fatalf("Probe has incorrect ONVIF protocol namespaces: %s", transport.sent[0])
	}
	if !strings.HasPrefix(xmlValue(t, transport.sent[0], "MessageID"), "urn:uuid:") {
		t.Fatal("MessageID is not a UUID URN")
	}
}

func TestDiscoveryParsesONVIFLegacyProbeMatches(t *testing.T) {
	tr := &fakeDatagrams{}
	tr.onSend = func(p []byte) { tr.packets = [][]byte{legacyProbeMatch(xmlValue(t, p, "MessageID"))} }
	got, err := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second}).Probe(context.Background(), "eth-test")
	if err != nil || len(got) != 1 || got[0].EndpointReference != "urn:uuid:legacy-camera" || len(got[0].XAddrs) != 1 {
		t.Fatalf("legacy ProbeMatches not parsed: devices=%+v err=%v", got, err)
	}
}

func TestDiscoveryCancellationAndBoundedInvalidPackets(t *testing.T) {
	t.Run("pre-canceled context does not send", func(t *testing.T) {
		tr := &fakeDatagrams{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second}).Probe(ctx, "eth-test")
		if !errors.Is(err, context.Canceled) || len(tr.sent) != 0 {
			t.Fatalf("err=%v sends=%d", err, len(tr.sent))
		}
	})
	t.Run("caller deadline is not swallowed", func(t *testing.T) {
		tr := &fakeDatagrams{receive: func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second}).Probe(ctx, "eth-test")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want caller deadline, got %v", err)
		}
	})
	t.Run("endless invalid packets stop at discovery deadline", func(t *testing.T) {
		tr := &fakeDatagrams{receive: func(context.Context) ([]byte, error) { return []byte("invalid"), nil }}
		started := time.Now()
		got, err := NewDiscovery(tr, DiscoveryConfig{Timeout: 20 * time.Millisecond}).Probe(context.Background(), "eth-test")
		if err != nil || len(got) != 0 || time.Since(started) > time.Second {
			t.Fatalf("unbounded invalid packet loop: devices=%v err=%v", got, err)
		}
	})
}

func TestDiscoveryRejectsUnrelatedOrUnsafeResponses(t *testing.T) {
	cases := []struct {
		name     string
		response func(string) []byte
	}{
		{"wrong action", func(id string) []byte {
			return stringsToBytes(strings.Replace(string(probeMatch(id, "urn:uuid:a", "http://cam/device", "")), "ProbeMatches", "Hello", 1))
		}},
		{"wrong relation", func(string) []byte { return probeMatch("urn:uuid:other", "urn:uuid:a", "http://cam/device", "") }},
		{"unsafe xaddr", func(id string) []byte { return probeMatch(id, "urn:uuid:a", "http://user:pass@cam/device", "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeDatagrams{}
			tr.onSend = func(p []byte) { tr.packets = [][]byte{tc.response(xmlValue(t, p, "MessageID"))} }
			d := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second, MaxResults: 3})
			got, err := d.Probe(context.Background(), "eth-test")
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "unsafe xaddr" {
				if len(got) != 1 || len(got[0].XAddrs) != 0 {
					t.Fatalf("unsafe XAddr was not discarded: %+v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("accepted invalid response: %+v", got)
			}
		})
	}
}

func TestDiscoveryIgnoresOversizedDatagram(t *testing.T) {
	tr := &fakeDatagrams{}
	tr.onSend = func([]byte) { tr.packets = [][]byte{make([]byte, maxDiscoveryPacket+1)} }
	d := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second, MaxResults: 2})
	got, err := d.Probe(context.Background(), "eth-test")
	if err != nil || len(got) != 0 {
		t.Fatalf("oversized datagram was not bounded: devices=%v err=%v", got, err)
	}
}

func TestDiscoveryBoundsResultsAndTimeout(t *testing.T) {
	tr := &fakeDatagrams{}
	tr.onSend = func(p []byte) {
		id := xmlValue(t, p, "MessageID")
		tr.packets = [][]byte{probeMatch(id, "urn:uuid:a", "http://a/device", ""), probeMatch(id, "urn:uuid:b", "http://b/device", "")}
	}
	d := NewDiscovery(tr, DiscoveryConfig{Timeout: time.Second, MaxResults: 1})
	got, err := d.Probe(context.Background(), "eth-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected result bound, got %d", len(got))
	}
	tr = &fakeDatagrams{}
	d = NewDiscovery(tr, DiscoveryConfig{Timeout: time.Millisecond, MaxResults: 2})
	got, err = d.Probe(context.Background(), "eth-test")
	if err != nil || len(got) != 0 {
		t.Fatalf("want bounded empty discovery, got devices=%v err=%v", got, err)
	}
}

// Helpers intentionally produce SOAP only for deterministic parser tests.
func probeMatch(rel, epr, xaddr, scopes string) []byte {
	return stringsToBytes(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Header><a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</a:Action><a:RelatesTo>` + rel + `</a:RelatesTo></s:Header><s:Body><d:ProbeMatches><d:ProbeMatch><a:EndpointReference><a:Address>` + epr + `</a:Address></a:EndpointReference><d:Types>tds:Device</d:Types><d:Scopes>` + scopes + `</d:Scopes><d:XAddrs>` + xaddr + `</d:XAddrs></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`)
}
func legacyProbeMatch(rel string) []byte {
	return []byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><s:Header><wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</wsa:Action><wsa:RelatesTo>` + rel + `</wsa:RelatesTo></s:Header><s:Body><d:ProbeMatches><d:ProbeMatch><wsa:EndpointReference><wsa:Address>urn:uuid:legacy-camera</wsa:Address></wsa:EndpointReference><d:Types>tds:Device</d:Types><d:XAddrs>http://camera.local/onvif/device_service</d:XAddrs></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`)
}
func stringsToBytes(s string) []byte { return []byte(s) }
func xmlValue(t *testing.T, data []byte, local string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == local {
			var value string
			if err := dec.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("missing %s", local)
	return ""
}
