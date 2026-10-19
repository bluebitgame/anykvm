package coredevice

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type recordingXPC struct {
	reports [][]byte
	fail    error
}

func (r *recordingXPC) Send(m map[string]interface{}, _ ...uint32) error {
	if r.fail != nil {
		return r.fail
	}
	payload, _ := m["payload"].(map[string]interface{})
	send, _ := payload["send"].(map[string]interface{})
	p, _ := send["_0"].([]byte)
	r.reports = append(r.reports, append([]byte{}, p...))
	return nil
}
func (*recordingXPC) ReceiveOnClientServerStream() (map[string]interface{}, error) {
	return nil, errors.New("unused")
}
func (*recordingXPC) ReceiveOnServerClientStream() (map[string]interface{}, error) {
	return nil, errors.New("unused")
}
func TestHeldKeysDragAndRelease(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wire := &recordingXPC{}
	channel := &xpcChannel{x: wire, conn: a, cancel: func() bool { return true }}
	input := &inputState{channel: channel, keyboardID: keyboardServiceID, x: 16384, y: 16384}
	c, _ := NewClient("coredevice", Options{})
	c.session = &session{input: input}
	for _, f := range []func() error{func() error { return c.KeyDown(225) }, func() error { return c.KeyDown(4) }, func() error { return c.KeyRelease(4) }, func() error { return c.MouseMove(0, 32767) }, func() error { return c.MouseButton(1, true) }, func() error { return c.MouseMove(32767, 0) }, func() error { return c.MouseButton(1, false) }} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	if wire.reports[1][1] != 16 || wire.reports[1][29] != 2 || wire.reports[2][1] != 0 || wire.reports[2][29] != 2 {
		t.Fatal("modifier was lost when releasing A")
	}
	if len(wire.reports) != 6 || wire.reports[3][3] != 0xc2 || wire.reports[4][4] != 255 || wire.reports[5][3] != 2 {
		t.Fatal("hover injected touch or drag missed release")
	}
	// Concurrent inputs must serialize their full keyboard bitmaps.
	var wg sync.WaitGroup
	for key := uint8(4); key < 20; key++ {
		wg.Add(1)
		go func(k uint8) { defer wg.Done(); _ = c.KeyDown(k); _ = c.KeyRelease(k) }(key)
	}
	wg.Wait()
	input.close()
	last := wire.reports[len(wire.reports)-1]
	for _, v := range last[1:31] {
		if v != 0 {
			t.Fatal("close left keys held")
		}
	}
	before := len(wire.reports)
	_ = c.KeyDown(4)
	input.close()
	if len(wire.reports) != before {
		t.Fatal("sent after close")
	}
}
func TestDisplayInvokeCancellationClosesTransport(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// A blocking write must end on cancellation, not leave a worker behind.
	channel := &xpcChannel{x: &blockedXPC{conn: a}, conn: a, cancel: func() bool { return true }}
	_, err := channel.invoke(ctx, map[string]interface{}{})
	if err == nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("blocked exchange escaped deadline", err)
	}
}

type blockedXPC struct{ conn net.Conn }

func (b *blockedXPC) Send(map[string]interface{}, ...uint32) error {
	_, err := b.conn.Write([]byte{1})
	return err
}
func (*blockedXPC) ReceiveOnClientServerStream() (map[string]interface{}, error) {
	return nil, errors.New("unused")
}
func (*blockedXPC) ReceiveOnServerClientStream() (map[string]interface{}, error) {
	return nil, errors.New("unused")
}
