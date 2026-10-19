// Package wol sends Wake-on-LAN magic packets and connects after a target wakes.
package wol

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Options are shared by upstream protocols. An empty MAC disables waking.
type Options struct {
	MAC       string `yaml:"mac,omitempty"`
	Broadcast string `yaml:"broadcast,omitempty"` // UDP destination with optional port (default 9); empty uses 255.255.255.255:9.
}

// Bare IPv6 is always a complete address. An explicit IPv6 port requires
// brackets, so a final address segment is never mistaken for a port.
func (o Options) destination() (string, error) {
	raw := o.Broadcast
	if raw == "" {
		return "255.255.255.255:9", nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host = raw
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			host = raw[1 : len(raw)-1]
			if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() {
				return "", fmt.Errorf("wol.broadcast contains an invalid bracketed IPv6 address")
			}
		} else if _, err := netip.ParseAddr(raw); err != nil && strings.ContainsAny(raw, ":[]") {
			return "", fmt.Errorf("wol.broadcast must be an IP address or hostname with optional UDP port (IPv6 port: [address]:port)")
		}
		port = "9"
	}
	if port == "" {
		port = "9"
	}
	n, err := strconv.Atoi(port)
	if host == "" || strings.TrimSpace(host) != host || err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("wol.broadcast must have a host and UDP port in 1..65535 (default 9)")
	}
	return net.JoinHostPort(host, port), nil
}

func (o Options) Validate() error {
	if o.MAC != "" {
		mac, err := net.ParseMAC(o.MAC)
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
			return fmt.Errorf("wol.mac must be a six-byte unicast MAC address")
		}
		zero := true
		for _, b := range mac {
			zero = zero && b == 0
		}
		if zero {
			return fmt.Errorf("wol.mac must not be all zeros")
		}
	}
	_, err := o.destination()
	return err
}

// Send sends one standard 102-byte magic packet. No network traffic is sent
// when waking is disabled.
func (o Options) Send() error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.MAC == "" {
		return nil
	}
	mac, _ := net.ParseMAC(o.MAC)
	packet := make([]byte, 102)
	for i := 0; i < 6; i++ {
		packet[i] = 0xff
	}
	for i := 6; i < len(packet); i += 6 {
		copy(packet[i:], mac)
	}
	destination, err := o.destination()
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp", destination)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = conn.Write(packet)
	return err
}

// DialContext first tries the service, then wakes an unreachable target and
// retries TCP for up to one second. Authentication stays with the caller:
// failed credentials must never cause wake packets or connection retries.
func (o Options) DialContext(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	probeTimeout := timeout
	if o.MAC != "" {
		probeTimeout = min(timeout, time.Second)
	}
	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(ctx, "tcp", address)
	if err == nil || o.MAC == "" || ctx.Err() != nil {
		return conn, err
	}
	if err := o.Send(); err != nil {
		return nil, fmt.Errorf("wol: send magic packet: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		conn, err = (&net.Dialer{Timeout: min(timeout, time.Second)}).DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("wol: waiting for target: %w (last connection error: %v)", ctx.Err(), err)
		case <-timer.C:
		}
	}
}
