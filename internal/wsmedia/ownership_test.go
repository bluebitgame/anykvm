package wsmedia

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/videotiming"
	"github.com/gorilla/websocket"
)

func ownedWSFrame(format capture.VideoFormat, freed *atomic.Int32) bridge.Frame {
	data := []byte{0, 0, 0, 1, 0x65}
	return bridge.Frame{Format: format, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
}

func TestVideoTimingFollowsQueuedPayload(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 8), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	defer c.close()
	trace := &videotiming.Trace{Present: time.Now().Add(-20 * time.Millisecond), Ready: time.Now(), Path: "d3d11/test"}
	f := bridge.Frame{Format: capture.FormatH264, Data: []byte{0, 0, 0, 1, 0x65}, Timing: videotiming.Delivery{Trace: trace}}
	s.PushFrame(f)
	select {
	case msg := <-c.sendCh:
		defer msg.release()
		if msg.timing.Trace != trace || msg.timing.Enqueued.Before(trace.Ready) || !f.Timing.Enqueued.IsZero() {
			t.Fatal("source timing was lost or mutated during queue ownership transfer")
		}
	default:
		t.Fatal("frame did not reach send queue")
	}
}
func TestWSOwnedReplacementCancellationAndClose(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 1), jpegReady: make(chan struct{}, 1), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	var freed atomic.Int32
	f := ownedWSFrame(capture.FormatJPEG, &freed)
	s.PushFrame(f)
	if &c.jpeg.data[0] != &f.Data[0] {
		t.Fatal("queue copied owned frame")
	}
	f.Release()
	if freed.Load() != 0 {
		t.Fatal("JPEG released before replacement")
	}
	f = ownedWSFrame(capture.FormatJPEG, &freed)
	s.PushFrame(f)
	f.Release()
	if freed.Load() != 1 {
		t.Fatal("replaced JPEG leaked")
	}
	f = ownedWSFrame(capture.FormatH264, &freed)
	s.PushFrame(f)
	f.Release()
	if freed.Load() != 2 {
		t.Fatal("codec switch retained JPEG")
	}
	stop := make(chan struct{})
	returned := make(chan struct{})
	f = ownedWSFrame(capture.FormatH264, &freed)
	go func() { s.PushFrameWithStop(f, stop); close(returned) }()
	close(stop)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("cancelled push blocked")
	}
	f.Release()
	if freed.Load() != 3 {
		t.Fatal("cancelled message leaked")
	}
	c.close()
	if freed.Load() != 4 {
		t.Fatal("closed queue leaked")
	}
	f = ownedWSFrame(capture.FormatJPEG, &freed)
	s.PushFrame(f)
	f.Release()
	if freed.Load() != 5 {
		t.Fatal("send after close retained frame")
	}
}

func TestWSLargeBorrowedPayloadDeliveredAndReleased(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "TLS"}[tls], func(t *testing.T) {
			s := NewServer(Config{})
			ts := httptest.NewUnstartedServer(http.HandlerFunc(s.handleWS))
			dialer := *websocket.DefaultDialer
			if tls {
				ts.StartTLS()
				dialer.TLSClientConfig = ts.Client().Transport.(*http.Transport).TLSClientConfig
			} else {
				ts.Start()
			}
			defer ts.Close()
			defer s.CloseClients()
			scheme := "ws"
			if tls {
				scheme = "wss"
			}
			c, _, err := dialer.Dial(scheme+strings.TrimPrefix(ts.URL, strings.Split(ts.URL, ":")[0]), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err = c.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte{0, 1, 2, 3, 255}, 100000)
			var freed atomic.Int32
			f := bridge.Frame{Format: capture.FormatHEVC, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
			s.PushFrame(f)
			f.Release()
			_, msg, err := c.ReadMessage()
			if err != nil || len(msg) != len(data)+9 || msg[0] != msgHEVC || !bytes.Equal(msg[9:], data) {
				t.Fatalf("fragmented media delivery: %v (%d bytes)", err, len(msg))
			}
			deadline := time.Now().Add(time.Second)
			for freed.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if freed.Load() != 1 {
				t.Fatal("completed write retained native lease")
			}
		})
	}
}
