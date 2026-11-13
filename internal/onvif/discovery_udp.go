package onvif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

type udpInterfaceResolver func(string) (net.Interface, net.IP, error)
type udpPacketConn interface {
	SetMulticastInterface(*net.Interface) error
	SetMulticastTTL(int) error
	WriteTo([]byte, net.Addr) (int, error)
	ReadFrom([]byte) (int, net.Addr, error)
	SetDeadline(time.Time) error
	Close() error
}
type udpSocketFactory func(*net.UDPAddr) (udpPacketConn, error)

// UDPTransport opens one ephemeral, interface-bound IPv4 socket per discovery
// probe. Its injectable dependencies are package-private for deterministic tests.
type UDPTransport struct {
	resolve udpInterfaceResolver
	open    udpSocketFactory
}

func NewUDPTransport() *UDPTransport {
	return &UDPTransport{resolve: resolveDiscoveryInterface, open: openDiscoverySocket}
}

func (t *UDPTransport) Open(ctx context.Context, interfaceName string) (DatagramSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil || t.resolve == nil || t.open == nil || interfaceName == "" {
		return nil, errors.New("invalid ONVIF discovery transport")
	}
	iface, ip, err := t.resolve(interfaceName)
	if err != nil {
		return nil, err
	}
	isAuto := interfaceName == "*" || interfaceName == "auto"
	if (!isAuto && iface.Name != interfaceName) || iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return nil, fmt.Errorf("ONVIF interface %q must be named, up, and multicast-capable", interfaceName)
	}
	ip4 := ip.To4()
	if ip4 == nil || ip4.IsUnspecified() || ip4.IsMulticast() {
		return nil, fmt.Errorf("ONVIF interface %q has no usable assigned IPv4 address", interfaceName)
	}
	conn, err := t.open(&net.UDPAddr{IP: append(net.IP(nil), ip4...), Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind ONVIF discovery socket: %w", err)
	}
	if err = conn.SetMulticastInterface(&iface); err == nil {
		err = conn.SetMulticastTTL(4)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("configure ONVIF discovery route: %w", err)
	}
	return &udpDiscoverySession{conn: conn, destination: &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}}, nil
}

func resolveDiscoveryInterface(name string) (net.Interface, net.IP, error) {
	if name == "*" || name == "auto" {
		ifaces, err := net.Interfaces()
		if err != nil {
			return net.Interface{}, nil, fmt.Errorf("list network interfaces: %w", err)
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addresses, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, address := range addresses {
				var ip net.IP
				switch value := address.(type) {
				case *net.IPNet:
					ip = value.IP
				case *net.IPAddr:
					ip = value.IP
				}
				if ip4 := ip.To4(); ip4 != nil && !ip4.IsUnspecified() && !ip4.IsMulticast() && !ip4.IsLoopback() {
					return iface, append(net.IP(nil), ip4...), nil
				}
			}
		}
		return net.Interface{}, nil, errors.New("no active multicast-capable IPv4 network interface found")
	}

	iface, err := net.InterfaceByName(name)
	if err != nil {
		return net.Interface{}, nil, fmt.Errorf("resolve ONVIF interface %q: %w", name, err)
	}
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return net.Interface{}, nil, fmt.Errorf("ONVIF interface %q must be up and multicast-capable", name)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return net.Interface{}, nil, fmt.Errorf("list ONVIF interface addresses: %w", err)
	}
	for _, address := range addresses {
		var ip net.IP
		switch value := address.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsUnspecified() && !ip4.IsMulticast() {
			return *iface, append(net.IP(nil), ip4...), nil
		}
	}
	return net.Interface{}, nil, fmt.Errorf("ONVIF interface %q has no assigned IPv4 address", name)
}

func openDiscoverySocket(local *net.UDPAddr) (udpPacketConn, error) {
	conn, err := net.ListenUDP("udp4", local)
	if err != nil {
		return nil, err
	}
	return ipv4PacketConn{ipv4.NewPacketConn(conn)}, nil
}

type ipv4PacketConn struct{ *ipv4.PacketConn }

func (c ipv4PacketConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	return c.PacketConn.WriteTo(payload, nil, addr)
}

func (c ipv4PacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	n, _, addr, err := c.PacketConn.ReadFrom(buffer)
	return n, addr, err
}

type udpDiscoverySession struct {
	mu          sync.Mutex
	opMu        sync.Mutex
	conn        udpPacketConn
	destination *net.UDPAddr
	closed      bool
}

func (s *udpDiscoverySession) Send(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.mu.Unlock()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if err := s.conn.SetDeadline(contextDeadline(ctx)); err != nil {
		return err
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.conn.SetDeadline(time.Now())
	})
	_, err := s.conn.WriteTo(payload, s.destination)
	if !stop() {
		<-callbackDone
	}
	return err
}

func (s *udpDiscoverySession) Receive(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	s.mu.Unlock()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if err := s.conn.SetDeadline(contextDeadline(ctx)); err != nil {
		return nil, err
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.conn.SetDeadline(time.Now())
	})
	buffer := make([]byte, maxDiscoveryPacket+1)
	n, _, err := s.conn.ReadFrom(buffer)
	if !stop() {
		<-callbackDone
	}
	if err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ctx.Err()
			}
			return nil, ErrDiscoveryTimeout
		}
		var netErr net.Error
		if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, ErrDiscoveryTimeout
		}
		return nil, err
	}
	return buffer[:n], nil
}

func contextDeadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func (s *udpDiscoverySession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}
