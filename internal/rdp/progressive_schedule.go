package rdp

import "time"

// A background frame buffers at most 64 tiles, targeting 256KiB, and checks a 2ms
// encoding budget after each eight-tile batch. A new capture or refresh discards unwritten
// work. No precision state advances until the bounded frame has been written.
func (c *client) sendProgressiveRefinement(e *progressiveEncoder, timestamp time.Time) (tiles, payload int, encodeTime, sendTime time.Duration, err error) {
	if c.refinementPreempted(nil) {
		return
	}
	rects := e.pendingRects(64)
	if len(rects) == 0 {
		return
	}
	id := c.nextGraphicsFrame()
	written := false
	defer func() {
		if !written {
			// Never wait for an ACK for a discarded, unwritten reservation.
			// Keep IDs monotonic: suspension boundaries may already include it.
			c.ackMu.Lock()
			delete(c.pendingFrames, id)
			c.ackMu.Unlock()
		}
	}()
	batch := graphicsStartFrame(id, timestamp)
	prepared := 0
	for i := 0; i < len(rects); {
		if c.refinementPreempted(&id) {
			return
		}
		end := progressiveBatchEnd(rects, i)
		t := time.Now()
		var encoded []byte
		encoded, err = e.prepareUpgrade(rects[i:end], id)
		encodeTime += time.Since(t)
		if err != nil {
			return
		}
		if prepared != 0 && len(batch)+graphicsProgressiveHeaderSize+len(encoded)+12 > 256<<10 {
			break
		}
		packing := time.Now()
		batch = appendGraphicsProgressive(batch, encoded)
		encodeTime += time.Since(packing)
		prepared = end
		i = end
		if encodeTime >= 2*time.Millisecond {
			break
		}
	}
	if c.refinementPreempted(&id) {
		return
	}
	batch = append(batch, gfxPDU(0xc, u32(id))...)
	t := time.Now()
	c.recordGraphicsWrite(id, len(batch), true, t)
	err = c.sendGFX(batch)
	sendTime = time.Since(t)
	if err == nil {
		written = true
		e.commitUpgrade(rects[:prepared])
		tiles, payload = prepared, len(batch)
	}
	return
}

func (c *client) refinementPreempted(reserved *uint32) bool {
	select {
	case <-c.done:
		return true
	default:
	}
	// PushFrame replaces the queued independent capture while holding this
	// lock. Do not mistake the drain/enqueue gap for an idle desktop.
	c.framePushMu.Lock()
	fresh := len(c.frames) != 0 || c.reset.Load()
	c.framePushMu.Unlock()
	if fresh {
		return true
	}
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	return c.graphicsWindowFullExcept(time.Now(), true, reserved)
}

// The last ACK may empty a congestion flight before its recovery hold expires.
// Wake once at that deadline so idle refinements do not wait for a new capture
// or the statistics ticker. This is a backoff expiry, not a frame cadence.
func (c *client) refinementRetryDelay(now time.Time) time.Duration {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	if c.suspendAck || len(c.pendingFrames) != 0 || c.graphicsFlow.queueDepth >= graphicsByteBudget {
		return 0
	}
	return max(0, c.graphicsFlow.recoverUntil.Sub(now))
}
