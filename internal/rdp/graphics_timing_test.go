package rdp

import (
	"bytes"
	"math"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestGraphicsTimestampUTCFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		t    time.Time
		want uint32
	}{
		{"unavailable", time.Time{}, 0},
		{"midnight", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), 0},
		{"all fields", time.Date(2026, 10, 23, 23, 59, 58, 999999999, time.UTC), 23<<22 | 59<<16 | 58<<10 | 999},
		{"timezone", time.Date(2026, 10, 23, 1, 2, 3, 456000000, time.FixedZone("UTC+8", 8*3600)), 17<<22 | 2<<16 | 3<<10 | 456},
		{"next day", time.Date(2026, 10, 24, 0, 0, 0, 1000000, time.UTC), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphicsTimestamp(tc.t); got != tc.want {
				t.Fatalf("timestamp=%#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestGraphicsStartFrameWireTimestamp(t *testing.T) {
	stamp := time.Date(2026, 10, 23, 12, 34, 56, 789000000, time.UTC)
	want := join(u16(0xb), u16(0), u32(16), u32(12<<22|34<<16|56<<10|789), u32(42))
	if got := graphicsStartFrame(42, stamp); !bytes.Equal(got, want) {
		t.Fatalf("START_FRAME=%x, want %x", got, want)
	}
	if got := graphicsFrame([]byte{1}, 64, 64, 42, stamp); !bytes.HasPrefix(got, want) {
		t.Fatalf("AVC420 START_FRAME=%x", got)
	}
	// Background UPGRADE frames must use the same wire clock as capture frames.
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
	e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer e.close()
	pixels := progressivePixels(64, 64, "gradient")
	if _, err := e.encode(pixels, 64, 64, []bitmapRect{{0, 0, 64, 64}}, 41); err != nil {
		t.Fatal(err)
	}
	conn := &dvcRecordingConn{}
	c := channelTestClient()
	c.conn, c.sent = conn, 41
	if n, _, _, _, err := c.sendProgressiveRefinement(e, stamp); err != nil || n != 1 {
		t.Fatalf("refinement tiles=%d error=%v", n, err)
	}
	messages := recordedDVC(t, conn)
	if len(messages) != 1 || !bytes.Contains(messages[0], want) {
		t.Fatalf("UPGRADE missing UTC START_FRAME: %x", messages)
	}
}

func TestGraphicsACKTracksIndividualFrames(t *testing.T) {
	c := channelTestClient()
	c.sent = 0
	c.nextGraphicsFrame()
	c.nextGraphicsFrame()
	ack := func(id uint32) {
		t.Helper()
		if err := c.handleGFX(gfxPDU(0xd, join(u32(0), u32(id), u32(id)))); err != nil {
			t.Fatal(err)
		}
	}
	ack(2) // ACK 2 does not implicitly acknowledge frame 1.
	if len(c.pendingFrames) != 1 || c.frameWindowFull() {
		t.Fatal("out-of-order ACK did not release exactly one slot")
	}
	c.nextGraphicsFrame()
	ack(2) // A duplicate ACK must not release frame 1 or 3.
	if !c.frameWindowFull() {
		t.Fatal("duplicate ACK released an unrelated frame")
	}
	ack(1)
	if c.frameWindowFull() || len(c.pendingFrames) != 1 {
		t.Fatal("late ACK failed to release its slot")
	}
	ack(3)
	if len(c.pendingFrames) != 0 {
		t.Fatal("ACKs left a phantom outstanding frame")
	}
}

func TestGraphicsACKSuspendResume(t *testing.T) {
	c := channelTestClient()
	c.sent = 0
	c.nextGraphicsFrame()
	c.nextGraphicsFrame()
	if err := c.acknowledgeFrame(1, math.MaxUint32); err != nil {
		t.Fatal(err)
	}
	if !c.suspendAck || c.frameWindowFull() || len(c.pendingFrames) != 0 {
		t.Fatal("SUSPEND did not clear outstanding frames")
	}
	for range 100 {
		c.nextGraphicsFrame()
	}
	if len(c.pendingFrames) != 0 {
		t.Fatal("suspended sends accumulated pending ACKs")
	}
	// Resume with an older ID: frames sent during SUSPEND need no ACK.
	if err := c.acknowledgeFrame(c.sent-1, 0); err != nil {
		t.Fatal(err)
	}
	if c.suspendAck || c.frameWindowFull() {
		t.Fatal("resume blocked on frames sent while ACKs were suspended")
	}
	c.nextGraphicsFrame()
	if c.frameWindowFull() {
		t.Fatal("resume did not restore the full two-frame window")
	}
	c.nextGraphicsFrame()
	if !c.frameWindowFull() {
		t.Fatal("resume stopped tracking new frames")
	}
}

func TestGraphicsACKFrameIDRollover(t *testing.T) {
	c := &client{sent: math.MaxUint32 - 1}
	before, after := c.nextGraphicsFrame(), c.nextGraphicsFrame()
	if before != math.MaxUint32 || after != 0 || !c.frameWindowFull() {
		t.Fatal("frame IDs failed to wrap")
	}
	if err := c.acknowledgeFrame(after, 0); err != nil || len(c.pendingFrames) != 1 {
		t.Fatal("wrapped ACK did not release its slot", err)
	}
	if err := c.acknowledgeFrame(before, 0); err != nil || len(c.pendingFrames) != 0 {
		t.Fatal("late pre-wrap ACK rejected", err)
	}
	if err := c.acknowledgeFrame(1, 0); err == nil {
		t.Fatal("accepted an unsent post-wrap frame")
	}
	fresh := &client{sent: 2}
	if err := fresh.acknowledgeFrame(math.MaxUint32, 0); err == nil {
		t.Fatal("accepted an unsent frame before the first rollover")
	}
}

func TestGraphicsRefreshRepaintsIdleSource(t *testing.T) {
	for _, codec := range []displayCodec{codecClear, codecPlanar, codecProgressive} {
		t.Run(codec.String(), func(t *testing.T) {
			t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
			frames := make(chan uint32, 4)
			c := channelTestClient()
			c.conn, c.codec, c.sent = progressiveFrameConn{finished: frames}, codec, 0
			c.frames = make(chan bridge.Frame, 1)
			c.s.active = c
			c.s.PushFrame(bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64, Data: progressivePixels(64, 64, "gradient")})
			result := make(chan error, 1)
			go func() { result <- c.bitmapLoop() }()
			t.Cleanup(func() {
				c.close()
				if err := <-result; err != nil {
					t.Error(err)
				}
			})
			waitFrame := func(want uint32) {
				t.Helper()
				select {
				case id := <-frames:
					if id != want {
						t.Fatalf("frame=%d, want %d", id, want)
					}
				case <-time.After(time.Second):
					t.Fatal("refresh waited for new capture damage")
				}
			}
			waitFrame(1)
			// Exercise the actual client Refresh Rect PDU, with no new capture.
			refresh := join([]byte{1, 0, 0, 0}, u16(0), u16(0), u16(63), u16(63))
			if err := c.handleShare(shareData(33, refresh)); err != nil {
				t.Fatal(err)
			}
			waitFrame(2)
		})
	}
}
