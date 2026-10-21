package gamestream

import (
	"anykvm/internal/netstats"
	"context"
	"net"
	"testing"
	"time"
)

func TestSourceFramePacingBufferedPTS(t *testing.T) {
	start := time.Unix(100, 0)
	capture := start.Add(-10 * time.Second)
	p := sourceFramePacer{fps: 60}
	for i := 0; i < 120; i++ {
		pts := capture.Add(time.Duration(i) * time.Second / 60)
		due := p.reserve(pts, start)
		want := start.Add(time.Duration(i) * time.Second / 60)
		if due.Sub(want) > time.Microsecond || due.Before(want.Add(-time.Microsecond)) {
			t.Fatalf("frame %d: buffered PTS due %v, want %v", i, due, want)
		}
	}
}

func TestSourceFramePacingMissingPTSAndQuietResume(t *testing.T) {
	start := time.Unix(100, 0)
	p := sourceFramePacer{fps: 60}
	for i := 0; i < 20; i++ {
		if due := p.reserve(time.Time{}, start); due.Sub(start) < time.Duration(i)*(time.Second/60) {
			t.Fatalf("missing PTS allowed burst at frame %d", i)
		}
	}
	now := start.Add(5 * time.Second)
	if due := p.reserve(time.Time{}, now); !due.Equal(now) {
		t.Fatal("quiet resume waited for expired pacing debt")
	}
	p = sourceFramePacer{fps: 60}
	p.reserve(start, start)
	if due := p.reserve(start.Add(20*time.Second), now); !due.Equal(now) {
		t.Fatal("source clock jump scheduled far into the future")
	}
}

func TestVideoPacingAcrossFrames(t *testing.T) {
	p := &videoPacer{}
	now := time.Unix(100, 0)
	start := p.reserve(now, 200000)
	if !start.Equal(now) || p.duration(200000) != 2*time.Millisecond {
		t.Fatal("incorrect 800 Mbps burst budget")
	}
	next := p.reserve(now, 100000)
	if !next.Equal(now.Add(2 * time.Millisecond)) {
		t.Fatal("next frame ignored previous tail")
	}
	idle := now.Add(time.Second)
	if !p.reserve(idle, 1000).Equal(idle) {
		t.Fatal("idle stream accumulated pacing debt")
	}
	p = &videoPacer{mbps: 100}
	if p.duration(200000) != 16*time.Millisecond {
		t.Fatal("configured burst ceiling ignored")
	}
}

func TestLargeVideoFrameDoesNotStallFollowingFrames(t *testing.T) {
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			if _, _, err := peer.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	defer func() { peer.Close(); <-done }()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	packets := make([][]byte, 200)
	for i := range packets {
		packets[i] = make([]byte, 1024)
	}
	// The previous per-frame average-rate cap took >600ms for this IDR.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	before := netstats.Total()
	pacer := &videoPacer{}
	start := time.Now()
	if err := pacer.send(ctx, sender, peer.LocalAddr().(*net.UDPAddr), packets); err != nil {
		t.Fatalf("large frame stalled stream: %v", err)
	}
	t.Logf("200-packet frame sent in %v", time.Since(start))
	if n := netstats.Total() - before; n != 200*1024 {
		t.Fatalf("datagram bytes not counted: %d", n)
	}
	cancel()
	if err := pacer.send(ctx, sender, peer.LocalAddr().(*net.UDPAddr), packets); err != context.Canceled {
		t.Fatalf("sender ignored cancellation: %v", err)
	}
	if netstats.Total()-before != 200*1024 {
		t.Fatal("cancelled packets counted as transmitted")
	}
}

// At 1 Mbps a 32 KiB frame must be spread over ~262 ms. The old inner
// 64 KiB batch sent the whole frame immediately, bypassing the configured cap.
func TestLowRatePacingLimitsWithinFrameBurst(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	packets := make([][]byte, 32)
	for i := range packets {
		packets[i] = make([]byte, 1024)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := videoPacer{mbps: 1}
	start := time.Now()
	if err := p.send(ctx, sender, receiver.LocalAddr().(*net.UDPAddr), packets); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 240*time.Millisecond {
		t.Fatalf("burst bypassed ceiling: %v", elapsed)
	}
}
