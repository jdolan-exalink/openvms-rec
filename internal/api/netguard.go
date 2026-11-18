package api

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// checkCameraHost enforces server.camera_networks when configured so probe and
// stream test endpoints cannot be abused as SSRF relays.
func checkCameraHost(ctx context.Context, networks []string, host string) error {
	if len(networks) == 0 {
		return nil
	}
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("empty host")
	}

	var prefixes []netip.Prefix
	for _, raw := range networks {
		p, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid configured network %q: %w", raw, err)
		}
		prefixes = append(prefixes, p)
	}

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip.Unmap()}
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(resolved) == 0 {
			return fmt.Errorf("cannot resolve host %q against camera_networks allowlist", host)
		}
		for _, r := range resolved {
			addrs = append(addrs, r.Unmap())
		}
	}

	for _, ip := range addrs {
		allowed := false
		for _, p := range prefixes {
			if p.Contains(ip) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("host %q (%s) is outside server.camera_networks allowlist", host, ip)
		}
	}
	return nil
}
