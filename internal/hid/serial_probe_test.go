package hid

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Model a serial driver that ignores Go deadlines; cancellation must close it.
type probeNoDeadlineConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *probeNoDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (c *probeNoDeadlineConn) SetWriteDeadline(time.Time) error { return nil }
func (c *probeNoDeadlineConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func TestSerialProbeCancelsBlockedIO(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			port := &probeNoDeadlineConn{Conn: client}
			defer client.Close()
			finished := make(chan struct{})
			start := time.Now()
			ok := runSerialProbe(250*time.Millisecond,
				func() (serialReadWriter, error) { return port, nil },
				func(serialReadWriter) error { return nil },
				func(rw serialReadWriter) bool {
					defer close(finished)
					if operation == "read" {
						_, err := rw.Read(make([]byte, 8))
						return err == nil
					}
					_, err := rw.Write(make([]byte, 8))
					return err == nil
				})
			if ok || time.Since(start) > time.Second {
				t.Fatalf("blocked %s did not time out: ok=%v elapsed=%s", operation, ok, time.Since(start))
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("cancellation left I/O blocked")
			}
			if port.closes.Load() != 1 {
				t.Fatalf("port closed %d times", port.closes.Load())
			}
		})
	}
}

func TestSerialProbeClosesLateOpenWithoutQuery(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	release := make(chan struct{})
	var configured, queried atomic.Bool
	start := time.Now()
	ok := runSerialProbe(30*time.Millisecond,
		func() (serialReadWriter, error) { <-release; return client, nil },
		func(serialReadWriter) error { configured.Store(true); return nil },
		func(serialReadWriter) bool { queried.Store(true); return true })
	if ok || time.Since(start) > time.Second {
		t.Fatalf("late open blocked discovery: ok=%v elapsed=%s", ok, time.Since(start))
	}
	close(release)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late-opened port was not closed: %v", err)
	}
	if configured.Load() || queried.Load() {
		t.Fatal("configured or queried a port after discovery timed out")
	}
}

func TestSerialProbeRejectsSilentAndOtherProtocols(t *testing.T) {
	for _, otherProtocol := range []bool{false, true} {
		t.Run(map[bool]string{false: "silent", true: "other protocol"}[otherProtocol], func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					var request [8]byte
					if _, err := io.ReadFull(peer, request[:]); err != nil {
						return
					}
					if otherProtocol {
						reply := serialHIDFrame(0x83, [4]byte{1, 0x27, 0, 1})
						reply[0] = 0x34
						crc := crc16modbus(reply[:6])
						reply[6], reply[7] = byte(crc>>8), byte(crc)
						if _, err := peer.Write(reply); err != nil {
							return
						}
					}
				}
			}()
			start := time.Now()
			ok := runSerialProbe(serialProbeTimeout,
				func() (serialReadWriter, error) { return client, nil },
				func(serialReadWriter) error { return nil }, probeHIDBridgeConnection)
			if ok || time.Since(start) > serialProbeTimeout+500*time.Millisecond {
				t.Fatalf("invalid device not bounded: ok=%v elapsed=%s", ok, time.Since(start))
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("peer remained connected after timeout")
			}
		})
	}
}

func TestSerialDiscoveryRetriesLostQuery(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	var requests atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var request [8]byte
			if _, err := io.ReadFull(peer, request[:]); err != nil {
				return
			}
			if requests.Add(1) == 1 {
				continue
			}
			for _, frame := range [][]byte{
				serialHIDFrame(0x83, [4]byte{1, 0x27, 0, 1}),
				serialHIDFrame(0x80, [4]byte{}),
				serialHIDFrame(0x81, [4]byte{0x20}),
			} {
				for _, part := range [][]byte{frame[:3], frame[3:]} {
					if _, err := peer.Write(part); err != nil {
						return
					}
				}
			}
		}
	}()
	if !runSerialProbe(serialProbeTimeout,
		func() (serialReadWriter, error) { return client, nil },
		func(serialReadWriter) error { return nil }, probeHIDBridgeConnection) {
		t.Fatal("compatible device not found after a lost first query")
	}
	if requests.Load() != 2 {
		t.Fatalf("queries = %d, want 2", requests.Load())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("successful probe did not release its port")
	}
}

func TestCH9329DiscoveryDelayedFragmentedReply(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := make([]byte, len(ch9329Frame(ch9329InfoCmd)))
		if _, err := io.ReadFull(peer, request); err != nil {
			return
		}
		time.Sleep(120 * time.Millisecond)
		reply := append([]byte{0xff, 0}, ch9329Frame(ch9329InfoCmd|0x80, 1, 2, 3)...)
		for _, b := range reply {
			if _, err := peer.Write([]byte{b}); err != nil {
				return
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	if !runSerialProbe(serialProbeTimeout,
		func() (serialReadWriter, error) { return client, nil },
		func(serialReadWriter) error { return nil }, probeCH9329Connection) {
		t.Fatal("CH9329 delayed fragmented response was not detected")
	}
	<-done
}
