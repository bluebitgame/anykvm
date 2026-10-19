//go:build cgo

package airplay

import (
	"bytes"
	"net"
	"os"
	"testing"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

type captureSink struct{ frames chan bridge.Frame }

func (s *captureSink) PushFrame(f bridge.Frame)     { s.frames <- f.Retain() }
func (*captureSink) UpdateFramebufferSize(int, int) {}
func (*captureSink) CloseClients()                  {}

// Use the actual bridge selection and a persistent decoder across IDR/P input.
// GameStream needs decoded pictures, while switching to Web restores passthrough.
func TestGameStreamDecodedCaptureAndWebPassthrough(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	encoder, err := avmedia.NewEncoder(avmedia.VideoOptions{Width: 320, Height: 240,
		FPS: 60, Bitrate: 2000, Codec: "libx264"})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	d, err := NewDevice("airplay:Bridge-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil) // No Bonjour registration during tests.
	if err := d.StartReceiver(1920, 1080, 60); err != nil {
		t.Fatal(err)
	}
	defer d.ShutdownReceiver()
	sink := &captureSink{frames: make(chan bridge.Frame, 4)}
	b := bridge.NewBridge([]*bridge.MachineSession{{Name: "AirPlay", Capture: d,
		CapW: 1920, CapH: 1080, FPS: 60}}, "alt", 0, 0, 0)
	b.SetGameStream(sink, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Codec: "h264",
		Width: 1920, Height: 1080, FPS: 60, Bitrate: 16000}); err != nil {
		t.Fatal(err)
	}
	if !b.GameStreamCanEncodePlaceholder() || b.GameStreamCanRestartForKeyframe() {
		t.Fatal("decoded AirPlay must allow waiting video without capture restart")
	}
	owner := &pairing{}
	d.receiver.mu.Lock()
	d.receiver.owner = owner
	d.receiver.mu.Unlock()
	var firstAU []byte
	base := time.Now()
	for i := 0; i < 4; i++ {
		pixels := make([]byte, 320*240*4)
		for off := 0; off < len(pixels); off += 4 {
			pixels[off+3] = 255
			pixels[off+2-i%2] = 255 // Alternating red and green BGRA.
		}
		au, err := encoder.Encode(pixels, 320, 240, "bgra", i == 0)
		if err != nil || len(au) == 0 {
			t.Fatalf("source encoding: %v", err)
		}
		if i == 0 {
			firstAU = append([]byte(nil), au...)
		}
		pts := base.Add(time.Duration(i) * time.Second / 60)
		d.receiver.frames <- receivedFrame{owner: owner, frame: &capture.Frame{
			Format: capture.FormatH264, Width: 320, Height: 240, Data: au, CapturedAt: pts}}
		select {
		case f := <-sink.frames:
			if f.Format != capture.FormatRaw || f.BufferH264Presentation || !f.PreserveSource || !f.CapturedAt.Equal(pts) || f.Width != 320 || f.Height != 240 {
				t.Errorf("picture %d: format=%v preserve=%t PTS=%v want=%v dimensions=%dx%d", i, f.Format, f.PreserveSource, f.CapturedAt, pts, f.Width, f.Height)
				f.Release()
				t.Fatal("GameStream did not receive decoded native geometry and PTS")
			}
			color := append([]byte(nil), f.Data[:3]...)
			f.Release()
			if color[2-i%2] < 200 || color[1+i%2] > 40 {
				t.Fatalf("dependent source frame %d frozen or corrupt: %v", i, color)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("missing decoded source picture")
		}
	}
	b.StopGameStream()
	b.SetGameStream(nil, nil)
	b.SetWS(sink, nil)
	b.ClaimWS()
	b.SetVideoWantsH264(true)
	b.OnClientConnected()
	defer b.OnClientDisconnected()
	owner = &pairing{}
	d.receiver.mu.Lock()
	d.receiver.owner = owner
	d.receiver.mu.Unlock()
	d.receiver.frames <- receivedFrame{owner: owner, frame: &capture.Frame{
		Format: capture.FormatH264, Width: 320, Height: 240, Data: firstAU, CapturedAt: base}}
	select {
	case f := <-sink.frames:
		defer f.Release()
		if f.Format != capture.FormatH264 || !f.BufferH264Presentation || !bytes.Equal(f.Data, firstAU) || !f.CapturedAt.Equal(base) {
			t.Fatal("GameStream compatibility mode leaked into Web passthrough")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing Web passthrough picture")
	}
}
