package rdp

import (
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestBitmapCadenceRetainsFinalCapture(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	initial := bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64, Data: []byte{1}}
	c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64, Data: []byte{2}}
	deadline := time.Now().Add(20 * time.Millisecond)
	f, ok := c.waitBitmapCapture(initial, 64, 64, deadline)
	if !ok || f.Data[0] != 2 || time.Now().Before(deadline) || len(c.frames) != 0 {
		t.Fatal("early final capture was dropped or sent before its slot")
	}
}

func TestBitmapCadenceRetainsPredictionReferences(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	c.frames <- bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 64, Data: []byte{2}}
	c.frames <- bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 64, Data: []byte{3}}
	f, ok := c.waitBitmapCapture(bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64}, 64, 64, time.Now().Add(time.Hour))
	if !ok || f.Data[0] != 2 || len(c.frames) != 1 || (<-c.frames).Data[0] != 3 {
		t.Fatal("cadence wait discarded or reordered prediction references")
	}
}

func TestBitmapCadenceWaitCanBeCanceled(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	close(c.done)
	_, ok := c.waitBitmapCapture(bridge.Frame{Format: capture.FormatRaw, Width: 64, Height: 64}, 64, 64, time.Now().Add(time.Hour))
	if ok {
		t.Fatal("cadence wait ignored connection shutdown")
	}
}
