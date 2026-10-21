package bridge

import (
	"testing"
	"time"
)

type notifyingCapture struct {
	blockingCapture
	ready chan struct{}
}

func (d *notifyingCapture) CaptureReady() <-chan struct{} { return d.ready }

func TestCaptureReadyWakesOnFrameOrStop(t *testing.T) {
	dev := &notifyingCapture{ready: make(chan struct{}, 1)}
	for _, queued := range []bool{false, true} {
		stop := make(chan struct{})
		if queued {
			dev.ready <- struct{}{}
		}
		result := make(chan bool, 1)
		go func() { result <- waitCaptureFrame(dev, stop) }()
		if !queued {
			select {
			case <-result:
				t.Fatal("event-driven capture fell back to short polling")
			case <-time.After(20 * time.Millisecond):
			}
			dev.ready <- struct{}{}
		}
		select {
		case ok := <-result:
			if !ok {
				t.Fatal("frame notification cancelled capture")
			}
		case <-time.After(time.Second):
			t.Fatal("frame notification was missed")
		}
		go func() { result <- waitCaptureFrame(dev, stop) }()
		close(stop)
		select {
		case ok := <-result:
			if ok {
				t.Fatal("stop did not cancel the notification wait")
			}
		case <-time.After(time.Second):
			t.Fatal("stop waited for the idle diagnostic timer")
		}
	}
}
