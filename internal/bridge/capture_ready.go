package bridge

import (
	"time"

	"anykvm/internal/capture"
)

func waitCaptureFrame(dev capture.Device, stop <-chan struct{}) bool {
	if source, ok := dev.(capture.FrameNotifier); ok {
		if ready := source.CaptureReady(); ready != nil {
			// The timeout only runs idle/recovery diagnostics. A ready frame
			// wakes this wait immediately, including one produced before select.
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-stop:
				return false
			case <-ready:
			case <-timer.C:
			}
			return true
		}
	}
	return waitCapture(stop, 2*time.Millisecond)
}
