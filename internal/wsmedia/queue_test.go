package wsmedia

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJPEGQueueKeepsLatestWithoutEvictingAudio(t *testing.T) {
	c := &wsClient{sendCh: make(chan *outboundMessage, 8), jpegReady: make(chan struct{}, 1), done: make(chan struct{})}
	c.trySend([]byte{msgPCM, 42})
	for i := 0; i < 100; i++ {
		c.trySend([]byte{msgJPEG, byte(i)})
	}
	if len(c.sendCh) != 1 || (<-c.sendCh).bytes()[0] != msgPCM {
		t.Fatal("JPEG replaced an ordered message")
	}
	if len(c.jpegReady) != 1 {
		t.Fatal("JPEG notifications accumulated")
	}
	<-c.jpegReady
	if msg := c.takeJPEG().bytes(); len(msg) != 2 || msg[1] != 99 {
		t.Fatalf("did not retain latest JPEG: %v", msg)
	}
	if c.takeJPEG() != nil {
		t.Fatal("JPEG delivered twice")
	}
}

func TestJPEGWebSocketDeliversLatestFrame(t *testing.T) {
	s := NewServer(Config{})
	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := c.ReadMessage(); err != nil || len(msg) == 0 || msg[0] != msgMetadata {
		t.Fatal("metadata did not precede JPEG")
	}
	for i := 0; i < 100; i++ {
		s.PushFrame(bridge.Frame{Format: capture.FormatJPEG, Data: []byte{0xff, 0xd8, byte(i)}})
	}
	previous := -1
	for previous < 99 {
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if len(msg) != 4 || msg[0] != msgJPEG || int(msg[3]) <= previous {
			t.Fatalf("stale or malformed JPEG delivery: %v", msg)
		}
		previous = int(msg[3])
	}
}

func TestJPEGQueueConcurrentProducersAndCodecSwitch(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 8), jpegReady: make(chan struct{}, 1), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	var producers sync.WaitGroup
	for i := 0; i < 4; i++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for j := 0; j < 100; j++ {
				c.trySend([]byte{msgJPEG, byte(j)})
			}
		}()
	}
	producers.Wait()
	s.broadcastH264([]byte{msgH264, 1}, nil)
	if c.takeJPEG() != nil || len(c.jpegReady) != 0 {
		t.Fatal("JPEG survived codec switch")
	}
	if (<-c.sendCh).bytes()[0] != msgH264 {
		t.Fatal("ordered video lost")
	}
	close(c.done)
	c.trySend([]byte{msgJPEG, 2})
	if c.takeJPEG() != nil {
		t.Fatal("closed client retained JPEG")
	}
}

func (m *outboundMessage) bytes() []byte {
	if len(m.header) == 0 {
		return m.data
	}
	return append(append([]byte(nil), m.header...), m.data...)
}
