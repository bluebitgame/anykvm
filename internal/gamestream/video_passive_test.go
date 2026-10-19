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

func passiveVideoSession(t *testing.T) (*streamSession, *net.UDPConn, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { socket.Close() })
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	restarts := make(chan struct{}, 8)
	s := &Server{videoSocket: socket, cfg: Config{
		OnVideoRestartAllowed: func() bool { return false },
		OnVideoRestart:        func() error { restarts <- struct{}{}; return nil },
	}}
	x := &streamSession{server: s, ctx: ctx, cancel: cancel, started: true,
		videoPeer: peer.LocalAddr().(*net.UDPAddr), frames: make(chan bridge.Frame, 1),
		keyframe: make(chan struct{}, 1), options: streamOptions{
			Codec: "h264", Width: 1920, Height: 1080, FPS: 60, PacketSize: 1024, MinFEC: 2,
		}}
	s.session = x
	return x, peer, cancel, restarts
}

func TestPassiveSourceKeyframeRequestKeepsConnection(t *testing.T) {
	t.Parallel()
	x, peer, cancel, restarts := passiveVideoSession(t)
	requested := make(chan struct{}, 1)
	x.server.cfg.OnVideoKeyframe = func() (bool, error) {
		requested <- struct{}{}
		return false, nil
	}
	epoch := time.Now().Add(-time.Second)
	frame := func(nal byte, elapsed time.Duration) bridge.Frame {
		return bridge.Frame{Format: capture.FormatH264, Width: 500, Height: 1080,
			CapturedAt: epoch.Add(elapsed), Data: []byte{0, 0, 0, 1, nal, 0xab}}
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := x.streamEncodedVideo(frame(0x65, 100*time.Millisecond), &videoPacketizer{}, epoch)
		done <- err
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	receive := func(want bridge.Frame, ticks uint32) {
		t.Helper()
		peer.SetReadDeadline(time.Now().Add(time.Second))
		var packet [2048]byte
		for {
			n, _, err := peer.ReadFromUDP(packet[:])
			if err != nil {
				t.Fatal(err)
			}
			if n < 40 {
				t.Fatal("short video packet")
			}
			info := binary.LittleEndian.Uint32(packet[28:])
			if info>>12&1023 >= info>>22 {
				continue
			} // FEC parity.
			if binary.BigEndian.Uint32(packet[4:8]) != ticks || n < 40+len(want.Data) || !bytes.Equal(packet[40:40+len(want.Data)], want.Data) {
				t.Fatal("source timestamp or payload changed")
			}
			return
		}
	}
	receive(frame(0x65, 100*time.Millisecond), 9000)
	x.keyframe <- struct{}{}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("client IDR request was not processed")
	}
	x.frames <- frame(0x41, 150*time.Millisecond)
	// Exceeds the legacy restart and native-feedback deadlines.
	select {
	case <-restarts:
		t.Fatal("passive source was disconnected for IDR recovery")
	case err := <-done:
		t.Fatalf("stream ended before recovery: %v", err)
	case <-time.After(3100 * time.Millisecond):
	}
	x.frames <- frame(0x65, 200*time.Millisecond)
	receive(frame(0x65, 200*time.Millisecond), 18000)
	x.frames <- frame(0x41, 220*time.Millisecond)
	receive(frame(0x41, 220*time.Millisecond), 19800)
}

func TestPassiveSourceWaitsForNativeFirstFrame(t *testing.T) {
	t.Parallel()
	x, peer, cancel, _ := passiveVideoSession(t)
	x.keyframe <- struct{}{} // Startup IDR feedback predates the native first frame.
	done := make(chan struct{})
	go func() { defer close(done); x.videoLoop() }()
	defer func() { cancel(); <-done }()
	// The former two-second fallback would send a locally encoded black frame.
	peer.SetReadDeadline(time.Now().Add(2600 * time.Millisecond))
	var packet [2048]byte
	if _, _, err := peer.ReadFromUDP(packet[:]); err == nil {
		t.Fatal("placeholder video was sent before the sender connected")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
	first := bridge.Frame{Format: capture.FormatH264, Width: 500, Height: 1080,
		CapturedAt: time.Now(), Data: []byte{0, 0, 0, 1, 0x65, 0xab}}
	x.frames <- first
	peer.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := peer.ReadFromUDP(packet[:])
	if err != nil {
		t.Fatal(err)
	}
	if n < 40+len(first.Data) || !bytes.Equal(packet[40:40+len(first.Data)], first.Data) {
		t.Fatal("first native IDR was not passed through")
	}
	// The queued startup request is satisfied by that first IDR. A following
	// delta must keep flowing instead of waiting for another source keyframe.
	peer.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	for {
		if _, _, err := peer.ReadFromUDP(packet[:]); err != nil {
			break
		}
	}
	delta := first
	delta.Data = []byte{0, 0, 0, 1, 0x41, 0xcd}
	x.frames <- delta
	peer.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err = peer.ReadFromUDP(packet[:])
	if err != nil || n < 40+len(delta.Data) || !bytes.Equal(packet[40:40+len(delta.Data)], delta.Data) {
		t.Fatalf("startup request stopped delta forwarding: %v", err)
	}
}

func TestPassiveSourceRetainsIDRUntilVideoPeerReady(t *testing.T) {
	t.Parallel()
	x, peer, cancel, restarts := passiveVideoSession(t)
	x.videoPeer = nil
	first := bridge.Frame{Format: capture.FormatH264, Width: 500, Height: 1080,
		CapturedAt: time.Now(), Data: []byte{0, 0, 0, 1, 0x65, 0xab}}
	done := make(chan error, 1)
	go func() { _, err := x.streamEncodedVideo(first, &videoPacketizer{}, time.Now()); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	x.keyframe <- struct{}{}
	time.Sleep(200 * time.Millisecond)
	x.mu.Lock()
	x.videoPeer = peer.LocalAddr().(*net.UDPAddr)
	x.mu.Unlock()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	var packet [2048]byte
	n, _, err := peer.ReadFromUDP(packet[:])
	if err != nil || n < 40+len(first.Data) || !bytes.Equal(packet[40:40+len(first.Data)], first.Data) {
		t.Fatalf("IDR lost before video endpoint became ready: %v", err)
	}
	select {
	case <-restarts:
		t.Fatal("sender restarted during endpoint setup")
	default:
	}
}

// A phone connected before PLAY delivers queued IDR/P pictures with old PTS.
// Test the full video loop and actual UDP output: no timestamp underflow, no
// compressed dependency loss, and no burst into Moonlight's 15-picture queue.
func TestPassiveSourceBufferedFramesHavePacingAndPositiveClock(t *testing.T) {
	x, peer, cancel, _ := passiveVideoSession(t)
	done := make(chan struct{})
	go func() { defer close(done); x.videoLoop() }()
	defer func() { cancel(); <-done }()
	const count = 24
	base := time.Now().Add(-2 * time.Second)
	frame := func(i int) bridge.Frame {
		nal := byte(0x41)
		if i == 0 {
			nal = 0x65
		}
		return bridge.Frame{Format: capture.FormatH264, Width: 500, Height: 1080,
			CapturedAt: base.Add(time.Duration(i) * time.Second / 60),
			Data:       []byte{0, 0, 0, 1, nal, byte(i)}}
	}
	producer := make(chan struct{})
	go func() {
		defer close(producer)
		for i := 0; i < count; i++ {
			select {
			case x.frames <- frame(i):
			case <-x.ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-producer }()
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var packet [2048]byte
	var firstArrival time.Time
	for i := 0; i < count; {
		n, _, err := peer.ReadFromUDP(packet[:])
		if err != nil || n < 46 {
			t.Fatalf("UDP frame %d: %v (%d bytes)", i, err, n)
		}
		info := binary.LittleEndian.Uint32(packet[28:])
		if info>>12&1023 >= info>>22 {
			continue
		}
		if i == 0 {
			firstArrival = time.Now()
		}
		if got := binary.BigEndian.Uint32(packet[4:]); got != uint32(90+i*1500) {
			t.Fatalf("frame %d: RTP timestamp %d, want %d", i, got, 90+i*1500)
		}
		if binary.LittleEndian.Uint32(packet[20:]) != uint32(i+1) || !bytes.Equal(packet[40:46], frame(i).Data) {
			t.Fatalf("frame %d lost a compressed dependency", i)
		}
		i++
	}
	if elapsed := time.Since(firstArrival); elapsed < 300*time.Millisecond {
		t.Fatalf("buffered video flooded client decode queue: %v", elapsed)
	}
}
