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

func TestReconnectNegotiationAndRecovery(t *testing.T) {
	starts, stops, keys := 0, 0, 0
	var s *Server
	s = NewServer(Config{
		OnVideoCodecsChanged: func(bool, bool, bool) {
			// Last-disconnect cleanup must not run between negotiation and start.
			if s.lifecycleMu.TryLock() {
				s.lifecycleMu.Unlock()
				t.Error("capability selection is outside lifecycle lock")
			}
		},
		OnClientConnected:        func() { starts++ },
		OnClientDisconnected:     func() { stops++ },
		OnVideoKeyframeRequested: func() { keys++ },
	})
	hello := append([]byte{msgHello}, []byte(`{"h264":true}`)...)
	first := &wsClient{done: make(chan struct{})}
	s.addClient(first)
	s.handleInput(first, hello)
	// Fast reconnect: new browser joins before the old socket is removed.
	second := &wsClient{done: make(chan struct{})}
	s.addClient(second)
	s.handleInput(second, hello)
	s.handleInput(second, hello) // Repeated capabilities must not create a consumer.
	s.removeClient(first)
	if starts != 1 || stops != 0 || keys != 2 {
		t.Fatalf("overlap lifecycle %d/%d, recovery=%d", starts, stops, keys)
	}
	s.removeClient(second)
	// A later connection must start capture and request recovery again.
	third := &wsClient{done: make(chan struct{})}
	s.addClient(third)
	s.handleInput(third, hello)
	s.removeClient(third)
	if starts != 2 || stops != 2 || keys != 3 {
		t.Fatalf("reopen lifecycle %d/%d, recovery=%d", starts, stops, keys)
	}
	// A stale hello from a removed socket must not reconfigure capture.
	s.handleInput(first, hello)
	if starts != 2 || stops != 2 || keys != 3 {
		t.Fatal("closed socket changed capture")
	}
}
