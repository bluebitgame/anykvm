package wsmedia

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"encoding/json"
	"testing"
)

func TestRotationMetadataPrecedesUnmodifiedHEVC(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 8), jpegReady: make(chan struct{}, 1), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	defer c.close()
	data := []byte{0, 0, 0, 1, 40, 1, 42}
	for _, rotation := range []int{0, 90, 180, 270, 0} {
		s.PushFrame(bridge.Frame{Format: capture.FormatHEVC, Data: data, Width: 1328, Height: 2896, Rotation: rotation})
		msg := <-c.sendCh
		var meta metadata
		if msg.kind() != msgMetadata {
			t.Fatal("rotation metadata did not precede video")
		}
		if err := json.Unmarshal(msg.data[1:], &meta); err != nil {
			t.Fatal(err)
		}
		msg.release()
		if meta.Width != 1328 || meta.Height != 2896 || meta.Rotation != rotation {
			t.Fatalf("incorrect geometry: %+v", meta)
		}
		video := <-c.sendCh
		if video.kind() != msgHEVC || string(video.data) != string(data) {
			t.Fatal("rotation modified the HEVC payload")
		}
		video.release()
	}
	// An unchanged orientation does not send redundant metadata each frame.
	s.PushFrame(bridge.Frame{Format: capture.FormatHEVC, Data: data, Width: 1328, Height: 2896})
	video := <-c.sendCh
	defer video.release()
	if video.kind() != msgHEVC || len(c.sendCh) != 0 {
		t.Fatal("unchanged rotation resent metadata")
	}
}

func TestH264BufferPolicyPrecedesVideoWithoutGeometryChange(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{sendCh: make(chan *outboundMessage, 8), jpegReady: make(chan struct{}, 1), done: make(chan struct{})}
	s.clients[c] = struct{}{}
	defer c.close()
	for _, enabled := range []bool{false, true, false} {
		s.PushFrame(bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 48,
			BufferH264Presentation: enabled, Data: []byte{0, 0, 0, 1, 0x65}})
		msg := <-c.sendCh
		var meta metadata
		if msg.kind() != msgMetadata {
			t.Fatal("presentation policy must precede video even with unchanged geometry")
		}
		if err := json.Unmarshal(msg.data[1:], &meta); err != nil {
			t.Fatal(err)
		}
		msg.release()
		if meta.BufferH264 != enabled {
			t.Fatalf("buffer policy = %v, want %v", meta.BufferH264, enabled)
		}
		video := <-c.sendCh
		if video.kind() != msgH264 {
			t.Fatal("missing H.264 after policy")
		}
		video.release()
	}
}
