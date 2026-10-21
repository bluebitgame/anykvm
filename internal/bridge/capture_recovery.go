package bridge

import (
	"anykvm/internal/capture"
	"anykvm/internal/logging"
	"time"
)

// Do not reset backoff on Open: locked desktops may open successfully and then
// immediately stop or never produce a frame. Require sustained healthy output.
type captureRecovery struct {
	delay        time.Duration
	healthySince time.Time
}

func (r *captureRecovery) retryDelay() time.Duration {
	r.healthySince = time.Time{}
	if r.delay == 0 {
		r.delay = time.Second
	} else {
		r.delay = min(2*r.delay, 30*time.Second)
	}
	return r.delay
}

func (r *captureRecovery) frame(now time.Time) {
	if r.healthySince.IsZero() {
		r.healthySince = now
	}
	if now.Sub(r.healthySince) >= 30*time.Second {
		r.delay = 0
	}
}

// Reopen only the video source, preserving audio, desktop mode, negotiated
// options and the connected clients. Stop cancels both backoff and future opens.
func recoverCapture(dev capture.Device, run *captureRun, recovery *captureRecovery, reason string) bool {
	for {
		delay := recovery.retryDelay()
		logging.Infof("bridge: capture recovery in %s: %s", delay, reason)
		if !waitCapture(run.stop, delay) {
			return false
		}
		run.reopenMu.Lock()
		select {
		case <-run.stop:
			run.reopenMu.Unlock()
			return false
		default:
		}
		dev.Close()
		err := dev.OpenWithFormat(run.width, run.height, run.fps)
		run.reopenMu.Unlock()
		if err == nil {
			logging.Infof("bridge: capture reopened, waiting for first frame")
			return true
		}
		reason = err.Error()
	}
}
