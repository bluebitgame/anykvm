package rdp

import (
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func predictionCoded(format capture.VideoFormat) bool {
	return format == capture.FormatH264 || format == capture.FormatHEVC || format == capture.FormatAV1
}

// Retain the final independent capture until its presentation slot, replacing
// it with newer captures during the wait. Dropping an early capture outright
// can leave the desktop stale forever when capture stops reporting changes.
// Prediction-coded frames still pass through the decoder in input order.
func (c *client) waitBitmapCapture(f bridge.Frame, w, h int, deadline time.Time) (bridge.Frame, bool) {
	if predictionCoded(f.Format) || f.Width != w || f.Height != h || c.reset.Load() || !deadline.After(time.Now()) {
		return f, true
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			f.Release()
			return bridge.Frame{}, false
		case next := <-c.frames:
			f.Release()
			f = next
			if predictionCoded(f.Format) || f.Width != w || f.Height != h || c.reset.Load() {
				return f, true
			}
		case <-timer.C:
			return f, true
		}
	}
}
