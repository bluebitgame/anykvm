package vnc

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestVNCInputWithoutConnection(t *testing.T) {
	c, err := NewVNCClient("vnc://localhost")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []func() error{
		func() error { return c.KeyDown(4) },
		func() error { return c.KeyRelease(4) }, c.KeyUp,
		func() error { return c.MouseButton(1, true) },
		func() error { return c.MouseWheel(1) },
		func() error { return c.MouseMove(100, 100) },
		func() error { return c.MouseMoveRel(1, 1) },
	} {
		if err := input(); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.pressed) != 0 || c.lastBtn != 0 {
		t.Fatal("disconnected input changed session state")
	}
}

func TestVNCInputReturnsWriteError(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	peer.Close()
	c := &VNCClient{conn: conn, running: true}
	if err := c.KeyDown(4); err == nil {
		t.Fatal("lost write error")
	}
}

func TestVNCInputWriteTimeout(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	c := &VNCClient{conn: conn, running: true}
	done := make(chan error, 1)
	go func() { done <- c.MouseButton(1, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected stalled peer error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input blocked on stalled peer")
	}
}

func TestVNCPointerClampsCoordinates(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	c := &VNCClient{conn: conn, running: true, fbWidth: 100, fbHeight: 50}
	done := make(chan error, 1)
	go func() { done <- c.MouseMove(-100, 65535) }()
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg [6]byte
	if _, err := io.ReadFull(peer, msg[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if x, y := binary.BigEndian.Uint16(msg[2:]), binary.BigEndian.Uint16(msg[4:]); x != 0 || y != 49 {
		t.Fatalf("coordinates = %d,%d; want 0,49", x, y)
	}
}
