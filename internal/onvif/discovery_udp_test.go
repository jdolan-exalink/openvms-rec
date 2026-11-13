package onvif

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakePacketConn struct {
	mu          sync.Mutex
	iface       *net.Interface
	ttl         int
	destination net.Addr
	deadline    time.Time
	closed      bool
	read        func() (int, net.Addr, error)
}

func (c *fakePacketConn) SetMulticastInterface(i *net.Interface) error { c.iface = i; return nil }
func (c *fakePacketConn) SetMulticastTTL(ttl int) error                { c.ttl = ttl; return nil }
func (c *fakePacketConn) WriteTo(_ []byte, a net.Addr) (int, error)    { c.destination = a; return 1, nil }
func (c *fakePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if c.read != nil {
		return c.read()
	}
	copy(b, []byte("reply"))
	return 5, &net.UDPAddr{}, nil
}
func (c *fakePacketConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadline = d
	c.mu.Unlock()
	return nil
}
func (c *fakePacketConn) Close() error { c.closed = true; return nil }

func TestUDPTransportBindsAndRoutesOneSessionPerInterface(t *testing.T) {
	iface := net.Interface{Index: 7, Name: "lan0", Flags: net.FlagUp | net.FlagMulticast}
	var locals []*net.UDPAddr
	var conns []*fakePacketConn
	transport := &UDPTransport{
		resolve: func(name string) (net.Interface, net.IP, error) {
			if name != "lan0" {
				t.Fatalf("interface=%q", name)
			}
			return iface, net.IPv4(192, 0, 2, 4), nil
		},
		open: func(local *net.UDPAddr) (udpPacketConn, error) {
			locals = append(locals, local)
			c := &fakePacketConn{}
			conns = append(conns, c)
			return c, nil
		},
	}
	one, err := transport.Open(context.Background(), "lan0")
	if err != nil {
		t.Fatal(err)
	}
	two, err := transport.Open(context.Background(), "lan0")
	if err != nil {
		t.Fatal(err)
	}
	if err = one.Send(context.Background(), []byte("probe")); err != nil {
		t.Fatal(err)
	}
	if locals[0].IP.String() != "192.0.2.4" || locals[0].Port != 0 || conns[0].iface.Index != 7 || conns[0].ttl != 4 || conns[0].destination.String() != "239.255.255.250:3702" {
		t.Fatalf("unsafe socket configuration: local=%v conn=%+v", locals[0], conns[0])
	}
	if conns[0] == conns[1] {
		t.Fatal("sessions shared a mutable socket")
	}
	got, err := one.Receive(context.Background())
	if err != nil || string(got) != "reply" {
		t.Fatalf("receive=%q err=%v", got, err)
	}
	_ = one.Close()
	_ = two.Close()
	if !conns[0].closed || !conns[1].closed {
		t.Fatal("session socket leaked")
	}
}

func TestUDPTransportAutoDetectInterface(t *testing.T) {
	iface := net.Interface{Index: 3, Name: "eth1", Flags: net.FlagUp | net.FlagMulticast}
	transport := &UDPTransport{
		resolve: func(name string) (net.Interface, net.IP, error) {
			if name != "*" && name != "auto" {
				t.Fatalf("unexpected interface name: %q", name)
			}
			return iface, net.IPv4(192, 168, 1, 50), nil
		},
		open: func(_ *net.UDPAddr) (udpPacketConn, error) {
			return &fakePacketConn{}, nil
		},
	}
	session, err := transport.Open(context.Background(), "*")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_ = session.Close()

	sessionAuto, err := transport.Open(context.Background(), "auto")
	if err != nil {
		t.Fatalf("Open with auto failed: %v", err)
	}
	_ = sessionAuto.Close()
}

func TestUDPTransportRejectsUnsafeInterfaceBeforeOpeningSocket(t *testing.T) {
	cases := []struct {
		name  string
		iface net.Interface
		ip    net.IP
	}{
		{"down", net.Interface{Name: "lan0", Flags: net.FlagMulticast}, net.IPv4(192, 0, 2, 1)},
		{"multicast-disabled", net.Interface{Name: "lan0", Flags: net.FlagUp}, net.IPv4(192, 0, 2, 1)},
		{"IPv6-only", net.Interface{Name: "lan0", Flags: net.FlagUp | net.FlagMulticast}, net.ParseIP("2001:db8::1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			tr := &UDPTransport{resolve: func(string) (net.Interface, net.IP, error) { return tc.iface, tc.ip, nil }, open: func(*net.UDPAddr) (udpPacketConn, error) { opened = true; return &fakePacketConn{}, nil }}
			if _, err := tr.Open(context.Background(), "lan0"); err == nil || opened {
				t.Fatalf("err=%v opened=%t", err, opened)
			}
		})
	}
	tr := &UDPTransport{resolve: func(string) (net.Interface, net.IP, error) { return net.Interface{}, nil, errors.New("not found") }, open: func(*net.UDPAddr) (udpPacketConn, error) {
		t.Fatal("opened despite unresolved interface")
		return nil, nil
	}}
	if _, err := tr.Open(context.Background(), "missing"); err == nil {
		t.Fatal("unresolved interface accepted")
	}
}

func TestUDPReceiveCancellationInterruptsRead(t *testing.T) {
	conn := &fakePacketConn{}
	conn.read = func() (int, net.Addr, error) {
		for {
			conn.mu.Lock()
			deadline := conn.deadline
			conn.mu.Unlock()
			if !deadline.IsZero() && time.Now().After(deadline) {
				return 0, nil, context.DeadlineExceeded
			}
			time.Sleep(time.Millisecond)
		}
	}
	s := &udpDiscoverySession{conn: conn, destination: &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Receive(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receive did not stop on cancellation")
	}
}

type callbackRaceConn struct {
	deadlineMu      sync.Mutex
	deadline        time.Time
	callbackEntered chan struct{}
	callbackRelease chan struct{}
	readStarted     chan struct{}
	once            sync.Once
}

func (c *callbackRaceConn) SetMulticastInterface(*net.Interface) error { return nil }
func (c *callbackRaceConn) SetMulticastTTL(int) error                  { return nil }
func (c *callbackRaceConn) WriteTo([]byte, net.Addr) (int, error)      { return 1, nil }
func (c *callbackRaceConn) ReadFrom([]byte) (int, net.Addr, error) {
	c.readStarted <- struct{}{}
	<-c.callbackEntered
	return 1, &net.UDPAddr{}, nil
}
func (c *callbackRaceConn) SetDeadline(d time.Time) error {
	if !d.IsZero() && time.Until(d) < 0 {
		c.once.Do(func() { close(c.callbackEntered); <-c.callbackRelease })
	}
	c.deadlineMu.Lock()
	c.deadline = d
	c.deadlineMu.Unlock()
	return nil
}
func (c *callbackRaceConn) Close() error { return nil }

func TestUDPReceiveWaitsForCancellationDeadlineCallback(t *testing.T) {
	conn := &callbackRaceConn{callbackEntered: make(chan struct{}), callbackRelease: make(chan struct{}), readStarted: make(chan struct{})}
	s := &udpDiscoverySession{conn: conn, destination: &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Receive(ctx); done <- err }()
	<-conn.readStarted
	cancel()
	select {
	case err := <-done:
		close(conn.callbackRelease)
		t.Fatalf("Receive returned before cancellation callback completed: %v", err)
	case <-conn.callbackEntered:
	}
	select {
	case err := <-done:
		close(conn.callbackRelease)
		t.Fatalf("Receive returned while callback was still active: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(conn.callbackRelease)
	if err := <-done; err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := s.Send(context.Background(), []byte("next")); err != nil {
		t.Fatal(err)
	}
	conn.deadlineMu.Lock()
	deadline := conn.deadline
	conn.deadlineMu.Unlock()
	if !deadline.IsZero() {
		t.Fatalf("stale cancellation callback reset next I/O deadline: %v", deadline)
	}
}
