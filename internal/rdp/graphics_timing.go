package rdp

import (
	"errors"
	"time"
)

// MS-RDPEGFX 2.2.2.11 packs UTC time of day into START_FRAME. This field
// is neither elapsed milliseconds nor the source's monotonic playback PTS.
func graphicsTimestamp(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	t = t.UTC()
	return uint32(t.Hour())<<22 | uint32(t.Minute())<<16 |
		uint32(t.Second())<<10 | uint32(t.Nanosecond()/int(time.Millisecond))
}

func graphicsStartFrame(id uint32, timestamp time.Time) []byte {
	return gfxPDU(0xb, join(u32(graphicsTimestamp(timestamp)), u32(id)))
}

// The video worker reserves the ID before writing so an immediate client ACK
// cannot race registration. A failed write ends the session.
func (c *client) nextGraphicsFrame() uint32 {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	c.sent++
	if c.sent == 0 {
		c.frameIDsWrapped = true
	}
	if !c.suspendAck {
		if c.pendingFrames == nil {
			c.pendingFrames = make(map[uint32]graphicsPendingFrame)
		}
		c.pendingFrames[c.sent] = graphicsPendingFrame{}
	}
	return c.sent
}

func (c *client) acknowledgeFrame(frame, queueDepth uint32) error {
	return c.acknowledgeFrameAt(frame, queueDepth, time.Now())
}

func (c *client) acknowledgeFrameAt(frame, queueDepth uint32, now time.Time) error {
	c.ackMu.Lock()
	// Serial-number arithmetic also accepts late ACKs across frame-ID rollover.
	if !c.frameIDsWrapped && frame > c.sent || c.frameIDsWrapped && int32(frame-c.sent) > 0 {
		c.ackMu.Unlock()
		return errors.New("acknowledgment for an unsent frame")
	}
	c.graphicsFlow.flushACKBatch(now)
	fresh := !c.ackSeen || int32(frame-c.acknowledged) > 0
	if fresh {
		c.acknowledged = frame
		c.ackSeen = true
	}
	// MS-RDPEGFX 3.2.5.13 acknowledges this ID, not every preceding ID.
	pending, tracked := c.pendingFrames[frame]
	// A duplicate or late ACK must not overwrite current queue feedback.
	if fresh {
		c.graphicsFlow.queueDepth = queueDepth
	}
	if tracked && !pending.ready.IsZero() && queueDepth != 0xffffffff {
		c.graphicsFlow.observeACK(now.Sub(pending.ready), pending.probe && fresh, now)
	}
	delete(c.pendingFrames, frame)
	if !c.suspendAck && queueDepth == 0xffffffff && (tracked || fresh) {
		c.suspendAck = true
		c.ackSuspendBarrier = c.sent
		// Frames sent while ACKs are suspended must not fill the window when
		// the client opts back in. Only subsequent sends need tracking.
		clear(c.pendingFrames)
		c.graphicsFlow.reset()
	} else if c.suspendAck && queueDepth != 0xffffffff && int32(frame-c.ackSuspendBarrier) > 0 {
		c.suspendAck = false
		c.graphicsFlow.queueDepth = queueDepth
	}
	c.ackMu.Unlock()
	select {
	case c.ack <- struct{}{}:
	default:
	}
	return nil
}
