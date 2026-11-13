package onvif

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ConnDevice exposes the video building blocks of one Conn as methods, so callers that need a
// seam (tests, lazy connections) can depend on an interface instead of the functions.
type ConnDevice struct{ Conn Conn }

// Streams returns the current video settings of every media profile.
func (d ConnDevice) Streams(ctx context.Context) ([]ProfileStream, error) {
	return ProfileStreams(ctx, d.Conn)
}

// Options asks the device which video values the profile supports.
func (d ConnDevice) Options(ctx context.Context, profileToken string) (VideoOptions, error) {
	return VideoEncoderOptions(ctx, d.Conn, profileToken)
}

// Set changes the requested fields of a profile (read-modify-write with verification).
func (d ConnDevice) Set(ctx context.Context, profileToken string, desired VideoDesired) (ProfileStream, error) {
	return SetVideoEncoder(ctx, d.Conn, profileToken, desired)
}

const (
	nsMedia1 = "/ver10/media/"
	nsMedia2 = "/ver20/media/"
)

// DiscoverConn finds the Media and Media2 services of the device behind deviceEndpoint and
// builds a Conn. The service addresses come from the device, so only those on the same host as
// the endpoint the caller already trusted are accepted: a device cannot point OpenVMS elsewhere.
func DiscoverConn(ctx context.Context, client *Client, deviceEndpoint string) (Conn, error) {
	dev, err := ParseEndpoint(deviceEndpoint)
	if err != nil {
		return Conn{}, err
	}
	devices := NewDeviceClient(client, dev)
	services, err := devices.GetServices(ctx)
	if err != nil || len(services) == 0 {
		if services, err = devices.GetCapabilities(ctx); err != nil {
			return Conn{}, err
		}
	}
	host := hostOf(dev.String())
	var media1, media2 string
	for _, s := range services {
		ep, err := s.ValidatedEndpoint()
		if err != nil || hostOf(ep.String()) != host {
			continue
		}
		switch {
		case strings.Contains(s.Namespace, nsMedia2) && media2 == "":
			media2 = ep.String()
		case strings.Contains(s.Namespace, nsMedia1) && media1 == "":
			media1 = ep.String()
		}
	}
	if media1 == "" && media2 == "" {
		return Conn{}, fmt.Errorf("device offers no media service: %w", ErrInvalidEndpoint)
	}
	return NewConn(client, media1, media2)
}

func hostOf(raw string) string {
	ep, err := ParseEndpoint(raw)
	if err != nil {
		return ""
	}
	u, err := url.Parse(ep.String())
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	// An explicit scheme-default port (`http://host:80`, `https://host:443`) is the same authority
	// as an endpoint that omits it: cameras (observed: Provision-ISR) advertise their services as
	// `:80` while the device endpoint leaves the port off, and rejecting that broke DiscoverConn.
	// Any other port is kept, so a service on a different host or port is still refused and the
	// check remains a real host guard.
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return host + ":" + port
	}
	return host
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PrivateAddr reports whether a is a LAN or VPN address a camera may live at: RFC 1918, unique
// local or carrier-grade NAT. Loopback, link-local, multicast, unspecified and public addresses
// are not.
func PrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && (a.IsPrivate() || cgnat.Contains(a)) && !a.IsLoopback() && !a.IsLinkLocalUnicast()
}

// PrivateNetworkTransport is an HTTP transport that only connects to private addresses (see
// PrivateAddr), checked on the address actually dialed, so neither a hostname nor a redirect
// nor a device-supplied URL can reach a public or local service. It never uses a proxy.
func PrivateNetworkTransport() http.RoundTripper {
	d := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !PrivateAddr(ap.Addr()) {
				return fmt.Errorf("onvif: destination is not a private address")
			}
			return nil
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}
