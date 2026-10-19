package wsmedia

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"anykvm/internal/capture"

	"github.com/gorilla/websocket"
)

// TestWSAudioDelivery verifies PushAudio reaches a connected browser as a PCM
// (0x02) binary message.
func TestWSAudioDelivery(t *testing.T) {
	s := NewServer(Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// First message is metadata (0x00); drain it.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := c.ReadMessage(); err != nil || len(msg) == 0 || msg[0] != msgMetadata {
		t.Fatalf("expected metadata first: msg=%v err=%v", msg, err)
	}

	// Give the server a moment to register the client, then push audio.
	time.Sleep(50 * time.Millisecond)
	pcm := make([]byte, 3840) // 20ms S16LE stereo @48k
	for i := range pcm {
		pcm[i] = byte(i)
	}
	s.PushAudio(capture.AudioFrame{PCM: pcm, SampleRate: 48000, Channels: 2})

	// Expect a 0x02 PCM message carrying our payload.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read pcm: %v", err)
	}
	if msg[0] != msgPCM {
		t.Fatalf("expected PCM (0x02), got 0x%02X", msg[0])
	}
	if len(msg)-1 != len(pcm) {
		t.Fatalf("pcm length = %d, want %d", len(msg)-1, len(pcm))
	}
}

func TestWSCompressedAudioDelivery(t *testing.T) {
	s := NewServer(Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	hello := append([]byte{msgHello}, []byte(`{"audioCodecs":["mp4a.40.39"]}`)...)
	if err := c.WriteMessage(websocket.BinaryMessage, hello); err != nil {
		t.Fatal(err)
	}
	f := capture.AudioFrame{Encoded: []byte{42, 43, 44}, Codec: "mp4a.40.39", Config: []byte{0xf8, 0xe6, 0x50, 0}, SampleRate: 48000, Channels: 2, FramesPerPacket: 480}
	deadline := time.Now().Add(time.Second)
	for !s.SupportsEncodedAudio(f) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.SupportsEncodedAudio(f) {
		t.Fatal("compressed capability handshake failed")
	}
	s.PushAudio(f)
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(msg) < 3 || msg[0] != msgEncodedAudio {
		t.Fatal("received PCM instead of compressed AAC")
	}
	n := int(binary.BigEndian.Uint16(msg[1:]))
	if !bytes.Equal(msg[3+n:], f.Encoded) {
		t.Fatal("AAC payload changed")
	}
}
