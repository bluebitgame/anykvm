package bridge

import (
	"anykvm/internal/capture"
	"testing"
	"time"
)

// A fixed capture device has no output-size override; it encodes its own mode.
type fixedSwitchCapture struct{ blockingCapture }

func (*fixedSwitchCapture) PreserveCaptureMode() bool { return true }
func (d *fixedSwitchCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	d.reads++
	first := d.reads == 1
	stop := d.stop
	w, h := d.openW, d.openH
	if first {
		close(d.entered)
	}
	d.mu.Unlock()
	if first {
		return &capture.Frame{Format: capture.FormatH264, Width: w, Height: h, Data: []byte{0, 0, 1, 0x65, 0x80}}, nil
	}
	<-stop
	return nil, capture.ErrStreamStopped
}

func TestRDPHotkeySwitchPreservesFixedSourceAndSession(t *testing.T) {
	local := &nativeResizeCapture{}
	card := &fixedSwitchCapture{}
	b := NewBridge([]*MachineSession{{Capture: local, CapW: 2560, CapH: 1440, FPS: 30}, {Capture: card, CapW: 1920, CapH: 1080, FPS: 60}}, "alt", 0, 0, 0)
	sink := &nativeResizeSink{frames: make(chan Frame, 8)}
	b.SetRDP(sink)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 2560, 1440)
	wait := func(w, h int, fixed bool) {
		t.Helper()
		select {
		case f := <-sink.frames:
			if f.Width != w || f.Height != h || f.PreserveSource != fixed {
				t.Fatalf("unexpected target frame: %+v", f)
			}
		case <-time.After(time.Second):
			t.Fatal("no frame after target switch")
		}
	}
	wait(2560, 1440, false)
	if err := b.KeyDown(hidKeyAltLeft); err != nil {
		t.Fatal(err)
	}
	if err := b.KeyDown(0x1f); err != nil {
		t.Fatal(err)
	} // Alt+2
	wait(1920, 1080, true)
	if b.activeProto != "rdp" || b.consumers != 1 || b.desktopSize.Width != 2560 || b.desktopSize.Height != 1440 {
		t.Fatal("target switch lost the RDP session or requested layout")
	}
	if err := b.KeyRelease(0x1f); err != nil {
		t.Fatal(err)
	}
	if err := b.KeyRelease(hidKeyAltLeft); err != nil {
		t.Fatal(err)
	}
	if err := b.KeyDown(hidKeyAltLeft); err != nil {
		t.Fatal(err)
	}
	if err := b.KeyDown(0x1e); err != nil {
		t.Fatal(err)
	} // Alt+1
	wait(2560, 1440, false)
	if b.activeProto != "rdp" || b.consumers != 1 {
		t.Fatal("return switch disconnected RDP")
	}
}
