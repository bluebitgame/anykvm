//go:build cgo

package gamestream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestDirectH264ForwardingAndRestart(t *testing.T) {
	t.Run("restart fallback", func(t *testing.T) { testDirectVideoRefresh(t, false, "h264") })
	t.Run("native keyframe", func(t *testing.T) { testDirectVideoRefresh(t, true, "h264") })
}

func testDirectVideoRefresh(t *testing.T, native bool, codec string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restarted := make(chan struct{}, 1)
	s := &Server{videoSocket: socket, cfg: Config{OnVideoRestart: func() error { restarted <- struct{}{}; return nil }}}
	x := &streamSession{server: s, ctx: ctx, started: true, videoPeer: peer.LocalAddr().(*net.UDPAddr), frames: make(chan bridge.Frame, 1), keyframe: make(chan struct{}), options: streamOptions{Codec: codec, Width: 640, Height: 360, FPS: 30, Bitrate: 1000, PacketSize: 1024, MinFEC: 2}}
	s.session = x
	requested := make(chan struct{}, 2)
	if native {
		s.cfg.OnVideoKeyframe = func() (bool, error) { requested <- struct{}{}; return true, nil }
	}
	epoch := time.Now().Add(-time.Second)
	frame := func(nal byte) bridge.Frame {
		data := []byte{0, 0, 0, 1, nal, 0xab, 0xcd}
		format := capture.FormatH264
		if codec == "hevc" {
			format = capture.FormatHEVC
			if nal == 0x65 {
				data = hevcNALs(32, 33, 34, 19)
			} else {
				data = hevcNALs(1)
			}
		}
		return bridge.Frame{CapturedAt: epoch.Add(123 * time.Millisecond), Format: format, Width: 320, Height: 240, SourceWidth: 320, SourceHeight: 200, Data: data}
	}
	done := make(chan error, 1)
	go func() { _, err := x.streamEncodedVideo(frame(0x65), &videoPacketizer{}, epoch); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	// The payload must be forwarded byte-for-byte; no second encoder is set up.
	receive := func(want bridge.Frame, index uint32) {
		t.Helper()
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 2048)
		for {
			n, _, err := peer.ReadFromUDP(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n < 40 {
				t.Fatal("short packet")
			}
			if got := binary.BigEndian.Uint32(buf[4:8]); got != 11070 {
				t.Fatalf("RTP timestamp=%d, want capture time 11070", got)
			}
			info := binary.LittleEndian.Uint32(buf[28:])
			if info>>12&1023 >= info>>22 {
				continue
			} // parity
			if got := binary.LittleEndian.Uint32(buf[20:]); got != index {
				t.Fatalf("frame index %d, want %d", got, index)
			}
			if !bytes.Equal(buf[40:40+len(want.Data)], want.Data) {
				t.Fatal("capture payload changed")
			}
			break
		}
	}
	receive(frame(0x65), 1)
	s.PushFrame(frame(0x41))
	receive(frame(0x41), 2)
	x.keyframe <- struct{}{} // Unbuffered: the refresh is consumed before the IDR.
	if native {
		select {
		case <-requested:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("native IDR request was delayed")
		}
	}
	s.PushFrame(frame(0x65))
	receive(frame(0x65), 3)
	select {
	case <-restarted:
		t.Fatal("natural IDR did not satisfy pending refresh")
	case <-time.After(1200 * time.Millisecond):
	}
	x.keyframe <- struct{}{}
	if native {
		select {
		case <-requested:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("native IDR request was delayed")
		}
		select {
		case <-restarted:
			t.Fatal("native capture restarted")
		default:
		}
	} else {
		select {
		case <-restarted:
		case <-time.After(3 * time.Second):
			t.Fatal("capture was not restarted")
		}
	}
	s.PushFrame(frame(0x41)) // discard P-frames until the new encoder's IDR
	s.PushFrame(frame(0x65))
	receive(frame(0x65), 4)
	x.input.geometryMu.Lock()
	sw, sh := x.input.sourceW, x.input.sourceH
	ow, oh := x.input.outputW, x.input.outputH
	x.input.geometryMu.Unlock()
	if ow != 320 || oh != 240 {
		t.Fatalf("client request replaced actual encoded geometry: %dx%d", ow, oh)
	}
	if sw != 320 || sh != 200 {
		t.Fatalf("lost source geometry: %dx%d", sw, sh)
	}
}

func TestEncodedCaptureBackpressureStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{ctx: ctx, started: true, frames: make(chan bridge.Frame, 1)}
	s := &Server{session: x}
	f := bridge.Frame{Format: capture.FormatH264, Width: 320, Height: 240, Data: []byte{1}}
	s.PushFrame(f)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { s.PushFrameWithStop(f, stop); close(done) }()
	select {
	case <-done:
		t.Fatal("encoded frame was dropped instead of waiting")
	case <-time.After(20 * time.Millisecond):
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("capture stop did not unblock producer")
	}
	if got := <-x.frames; !bytes.Equal(got.Data, f.Data) {
		t.Fatal("queued frame was replaced")
	}
}

func TestCaptureRTPTimestamp(t *testing.T) {
	epoch := time.Now()
	first := epoch.Add(time.Second)
	// An extra 100 ms of processing must not stretch a 20 ms capture interval.
	a := captureRTPTimestamp(first, epoch, first.Add(10*time.Millisecond))
	b := captureRTPTimestamp(first.Add(20*time.Millisecond), epoch, first.Add(130*time.Millisecond))
	if b-a != 1800 {
		t.Fatalf("capture interval distorted: %d", b-a)
	}
	if got := captureRTPTimestamp(time.Time{}, epoch, epoch.Add(time.Second)); got != 90000 {
		t.Fatalf("fallback timestamp=%d", got)
	}
	if got := captureRTPTimestamp(epoch.Add(-time.Second), epoch, epoch); got != uint32(4294877296) {
		t.Fatalf("pre-epoch wrap=%d", got)
	}
}

func TestDirectHEVCForwardingAndRestart(t *testing.T) {
	t.Run("restart fallback", func(t *testing.T) { testDirectVideoRefresh(t, false, "hevc") })
	t.Run("native keyframe", func(t *testing.T) { testDirectVideoRefresh(t, true, "hevc") })
}

func TestFractionalCaptureRTPTimestamp(t *testing.T) {
	epoch := time.Now()
	for frame := int64(0); frame < 30000; frame++ {
		captured := epoch.Add(time.Duration(frame * 1001 * int64(time.Second) / 30000))
		got := captureRTPTimestamp(captured, epoch, captured.Add(time.Second))
		if got != uint32(frame*3003) {
			t.Fatalf("frame %d: timestamp %d", frame, got)
		}
	}
}
