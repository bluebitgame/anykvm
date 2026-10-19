package wol

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func listenWake(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	return c
}
func TestSendMagicPacket(t *testing.T) {
	c := listenWake(t)
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: c.LocalAddr().String()}
	if err := o.Send(); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 200)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte{255}, 6), bytes.Repeat([]byte{2, 17, 34, 51, 68, 85}, 16)...)
	if !bytes.Equal(b[:n], want) {
		t.Fatalf("invalid magic packet: %x", b[:n])
	}
}
func TestValidate(t *testing.T) {
	for _, o := range []Options{{MAC: "bad"}, {MAC: "00:00:00:00:00:00"}, {MAC: "ff:ff:ff:ff:ff:ff"}, {MAC: "02:11:22:33:44:55:66:77"}, {Broadcast: "127.0.0.1:0"}, {Broadcast: ":9"}, {Broadcast: "[::1]:65536"}} {
		if o.Validate() == nil {
			t.Errorf("accepted invalid options: %+v", o)
		}
	}
	if err := (Options{}).Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestValidateDestinationTypes(t *testing.T) {
	for _, address := range []string{"192.168.1.255:9", "192.168.1.20:7", "239.1.2.3:9", "[::1]:9", "[ff02::1%en0]:9", "wake.example:9"} {
		if err := (Options{Broadcast: address}).Validate(); err != nil {
			t.Errorf("rejected %q: %v", address, err)
		}
	}
}

func TestDestinationDefaultPort(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "255.255.255.255:9"},
		{"192.168.1.255", "192.168.1.255:9"},
		{"192.168.1.255:7", "192.168.1.255:7"},
		{"wake.example", "wake.example:9"},
		{"wake.example:7", "wake.example:7"},
		{"::1", "[::1]:9"},
		{"[::1]", "[::1]:9"},
		{"[::1]:7", "[::1]:7"},
		{"2001:db8::9", "[2001:db8::9]:9"},
		{"fe80::1%en0", "[fe80::1%en0]:9"},
		{"[fe80::1%en0]:7", "[fe80::1%en0]:7"},
		{"wake.example:", "wake.example:9"},
	} {
		o := Options{Broadcast: tc.input}
		got, err := o.destination()
		if err != nil || got != tc.want {
			t.Errorf("destination(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
		if err := o.Validate(); err != nil {
			t.Errorf("rejected %q: %v", tc.input, err)
		}
	}
	for _, input := range []string{"[::1", "[invalid]", "[::1]:bad", "host:65536", "host:-1", "host:0", "not:ipv6:address", ":", " ", "[127.0.0.1]"} {
		if err := (Options{Broadcast: input}).Validate(); err == nil {
			t.Errorf("accepted invalid destination %q", input)
		}
	}
}

func TestSendMagicPacketIPv6(t *testing.T) {
	c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: c.LocalAddr().String()}
	if err := o.Send(); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 200)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte{255}, 6), bytes.Repeat([]byte{2, 17, 34, 51, 68, 85}, 16)...)
	if !bytes.Equal(b[:n], want) {
		t.Fatalf("invalid IPv6 magic packet: %x", b[:n])
	}
}

func TestDialWakesUnavailableTarget(t *testing.T) {
	udp := listenWake(t)
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	result := make(chan error, 1)
	go func() {
		packet := make([]byte, 102)
		_, _, err := udp.ReadFromUDP(packet)
		if err != nil {
			result <- err
			return
		}
		server, err := net.Listen("tcp4", address)
		if err != nil {
			result <- err
			return
		}
		defer server.Close()
		server.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
		conn, err := server.Accept()
		if err == nil {
			conn.Close()
		}
		result <- err
	}()
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: udp.LocalAddr().String()}
	conn, err := o.DialContext(context.Background(), address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
func TestDialReachableDoesNotWake(t *testing.T) {
	udp := listenWake(t)
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: udp.LocalAddr().String()}
	conn, err := o.DialContext(context.Background(), server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	udp.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := udp.ReadFromUDP(make([]byte, 200)); err == nil {
		t.Fatal("reachable target received magic packet")
	}
}
func TestDialWakeCancellation(t *testing.T) {
	udp := listenWake(t)
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := server.Addr().String()
	server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: udp.LocalAddr().String()}
	if _, err := o.DialContext(ctx, address, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", err)
	}
}

func TestDialWakeDeadlineIsOneSecond(t *testing.T) {
	udp := listenWake(t)
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := server.Addr().String()
	server.Close()
	o := Options{MAC: "02:11:22:33:44:55", Broadcast: udp.LocalAddr().String()}
	start := time.Now()
	_, err = o.DialContext(context.Background(), address, 10*time.Second)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wake deadline: %v", err)
	}
	if elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("wake deadline took %s; expected one second", elapsed)
	}
}
