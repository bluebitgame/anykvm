package rdp

import (
	"time"

	"anykvm/internal/gpuconv"
)

// A verified capture may use its remaining frame time for one small UPGRADE
// packet. Queue occupancy alone cannot cancel this opportunity: continuously
// arriving identical captures would otherwise prevent any quality progress.
// Changed tiles and move destinations are excluded, and all FIRSTs precede it.
func (c *client) prepareCaptureRefinement(e *progressiveEncoder, changed []bitmapRect, copies []gpuconv.SurfaceMove, id uint32, started time.Time, buffered int) ([]bitmapRect, []byte, error) {
	if !e.hasRefinements() || c.captureRefinementBlocked(buffered) {
		return nil, nil, nil
	}
	c.ackMu.Lock()
	interval := c.graphicsFlow.interval
	c.ackMu.Unlock()
	if interval <= 0 {
		interval = time.Second / 30
	}
	deadline := started.Add(interval - min(time.Millisecond, interval/8))
	remaining := time.Until(deadline)
	cost := e.upgradeTileCost
	if !e.upgradeCostAt.IsZero() && time.Since(e.upgradeCostAt) >= 250*time.Millisecond {
		// A one-off driver/compiler stall must not permanently suppress quality
		// work. Revalidate stale expensive estimates with one small probe.
		cost = min(cost, 2*time.Millisecond)
	}
	if remaining <= 0 || cost > remaining {
		return nil, nil, nil
	}
	limit := progressiveBatchTiles
	if cost > 0 {
		limit = max(1, min(limit, int(min(2*time.Millisecond, remaining)/cost)))
	}
	blocked := make([]bool, len(e.pending))
	exclude := func(x, y, w, h int) {
		for yy := y / 64; yy < (y+h+63)/64; yy++ {
			for xx := x / 64; xx < (x+w+63)/64; xx++ {
				i := yy*((e.w+63)/64) + xx
				if i >= 0 && i < len(blocked) {
					blocked[i] = true
				}
			}
		}
	}
	for _, r := range changed {
		exclude(r.x, r.y, r.w, r.h)
	}
	for _, m := range copies {
		exclude(m.DestinationX, m.DestinationY, m.Width, m.Height)
	}
	rects := e.pendingRectsExcept(limit, blocked)
	if len(rects) == 0 {
		return nil, nil, nil
	}
	packet, err := e.prepareUpgrade(rects, id)
	if err != nil {
		return nil, nil, err
	}
	if time.Now().After(deadline) || c.captureRefinementBlocked(buffered+graphicsProgressiveHeaderSize+len(packet)+12) {
		return nil, nil, nil
	}
	return rects, packet, nil
}

func (c *client) captureRefinementBlocked(buffered int) bool {
	select {
	case <-c.done:
		return true
	default:
	}
	if c.reset.Load() {
		return true
	}
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	if !c.suspendAck && (c.graphicsFlow.window(time.Now()) <= 1 || c.graphicsFlow.queueFull()) {
		return true
	}
	bytes := uint64(max(0, buffered))
	for _, pending := range c.pendingFrames {
		bytes += pending.bytes
	}
	reserve := max(uint64(256<<10), c.graphicsFlow.frameBytes)
	return bytes >= graphicsByteBudget || reserve >= graphicsByteBudget-bytes
}

// A duplicate capture has no FIRST to carry the packet. Send one bounded
// refinement frame, still reserving ACK capacity for the next changed capture.
func (c *client) sendCheckedCaptureRefinement(e *progressiveEncoder, started time.Time) (int, time.Duration, time.Duration, error) {
	if !e.hasRefinements() || c.refinementWindowFull() {
		return 0, 0, 0, nil
	}
	id := c.nextGraphicsFrame()
	written := false
	defer func() {
		if !written {
			c.ackMu.Lock()
			delete(c.pendingFrames, id)
			c.ackMu.Unlock()
		}
	}()
	t := time.Now()
	batch := graphicsStartFrame(id, t)
	rects, packet, err := c.prepareCaptureRefinement(e, nil, nil, id, started, len(batch))
	encoded := time.Since(t)
	if err != nil || len(rects) == 0 {
		return 0, encoded, 0, err
	}
	c.ackMu.Lock()
	blocked := c.graphicsWindowFullExcept(time.Now(), true, &id)
	c.ackMu.Unlock()
	if blocked {
		return 0, encoded, 0, nil
	}
	batch = appendGraphicsProgressive(batch, packet)
	batch = append(batch, gfxPDU(0xc, u32(id))...)
	t = time.Now()
	c.recordGraphicsWrite(id, len(batch), true, t)
	err = c.sendGFX(batch)
	sent := time.Since(t)
	if err != nil {
		return 0, encoded, sent, err
	}
	written = true
	e.commitUpgrade(rects)
	return len(rects), encoded, sent, nil
}
