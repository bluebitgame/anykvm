package rdp

import (
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

func TestLatestCaptureKeepsFinalUpdate(t *testing.T) {
	c := &client{frames: make(chan bridge.Frame, 2)}
	c.frames <- bridge.Frame{Data: []byte{2}, Width: 1280}
	c.frames <- bridge.Frame{Data: []byte{3}, Width: 1920, CaptureFPS: 60}
	f, dropped := c.latestCapture(bridge.Frame{Data: []byte{1}, Width: 640})
	if f.Data[0] != 3 || f.Width != 1920 || dropped != 2 || len(c.frames) != 0 {
		t.Fatal("final capture was lost or left behind older frames")
	}
}

func TestLatestCapturePreservesPredictionReferences(t *testing.T) {
	for _, format := range []capture.VideoFormat{capture.FormatH264, capture.FormatHEVC, capture.FormatAV1} {
		c := &client{frames: make(chan bridge.Frame, 2)}
		c.frames <- bridge.Frame{Format: format, Data: []byte{2}}
		c.frames <- bridge.Frame{Format: format, Data: []byte{3}}
		f, dropped := c.latestCapture(bridge.Frame{Format: format, Data: []byte{1}})
		if f.Data[0] != 1 || dropped != 0 || len(c.frames) != 2 {
			t.Fatal("prediction frame was replaced")
		}
		f, dropped = c.latestCapture(bridge.Frame{Format: capture.FormatRaw, Data: []byte{0}})
		if f.Data[0] != 2 || dropped != 1 || len(c.frames) != 1 || (<-c.frames).Data[0] != 3 {
			t.Fatal("format transition discarded or reordered prediction references")
		}
	}
}

func TestFrameWindowSelectsFreshCaptureAfterAck(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 1), ack: make(chan struct{}, 1), sent: 2}
	defer close(c.done)
	result := make(chan bridge.Frame, 1)
	go func() {
		f, _, err := c.waitCaptureWindow(bridge.Frame{Data: []byte{1}})
		if err != nil {
			return
		}
		result <- f
	}()
	select {
	case <-result:
		t.Fatal("exceeded the client's outstanding frame window")
	case <-time.After(20 * time.Millisecond):
	}
	c.frames <- bridge.Frame{Data: []byte{2}}
	c.ackMu.Lock()
	c.acknowledged = 1
	c.ackMu.Unlock()
	c.ack <- struct{}{}
	select {
	case f := <-result:
		if f.Data[0] != 2 {
			t.Fatal("ACK release sent a capture retained before the wait")
		}
	case <-time.After(time.Second):
		t.Fatal("ACK did not immediately release frame processing")
	}
}

func TestFrameWindowReleasesOldCaptureBeforeAck(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 1), ack: make(chan struct{}, 1), sent: 2}
	defer close(c.done)
	freed := make(chan byte, 3)
	frame := func(id byte) bridge.Frame {
		data := []byte{id}
		return bridge.Frame{Data: data, Buffer: framebuf.New(data, func() { freed <- id })}
	}
	result := make(chan bridge.Frame, 1)
	go func() {
		f, _, err := c.waitCaptureWindow(frame(1))
		if err == nil {
			result <- f
		}
	}()
	for id := byte(2); id <= 3; id++ {
		c.frames <- frame(id)
		select {
		case old := <-freed:
			if old != id-1 {
				t.Fatalf("released %d before %d", old, id-1)
			}
		case <-time.After(time.Second):
			t.Fatal("ACK wait exhausted the capture pool with an old snapshot")
		}
	}
	c.ackMu.Lock()
	c.acknowledged = 1
	c.ackMu.Unlock()
	c.ack <- struct{}{}
	select {
	case f := <-result:
		if f.Data[0] != 3 {
			t.Fatal("ACK released a stale snapshot")
		}
		f.Release()
	case <-time.After(time.Second):
		t.Fatal("ACK failed to wake snapshot processing")
	}
	if <-freed != 3 {
		t.Fatal("latest snapshot lost its owner")
	}
}
