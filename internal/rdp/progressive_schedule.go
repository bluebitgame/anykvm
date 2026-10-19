package rdp

import "time"

// A background refinement frame contains at most 64 tiles and checks a 2ms
// encoding budget after each batch. A newly queued capture finishes the current eight
// tile batch, closes the frame, and takes priority on the next loop iteration.
// Pending tiles are immediately eligible; ACKs wake the bitmap loop to continue
// refinement between captures without waiting for a frame tick or idle timeout.
func (c *client) sendProgressiveRefinement(e *progressiveEncoder, started time.Time) (tiles, payload int, encodeTime, sendTime time.Duration, err error) {
	select {
	case <-c.done:
		return
	default:
	}
	c.ackMu.Lock()
	available := c.suspendAck || c.sent-c.acknowledged < 2
	c.ackMu.Unlock()
	if !available || len(c.frames) != 0 || c.reset.Load() {
		return
	}
	rects := e.pendingRects(64)
	if len(rects) == 0 {
		return
	}
	c.ackMu.Lock()
	c.sent++
	id := c.sent
	c.ackMu.Unlock()
	batch := gfxPDU(0xb, join(u32(uint32(time.Since(started)/time.Millisecond)), u32(id)))
	send := func() error {
		t := time.Now()
		err := c.sendGFX(batch)
		sendTime += time.Since(t)
		if err == nil {
			payload += len(batch)
		}
		batch = batch[:0]
		return err
	}
	for i := 0; i < len(rects); {
		end := progressiveBatchEnd(rects, i)
		t := time.Now()
		var encoded []byte
		encoded, err = e.upgrade(rects[i:end], id)
		encodeTime += time.Since(t)
		if err != nil {
			return
		}
		update := graphicsProgressive(encoded)
		if len(batch)+len(update) > 256<<10 {
			if err = send(); err != nil {
				return
			}
		}
		batch = append(batch, update...)
		tiles += end - i
		i = end
		if encodeTime >= 2*time.Millisecond || len(c.frames) != 0 || c.reset.Load() {
			break
		}
		select {
		case <-c.done:
			return tiles, payload, encodeTime, sendTime, nil
		default:
		}
	}
	batch = append(batch, gfxPDU(0xc, u32(id))...)
	err = send()
	return
}
