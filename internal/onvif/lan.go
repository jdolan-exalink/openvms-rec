package onvif

// LAN discovery for the Recorder: WS-Discovery multicast on every active IPv4
// interface at once and, optionally, a unicast probe to each host of the
// interface's subnet for networks where multicast does not get through
// (VLANs, managed switches, some Windows firewall setups). The probe messages
// and the ProbeMatches parser are shared with Discovery (discovery.go).

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// LANDevice is one device that answered a probe. All fields come from the
// network and are untrusted; nothing here has been contacted over HTTP.
type LANDevice struct {
	Address           string   `json:"address"`
	XAddrs            []string `json:"xaddrs"`
	Name              string   `json:"name,omitempty"`
	Hardware          string   `json:"hardware,omitempty"`
	Manufacturer      string   `json:"manufacturer,omitempty"`
	Types             []string `json:"types,omitempty"`
	EndpointReference string   `json:"endpoint_reference,omitempty"`
	Interface         string   `json:"interface"`
	Via               string   `json:"via"` // multicast or sweep
}

// LANOptions bounds a discovery run.
type LANOptions struct {
	Timeout time.Duration // listening time after the probes are sent; default 3 s
	Sweep   bool          // also probe every host of each subnet by unicast
	// MaxSweepHosts skips the sweep of larger subnets (default 1024, a /22).
	MaxSweepHosts int
}

// LANInterface describes an interface that was probed.
type LANInterface struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Prefix  string `json:"prefix"`
	Swept   bool   `json:"swept"`
	Error   string `json:"error,omitempty"`
}

const discoveryPort = 3702

// DiscoverLAN probes all active IPv4 interfaces and returns the devices that
// answered, sorted by address.
func DiscoverLAN(ctx context.Context, opts LANOptions) ([]LANDevice, []LANInterface, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.MaxSweepHosts <= 0 {
		opts.MaxSweepHosts = 1024
	}
	type nic struct {
		iface  net.Interface
		ip     netip.Addr
		prefix netip.Prefix
	}
	var nics []nic
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
				continue
			}
			ip, _ := netip.AddrFromSlice(ip4)
			ones, _ := ipn.Mask.Size()
			nics = append(nics, nic{iface: iface, ip: ip, prefix: netip.PrefixFrom(ip, ones).Masked()})
		}
	}
	if len(nics) == 0 {
		return nil, nil, errors.New("no active IPv4 network interface")
	}

	ids := make([]string, 2)
	for i := range ids {
		if ids[i], err = newMessageID(); err != nil {
			return nil, nil, err
		}
	}
	typed, wildcard := buildProbe(ids[0]), buildWildcardProbe(ids[1])

	var (
		mu     sync.Mutex
		byKey  = map[string]*LANDevice{}
		infos  = make([]LANInterface, len(nics))
		wg     sync.WaitGroup
		runCtx context.Context
		cancel context.CancelFunc
		multi  = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: discoveryPort}
	)
	runCtx, cancel = context.WithCancel(ctx)
	defer cancel()

	for i, n := range nics {
		infos[i] = LANInterface{Name: n.iface.Name, Address: n.ip.String(), Prefix: n.prefix.String()}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: n.ip.AsSlice()})
		if err != nil {
			infos[i].Error = "cannot open a UDP socket on this interface"
			continue
		}
		pc := ipv4.NewPacketConn(conn)
		if n.iface.Flags&net.FlagMulticast != 0 {
			if pc.SetMulticastInterface(&n.iface) == nil {
				_ = pc.SetMulticastTTL(4)
				_, _ = conn.WriteTo(typed, multi)
				_, _ = conn.WriteTo(wildcard, multi)
			}
		}
		swept := opts.Sweep && n.prefix.Bits() >= 8 && hostCount(n.prefix) <= opts.MaxSweepHosts
		infos[i].Swept = swept
		via := func(src netip.Addr) string {
			if swept && n.prefix.Contains(src) {
				return "sweep"
			}
			return "multicast"
		}
		wg.Add(1)
		go func(n nic, conn *net.UDPConn) {
			defer wg.Done()
			defer conn.Close()
			deadline := time.Now().Add(opts.Timeout)
			if swept {
				// Leave time for answers to the last packets of the sweep.
				deadline = deadline.Add(time.Duration(hostCount(n.prefix)) * 200 * time.Microsecond)
				go sweep(runCtx, conn, n.prefix, n.ip, typed)
			}
			if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
				deadline = d
			}
			_ = conn.SetReadDeadline(deadline)
			stop := context.AfterFunc(runCtx, func() { _ = conn.SetReadDeadline(time.Now()) })
			defer stop()
			buf := make([]byte, maxDiscoveryPacket+1)
			for {
				nr, src, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if nr == 0 || nr > maxDiscoveryPacket || src == nil {
					continue
				}
				matches, ok := parseProbeMatches(buf[:nr], ids...)
				if !ok {
					continue
				}
				srcAddr, _ := netip.AddrFromSlice(src.IP.To4())
				mu.Lock()
				for _, m := range matches {
					mergeMatch(byKey, m, srcAddr, n.iface.Name, via(srcAddr))
				}
				mu.Unlock()
			}
		}(n, conn)
	}
	wg.Wait()

	out := make([]LANDevice, 0, len(byKey))
	seen := map[*LANDevice]bool{}
	for _, d := range byKey {
		if !seen[d] {
			seen[d] = true
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		x, _ := netip.ParseAddr(out[a].Address)
		y, _ := netip.ParseAddr(out[b].Address)
		return x.Less(y)
	})
	return out, infos, ctx.Err()
}

func hostCount(p netip.Prefix) int {
	bits := 32 - p.Bits()
	if bits >= 31 {
		return 1 << 30
	}
	return 1 << bits
}

// sweep sends a unicast probe to every host of prefix except self, paced so a
// /22 takes about 200 ms and does not flood cheap switches or cameras.
func sweep(ctx context.Context, conn *net.UDPConn, prefix netip.Prefix, self netip.Addr, payload []byte) {
	n := 0
	for a := prefix.Addr().Next(); prefix.Contains(a); a = a.Next() {
		if ctx.Err() != nil {
			return
		}
		if a == self || !prefix.Contains(a.Next()) { // skip self and the broadcast address
			continue
		}
		_, _ = conn.WriteToUDP(payload, &net.UDPAddr{IP: a.AsSlice(), Port: discoveryPort})
		n++
		if n%32 == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func mergeMatch(byKey map[string]*LANDevice, m parsedProbeMatch, src netip.Addr, iface, via string) {
	key := m.EndpointReference
	if key == "" {
		key = "ip:" + src.String()
	}
	d, ok := byKey[key]
	if !ok {
		d = byKey["ip:"+src.String()]
	}
	if d == nil {
		d = &LANDevice{Address: src.String(), EndpointReference: m.EndpointReference, Interface: iface, Via: via, XAddrs: []string{}}
		applyScopes(d, m.ScopeHints)
		d.Types = m.Types
	} else if d.Via == "sweep" && via == "multicast" {
		d.Via = via
	}
	byKey[key] = d
	byKey["ip:"+src.String()] = d
	for _, x := range m.XAddrs {
		s := x.String()
		dup := false
		for _, have := range d.XAddrs {
			if have == s {
				dup = true
				break
			}
		}
		if !dup {
			d.XAddrs = append(d.XAddrs, s)
		}
	}
}

func applyScopes(d *LANDevice, scopes []string) {
	for _, s := range scopes {
		const pfx = "onvif://www.onvif.org/"
		if !strings.HasPrefix(strings.ToLower(s), pfx) {
			continue
		}
		rest := s[len(pfx):]
		k, v, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		if uv, err := url.PathUnescape(v); err == nil {
			v = uv
		}
		v = clean(v)
		switch strings.ToLower(k) {
		case "name":
			if d.Name == "" {
				d.Name = v
			}
		case "hardware":
			if d.Hardware == "" {
				d.Hardware = v
			}
		case "mfrname", "manufacturer", "mfr", "brand":
			if d.Manufacturer == "" {
				d.Manufacturer = v
			}
		}
	}
}
