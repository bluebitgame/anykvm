package rdp

import (
	"errors"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func predictionCoded(format capture.VideoFormat) bool {
	return format == capture.FormatH264 || format == capture.FormatHEVC || format == capture.FormatAV1
}

// Keep replacing independent captures while the client is busy. Releasing an
// old GPU snapshot here also leaves capture pool space for a fresher snapshot.
func (c *client) waitCaptureWindow(f bridge.Frame) (bridge.Frame, int, error) {
	c.setGraphicsFPS(captureFPS(f))
	dropped := 0
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for c.frameWindowFull() {
		if timer == nil {
			timer = time.NewTimer(5 * time.Second)
		}
		var newer <-chan bridge.Frame
		if !predictionCoded(f.Format) {
			newer = c.frames
		}
		select {
		case <-c.done:
			return f, dropped, nil
		case <-c.ack:
		case next := <-newer:
			next = mergeProgressiveDamage(f, next)
			f.Release()
			f = next
			c.setGraphicsFPS(captureFPS(f))
			dropped++
		case <-timer.C:
			return f, dropped, errors.New("graphics frame acknowledgment timed out")
		}
	}
	f, extra := c.latestCapture(f)
	c.setGraphicsFPS(captureFPS(f))
	return f, dropped + extra, nil
}

// Select the freshest independent capture after flow control permits output.
// Stop at the first prediction-coded frame: its successors depend on it.
// The caller owns the returned reference; superseded references are released.
func (c *client) latestCapture(f bridge.Frame) (bridge.Frame, int) {
	dropped := 0
	for !predictionCoded(f.Format) {
		select {
		case next := <-c.frames:
			next = mergeProgressiveDamage(f, next)
			f.Release()
			f = next
			dropped++
		default:
			return f, dropped
		}
	}
	return f, dropped
}
