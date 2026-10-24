package wsmedia

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"github.com/gorilla/websocket"
)

func TestListenerVideoBufferFramesMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		secure bool
		want   int
	}{
		{"auto", Config{}, false, 1},
		{"limit", Config{HTTPVideoBufferFrames: 9}, false, 8},
		{"http", Config{HTTPVideoBufferFrames: 8, HTTPSVideoBufferFrames: 4}, false, 8},
		{"https", Config{HTTPVideoBufferFrames: 8, HTTPSVideoBufferFrames: 4}, true, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(tc.cfg)
			ts := httptest.NewUnstartedServer(http.HandlerFunc(s.handleWS))
			if tc.secure {
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()
			defer s.CloseClients()
			dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // local test certificate
			c, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, message, err := c.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var meta metadata
			if err := json.Unmarshal(message[1:], &meta); err != nil {
				t.Fatal(err)
			}
			if message[0] != msgMetadata || meta.VideoBufferFrames != tc.want {
				t.Fatalf("unexpected metadata: %+v", meta)
			}
		})
	}
}

func TestVideoTimestampWirePreservesSourceAndOwnership(t *testing.T) {
	for _, codec := range []struct {
		name   string
		format capture.VideoFormat
		kind   byte
	}{
		{"h264", capture.FormatH264, msgH264}, {"hevc", capture.FormatHEVC, msgHEVC},
	} {
		t.Run(codec.name, func(t *testing.T) {
			s := NewServer(Config{})
			ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
			defer ts.Close()
			defer s.CloseClients()
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err := c.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			for _, source := range []time.Time{time.UnixMicro(1791655600123456), time.UnixMicro(1791655600140123), {}} {
				var freed atomic.Int32
				data := []byte{0, 0, 0, 1, 0x65, 0x80}
				f := bridge.Frame{Format: codec.format, Data: data, CapturedAt: source, Buffer: framebuf.New(data, func() { freed.Add(1) })}
				before := time.Now().UnixMicro()
				s.PushFrame(f)
				f.Release()
				_, message, err := c.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if len(message) != 9+len(data) || message[0] != codec.kind || !bytes.Equal(message[9:], data) {
					t.Fatalf("invalid timestamped video message: %x", message)
				}
				stamp := int64(binary.LittleEndian.Uint64(message[1:9]))
				if !source.IsZero() && stamp != source.UnixMicro() {
					t.Fatalf("source PTS changed: %d", stamp)
				}
				if source.IsZero() && (stamp < before || stamp > time.Now().UnixMicro()) {
					t.Fatalf("invalid fallback PTS: %d", stamp)
				}
				deadline := time.Now().Add(time.Second)
				for freed.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if freed.Load() != 1 {
					t.Fatal("timestamp header retained media buffer")
				}
			}
		})
	}
}

func TestQPCPlaybackTimestampIndependentOfDiagnosticClock(t *testing.T) {
	for _, codec := range []struct {
		format capture.VideoFormat
		kind   byte
	}{
		{capture.FormatH264, msgH264}, {capture.FormatHEVC, msgHEVC},
	} {
		s := NewServer(Config{})
		c := &wsClient{sendCh: make(chan *outboundMessage, 8), done: make(chan struct{})}
		s.clients[c] = struct{}{}
		for i, diagnostic := range []time.Time{time.Now(), time.Now().Add(-time.Hour), {}, time.Now().Add(time.Hour)} {
			pts := int64(123456789 + i*16667)
			data := []byte{0, 0, 0, 1, 0x65}
			f := bridge.Frame{Format: codec.format, Data: data, CapturedAt: diagnostic, PresentationPTS: pts}
			s.PushFrame(f)
			msg := <-c.sendCh
			wire := msg.bytes()
			if len(wire) != 9+len(data) || wire[0] != codec.kind || int64(binary.LittleEndian.Uint64(wire[1:9])) != pts || !bytes.Equal(wire[9:], data) {
				t.Fatalf("Go time changed QPC playback PTS: %x", wire)
			}
			msg.release()
		}
		for _, invalid := range []int64{-1, 1 << 53} {
			s.PushFrame(bridge.Frame{Format: codec.format, Data: []byte{1}, PresentationPTS: invalid})
			if len(c.sendCh) != 0 {
				t.Fatal("invalid QPC PTS was replaced by Go time")
			}
		}
		c.close()
	}
}

func TestVideoDiagnosticsBoundsAndRateLimit(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{}
	s.handleInput(c, []byte{msgVideoDiagnostics, '{'})
	if !c.lastVideoDiagnostics.IsZero() {
		t.Fatal("malformed report accepted")
	}
	if s.logVideoDiagnostics(bytes.Repeat([]byte{' '}, 4097)) {
		t.Fatal("oversized report accepted")
	}
	if s.logVideoDiagnostics([]byte(`{"decodedFps":"bad"}`)) {
		t.Fatal("invalid metric accepted")
	}
	s.handleInput(c, append([]byte{msgVideoDiagnostics}, []byte(`{"receivedFps":60,"decodedFps":58,"drawnFps":56,"presentationTargetBufferMs":40}`)...))
	first := c.lastVideoDiagnostics
	if first.IsZero() {
		t.Fatal("valid report rejected")
	}
	s.handleInput(c, []byte{msgVideoDiagnostics, '{', '}'})
	if c.lastVideoDiagnostics != first {
		t.Fatal("rapid repeated report was not rate limited")
	}
}
