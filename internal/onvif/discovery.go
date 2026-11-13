package onvif

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const (
	discoveryNamespace     = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	addressingNamespace    = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	discoveryProbeAction   = discoveryNamespace + "/Probe"
	discoveryMatchesAction = discoveryNamespace + "/ProbeMatches"
	discoveryTo            = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	discoveryAnonymous     = "http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous"
	networkNamespace       = "http://www.onvif.org/ver10/network/wsdl"
	maxDiscoveryPacket     = 64 << 10
)

var ErrDiscoveryTimeout = errors.New("ONVIF discovery timed out")

// DatagramSession owns one interface-bound socket for a single probe.
type DatagramSession interface {
	Send(ctx context.Context, payload []byte) error
	Receive(ctx context.Context) ([]byte, error)
	Close() error
}

// DatagramTransport opens an isolated session; sessions never share sockets.
type DatagramTransport interface {
	Open(ctx context.Context, interfaceName string) (DatagramSession, error)
}

type DiscoveryConfig struct {
	Timeout    time.Duration
	MaxResults int
}

type Discovery struct {
	transport DatagramTransport
	config    DiscoveryConfig
}

func NewDiscovery(transport DatagramTransport, config DiscoveryConfig) *Discovery {
	if config.Timeout <= 0 {
		config.Timeout = 3 * time.Second
	}
	if config.Timeout > 30*time.Second {
		config.Timeout = 30 * time.Second
	}
	if config.MaxResults <= 0 {
		config.MaxResults = 32
	}
	if config.MaxResults > 256 {
		config.MaxResults = 256
	}
	return &Discovery{transport: transport, config: config}
}

// DiscoveredDevice contains untrusted discovery metadata. ScopeHints are not
// manufacturer identity, and XAddrs are validated but never contacted here.
type DiscoveredDevice struct {
	EndpointReference string
	XAddrs            []Endpoint
	Types             []string
	ScopeHints        []string
}

func (d *Discovery) Probe(ctx context.Context, interfaceName string) ([]DiscoveredDevice, error) {
	if d == nil || d.transport == nil || strings.TrimSpace(interfaceName) == "" {
		return nil, fmt.Errorf("invalid ONVIF discovery configuration")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	messageID, err := newMessageID()
	if err != nil {
		return nil, fmt.Errorf("create discovery message identity: %w", err)
	}
	messageIDWildcard, err := newMessageID()
	if err != nil {
		return nil, fmt.Errorf("create discovery message identity: %w", err)
	}
	payload := buildProbe(messageID)
	payloadWildcard := buildWildcardProbe(messageIDWildcard)
	probeCtx, cancel := context.WithTimeout(ctx, d.config.Timeout)
	defer cancel()
	session, err := d.transport.Open(probeCtx, interfaceName)
	if err != nil {
		return nil, fmt.Errorf("open ONVIF discovery session: %w", err)
	}
	defer session.Close()
	if err := session.Send(probeCtx, payload); err != nil {
		return nil, fmt.Errorf("send ONVIF discovery probe: %w", err)
	}
	_ = session.Send(probeCtx, payloadWildcard)

	result := make([]DiscoveredDevice, 0)
	deviceIndex := make(map[string]int)
	seenAddresses := make(map[string]bool)
	for len(result) < d.config.MaxResults {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if probeCtx.Err() != nil {
			return result, nil
		}
		packet, err := session.Receive(probeCtx)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			var netErr net.Error
			if errors.Is(err, ErrDiscoveryTimeout) ||
				errors.Is(err, os.ErrDeadlineExceeded) ||
				errors.Is(err, context.DeadlineExceeded) ||
				(errors.As(err, &netErr) && netErr.Timeout()) {
				return result, nil
			}
			if errors.Is(err, context.Canceled) {
				return result, err
			}
			return result, fmt.Errorf("receive ONVIF discovery response: %w", err)
		}
		if len(packet) == 0 || len(packet) > maxDiscoveryPacket {
			continue
		}
		matches, ok := parseProbeMatches(packet, messageID, messageIDWildcard)
		if !ok {
			continue
		}
		for _, match := range matches {
			if match.EndpointReference == "" {
				continue
			}
			i, exists := deviceIndex[match.EndpointReference]
			if !exists {
				if len(result) >= d.config.MaxResults {
					break
				}
				i = len(result)
				deviceIndex[match.EndpointReference] = i
				result = append(result, DiscoveredDevice{EndpointReference: match.EndpointReference, Types: match.Types, ScopeHints: match.ScopeHints})
			}
			for _, address := range match.XAddrs {
				key := match.EndpointReference + "\x00" + address.value
				if !seenAddresses[key] {
					seenAddresses[key] = true
					result[i].XAddrs = append(result[i].XAddrs, address)
				}
			}
		}
	}
	return result, nil
}

func newMessageID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return "urn:uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func buildProbe(messageID string) []byte {
	return []byte(`<s:Envelope xmlns:s="` + soapNamespace + `" xmlns:wsadis="` + addressingNamespace + `" xmlns:d="` + discoveryNamespace + `" xmlns:tds="` + deviceNamespace + `" xmlns:dn="` + networkNamespace + `"><s:Header><wsadis:Action>` + discoveryProbeAction + `</wsadis:Action><wsadis:MessageID>` + messageID + `</wsadis:MessageID><wsadis:To>` + discoveryTo + `</wsadis:To><wsadis:ReplyTo><wsadis:Address>` + discoveryAnonymous + `</wsadis:Address></wsadis:ReplyTo></s:Header><s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types></d:Probe></s:Body></s:Envelope>`)
}

func buildWildcardProbe(messageID string) []byte {
	return []byte(`<s:Envelope xmlns:s="` + soapNamespace + `" xmlns:wsadis="` + addressingNamespace + `" xmlns:d="` + discoveryNamespace + `"><s:Header><wsadis:Action>` + discoveryProbeAction + `</wsadis:Action><wsadis:MessageID>` + messageID + `</wsadis:MessageID><wsadis:To>` + discoveryTo + `</wsadis:To><wsadis:ReplyTo><wsadis:Address>` + discoveryAnonymous + `</wsadis:Address></wsadis:ReplyTo></s:Header><s:Body><d:Probe/></s:Body></s:Envelope>`)
}

type probeEnvelope struct {
	XMLName xml.Name `xml:"http://www.w3.org/2003/05/soap-envelope Envelope"`
	Header  struct {
		Action    string `xml:"http://schemas.xmlsoap.org/ws/2004/08/addressing Action"`
		RelatesTo string `xml:"http://schemas.xmlsoap.org/ws/2004/08/addressing RelatesTo"`
	} `xml:"http://www.w3.org/2003/05/soap-envelope Header"`
	Body struct {
		Matches struct {
			Items []probeMatchEntry `xml:"http://schemas.xmlsoap.org/ws/2005/04/discovery ProbeMatch"`
		} `xml:"http://schemas.xmlsoap.org/ws/2005/04/discovery ProbeMatches"`
	} `xml:"http://www.w3.org/2003/05/soap-envelope Body"`
}
type probeMatchEntry struct {
	EPR struct {
		Address string `xml:"http://schemas.xmlsoap.org/ws/2004/08/addressing Address"`
	} `xml:"http://schemas.xmlsoap.org/ws/2004/08/addressing EndpointReference"`
	Types  string `xml:"http://schemas.xmlsoap.org/ws/2005/04/discovery Types"`
	Scopes string `xml:"http://schemas.xmlsoap.org/ws/2005/04/discovery Scopes"`
	XAddrs string `xml:"http://schemas.xmlsoap.org/ws/2005/04/discovery XAddrs"`
}

type parsedProbeMatch struct {
	EndpointReference string
	XAddrs            []Endpoint
	Types             []string
	ScopeHints        []string
}

func parseProbeMatches(packet []byte, messageIDs ...string) ([]parsedProbeMatch, bool) {
	var envelope probeEnvelope
	if err := xml.Unmarshal(packet, &envelope); err != nil || envelope.XMLName.Space != soapNamespace || envelope.Header.Action != discoveryMatchesAction {
		return nil, false
	}
	relatesTo := strings.TrimSpace(envelope.Header.RelatesTo)
	if len(messageIDs) > 0 {
		matched := false
		for _, mid := range messageIDs {
			if relatesTo == mid {
				matched = true
				break
			}
		}
		if !matched && relatesTo != "" {
			return nil, false
		}
	}
	result := make([]parsedProbeMatch, 0, len(envelope.Body.Matches.Items))
	for _, m := range envelope.Body.Matches.Items {
		parsed := parsedProbeMatch{EndpointReference: strings.TrimSpace(m.EPR.Address), Types: strings.Fields(m.Types), ScopeHints: strings.Fields(m.Scopes)}
		for _, raw := range strings.Fields(m.XAddrs) {
			endpoint, err := ParseEndpoint(raw)
			if err == nil {
				parsed.XAddrs = append(parsed.XAddrs, endpoint)
			}
		}
		result = append(result, parsed)
	}
	return result, true
}
