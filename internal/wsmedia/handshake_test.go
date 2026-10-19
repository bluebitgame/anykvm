package wsmedia

import (
	"testing"
	"time"
)

func TestHelloSelectsCodecBeforeCaptureStarts(t *testing.T) {
	selected, starts, stops := false, 0, 0
	s := NewServer(Config{
		OnVideoCodecsChanged: func(_, hevc, _ bool) { selected = hevc },
		OnClientConnected: func() {
			if !selected {
				t.Error("capture opened before codec selection")
			}
			starts++
		},
		OnClientDisconnected: func() { stops++ },
	})
	c := &wsClient{done: make(chan struct{})}
	s.addClient(c)
	if starts != 0 {
		t.Fatal("capture started before hello")
	}
	s.handleInput(c, append([]byte{msgHello}, []byte(`{"h264":true,"hevc":true}`)...))
	s.handleInput(c, append([]byte{msgHello}, []byte(`{"h264":true,"hevc":true}`)...))
	if starts != 1 {
		t.Fatalf("capture starts = %d", starts)
	}
	s.removeClient(c)
	if stops != 1 {
		t.Fatalf("capture stops = %d", stops)
	}
}

func TestClosedPendingClientCannotStartCapture(t *testing.T) {
	starts, stops := 0, 0
	s := NewServer(Config{OnVideoCodecsChanged: func(bool, bool, bool) {},
		OnClientConnected: func() { starts++ }, OnClientDisconnected: func() { stops++ }})
	c := &wsClient{done: make(chan struct{})}
	s.addClient(c)
	c.close()
	s.activateClient(c)
	s.removeClient(c)
	s.activateClient(c)
	if starts != 0 || stops != 0 {
		t.Fatalf("pending client changed capture lifecycle: %d/%d", starts, stops)
	}
}

func TestLegacyClientStartsAfterGracePeriod(t *testing.T) {
	started := make(chan struct{}, 1)
	s := NewServer(Config{OnVideoCodecsChanged: func(bool, bool, bool) {}, OnClientConnected: func() { started <- struct{}{} }})
	c := &wsClient{done: make(chan struct{})}
	s.addClient(c)
	defer s.removeClient(c)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy client never started capture")
	}
}
