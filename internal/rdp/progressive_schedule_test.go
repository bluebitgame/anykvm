package rdp

import (
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

type progressiveFrameConn struct {
	displayActivationConn
	finished chan uint32
}

func (c progressiveFrameConn) Write(p []byte) (int, error) {
	// Uncompressed GFX frame-end is the final PDU of the last VC fragment.
	if len(p) >= 12 {
		end := p[len(p)-12:]
		if le.Uint16(end) == 0xc && le.Uint32(end[4:]) == 12 {
			c.finished <- le.Uint32(end[8:])
		}
	}
	return len(p), nil
}

func TestProgressiveRefinementWakesOnACK(t *testing.T) {
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
	frames := make(chan uint32, 4)
	c := channelTestClient()
	c.conn, c.codec, c.sent = progressiveFrameConn{finished: frames}, codecProgressive, 0
	c.frames = make(chan bridge.Frame, 1)
	// Startup keeps one slot available for fresh captures; background work
	// resumes on ACK without an idle timer.
	c.s.cfg.ProgressiveRefinement = 2
	// More than 64 tiles ensure at least two background frames are needed.
	c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 65 * 64, Data: progressivePixels(64, 65*64, "gradient")}
	finished := make(chan error, 1)
	go func() { finished <- c.bitmapLoop() }()
	t.Cleanup(func() {
		close(c.done)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	})
	next := func(want uint32) {
		t.Helper()
		select {
		case id := <-frames:
			if id != want {
				t.Fatalf("frame %d, want %d", id, want)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("refinement waited for an idle/cadence timer instead of an ACK")
		}
	}
	next(1)
	if err := c.handleGFX(gfxPDU(0xd, join(u32(0), u32(1), u32(1)))); err != nil {
		t.Fatal(err)
	}
	next(2)
	if err := c.handleGFX(gfxPDU(0xd, join(u32(0), u32(2), u32(2)))); err != nil {
		t.Fatal(err)
	}
	next(3)
}

func TestProgressiveRefinementWakesAfterIdleRecovery(t *testing.T) {
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
	frames := make(chan uint32, 4)
	c := channelTestClient()
	c.conn, c.codec, c.sent = progressiveFrameConn{finished: frames}, codecProgressive, 0
	c.frames = make(chan bridge.Frame, 1)
	c.s.cfg.ProgressiveRefinement = 2
	c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64, Data: progressivePixels(64, 64, "gradient")}
	finished := make(chan error, 1)
	go func() { finished <- c.bitmapLoop() }()
	t.Cleanup(func() {
		close(c.done)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	})
	next := func(want uint32) {
		t.Helper()
		select {
		case id := <-frames:
			if id != want {
				t.Fatalf("frame %d, want %d", id, want)
			}
		case <-time.After(time.Second):
			t.Fatal("idle refinement did not wake on recovery/ACK")
		}
	}
	next(1)
	c.ackMu.Lock()
	deadline := time.Now().Add(60 * time.Millisecond)
	c.graphicsFlow.rtt, c.graphicsFlow.baseline = 50*time.Millisecond, 50*time.Millisecond
	c.graphicsFlow.recoverUntil = deadline
	c.ackMu.Unlock()
	if err := c.acknowledgeFrame(1, 0); err != nil {
		t.Fatal(err)
	}
	// No capture or ACK arrives during the hold. The one-shot backoff wake
	// must permit 75%, then its ACK must permit the remaining 100% pass.
	next(2)
	if time.Now().Before(deadline) {
		t.Fatal("UPGRADE consumed the fresh-only congestion window")
	}
	if err := c.acknowledgeFrame(2, 0); err != nil {
		t.Fatal(err)
	}
	next(3)
}
