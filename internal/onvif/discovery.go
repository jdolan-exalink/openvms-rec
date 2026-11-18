package onvif

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	discoveryNamespace     = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	addressingNamespace    = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	networkNamespace       = "http://www.onvif.org/ver10/network/wsdl"
	discoveryProbeAction   = discoveryNamespace + "/Probe"
	discoveryMatchesAction = discoveryNamespace + "/ProbeMatches"
	discoveryTo            = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	discoveryAnonymous     = "http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous"
	maxDiscoveryPacket     = 64 << 10
)

// DiscoveredDevice is a camera or NVR found via WS-Discovery multicast.
// Scope hints (Name, Hardware) are unauthenticated hints advertised by the
// device; capabilities are only confirmed after an authenticated Probe.
type DiscoveredDevice struct {
	EndpointReference string   `json:"endpoint_reference"`
	Host              string   `json:"host"`
	ONVIFPort         int      `json:"onvif_port"`
	XAddrs            []string `json:"xaddrs"`
	Name              string   `json:"name,omitempty"`
	Hardware          string   `json:"hardware,omitempty"`
	Scopes            []string `json:"scopes,omitempty"`
}

// Discover broadcasts WS-Discovery Probe messages on active IPv4 multicast
// interfaces and collects responses until timeout.
func Discover(ctx context.Context, timeout time.Duration) ([]DiscoveredDevice, error) {
	if timeout <= 0 {
		timeout = 2500 * time.Millisecond
	}
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	ips := multicastIPv4s()
	if len(ips) == 0 {
		ips = []net.IP{net.IPv4zero}
	}

	msgID1, err := newUUIDURN()
	if err != nil {
		return nil, err
	}
	msgID2, err := newUUIDURN()
	if err != nil {
		return nil, err
	}
	probe1 := buildProbePacket(msgID1, true)
	probe2 := buildProbePacket(msgID2, false)
	dest := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}

	type rawPacket struct {
		data []byte
		ip   string
	}
	ch := make(chan rawPacket, 64)
	deadline := time.Now().Add(timeout)
	var conns []*net.UDPConn
	for _, ip := range ips {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
		if err != nil {
			continue
		}
		_ = conn.SetDeadline(deadline)
		_, _ = conn.WriteToUDP(probe1, dest)
		_, _ = conn.WriteToUDP(probe2, dest)
		conns = append(conns, conn)
		go func(c *net.UDPConn) {
			buf := make([]byte, maxDiscoveryPacket)
			for {
				n, addr, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if n > 0 {
					pkt := make([]byte, n)
					copy(pkt, buf[:n])
					peer := ""
					if addr != nil && addr.IP != nil {
						peer = addr.IP.String()
					}
					select {
					case ch <- rawPacket{data: pkt, ip: peer}:
					default:
					}
				}
			}
		}(conn)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var out []DiscoveredDevice
	byKey := map[string]int{}
	seenXAddr := map[string]bool{}

	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-timer.C:
			return out, nil
		case pkt := <-ch:
			matches := ParseProbeMatches(pkt.data, msgID1, msgID2)
			for _, m := range matches {
				host, port := extractHostPort(m.XAddrs, pkt.ip)
				if host == "" {
					continue
				}
				key := m.EndpointReference
				if key == "" {
					key = host
				}
				idx, ok := byKey[key]
				if !ok {
					idx = len(out)
					byKey[key] = idx
					out = append(out, DiscoveredDevice{
						EndpointReference: m.EndpointReference,
						Host:              host,
						ONVIFPort:         port,
						Name:              m.Name,
						Hardware:          m.Hardware,
						Scopes:            m.Scopes,
					})
				}
				for _, xa := range m.XAddrs {
					xk := key + "|" + xa
					if !seenXAddr[xk] {
						seenXAddr[xk] = true
						out[idx].XAddrs = append(out[idx].XAddrs, xa)
					}
				}
			}
		}
	}
}

func multicastIPv4s() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsUnspecified() && !ip4.IsLoopback() && !ip4.IsMulticast() {
				out = append(out, ip4)
				break
			}
		}
	}
	return out
}

func extractHostPort(xaddrs []string, fallbackIP string) (string, int) {
	for _, raw := range xaddrs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		p := 80
		if u.Scheme == "https" {
			p = 443
		}
		if u.Port() != "" {
			if n, err := strconv.Atoi(u.Port()); err == nil && n > 0 && n <= 65535 {
				p = n
			}
		}
		return u.Hostname(), p
	}
	if fallbackIP != "" {
		return fallbackIP, 80
	}
	return "", 80
}

// ParseProbeMatches parses a WS-Discovery ProbeMatches UDP payload.
func ParseProbeMatches(packet []byte, messageIDs ...string) []DiscoveredDevice {
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Header  struct {
			Action    string `xml:"Action"`
			RelatesTo string `xml:"RelatesTo"`
		} `xml:"Header"`
		Body struct {
			Matches struct {
				Items []struct {
					EPR struct {
						Address string `xml:"Address"`
					} `xml:"EndpointReference"`
					Scopes string `xml:"Scopes"`
					XAddrs string `xml:"XAddrs"`
				} `xml:"ProbeMatch"`
			} `xml:"ProbeMatches"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(packet, &env); err != nil {
		return nil
	}
	if !strings.HasSuffix(strings.TrimSpace(env.Header.Action), "/ProbeMatches") {
		return nil
	}
	rel := strings.TrimSpace(env.Header.RelatesTo)
	if len(messageIDs) > 0 && rel != "" {
		matched := false
		for _, id := range messageIDs {
			if rel == id {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
	}
	var out []DiscoveredDevice
	for _, it := range env.Body.Matches.Items {
		var xaddrs []string
		for _, raw := range strings.Fields(it.XAddrs) {
			if ep, err := ParseEndpoint(raw); err == nil {
				xaddrs = append(xaddrs, ep.String())
			}
		}
		scopes := strings.Fields(it.Scopes)
		name, hw := parseScopeHints(scopes)
		host, port := extractHostPort(xaddrs, "")
		out = append(out, DiscoveredDevice{
			EndpointReference: strings.TrimSpace(it.EPR.Address),
			Host:              host,
			ONVIFPort:         port,
			XAddrs:            xaddrs,
			Name:              name,
			Hardware:          hw,
			Scopes:            scopes,
		})
	}
	return out
}

func parseScopeHints(scopes []string) (name, hardware string) {
	for _, s := range scopes {
		dec, err := url.PathUnescape(s)
		if err != nil {
			dec = s
		}
		lower := strings.ToLower(dec)
		switch {
		case strings.HasPrefix(lower, "onvif://www.onvif.org/name/"):
			if name == "" {
				name = strings.TrimSpace(dec[len("onvif://www.onvif.org/name/"):])
			}
		case strings.HasPrefix(lower, "onvif://www.onvif.org/hardware/"):
			if hardware == "" {
				hardware = strings.TrimSpace(dec[len("onvif://www.onvif.org/hardware/"):])
			}
		}
	}
	return name, hardware
}

func newUUIDURN() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return "urn:uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func buildProbePacket(messageID string, typed bool) []byte {
	if typed {
		return []byte(`<s:Envelope xmlns:s="` + soapNamespace + `" xmlns:wsadis="` + addressingNamespace + `" xmlns:d="` + discoveryNamespace + `" xmlns:tds="` + deviceNamespace + `" xmlns:dn="` + networkNamespace + `"><s:Header><wsadis:Action>` + discoveryProbeAction + `</wsadis:Action><wsadis:MessageID>` + messageID + `</wsadis:MessageID><wsadis:To>` + discoveryTo + `</wsadis:To><wsadis:ReplyTo><wsadis:Address>` + discoveryAnonymous + `</wsadis:Address></wsadis:ReplyTo></s:Header><s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types></d:Probe></s:Body></s:Envelope>`)
	}
	return []byte(`<s:Envelope xmlns:s="` + soapNamespace + `" xmlns:wsadis="` + addressingNamespace + `" xmlns:d="` + discoveryNamespace + `"><s:Header><wsadis:Action>` + discoveryProbeAction + `</wsadis:Action><wsadis:MessageID>` + messageID + `</wsadis:Action><wsadis:To>` + discoveryTo + `</wsadis:To><wsadis:ReplyTo><wsadis:Address>` + discoveryAnonymous + `</wsadis:Address></wsadis:ReplyTo></s:Header><s:Body><d:Probe/></s:Body></s:Envelope>`)
}

// IsTimeout reports whether err is a network/deadline timeout.
func IsTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
