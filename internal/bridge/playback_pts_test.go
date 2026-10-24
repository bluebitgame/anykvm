package bridge

import (
	"testing"
	"time"

	"anykvm/internal/capture"
)

type playbackPTSCapture struct {
	blockingCapture
	frame *capture.Frame
}

func (d *playbackPTSCapture) Capture() (*capture.Frame, error) {
	if d.frame != nil {
		f := d.frame
		d.frame = nil
		return f, nil
	}
	return d.blockingCapture.Capture()
}

func TestCaptureBridgePreservesIndependentPlaybackPTS(t *testing.T) {
	for _, format := range []capture.VideoFormat{capture.FormatH264, capture.FormatHEVC} {
		diagnostic := time.Now().Add(-time.Hour)
		dev := &playbackPTSCapture{frame: &capture.Frame{Format: format, Data: []byte{0, 0, 1, 0x65},
			Width: 4, Height: 4, CapturedAt: diagnostic, PresentationPTS: 123456789}}
		b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
		sink := &nativeResizeSink{frames: make(chan Frame, 4)}
		b.SetWS(sink, nil)
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		b.ClaimWS()
		b.OnClientConnected()
		select {
		case f := <-sink.frames:
			if f.Format != format || f.PresentationPTS != 123456789 || !f.CapturedAt.Equal(diagnostic) {
				t.Errorf("bridge mixed playback and diagnostic clocks: %+v", f)
			}
		case <-time.After(time.Second):
			t.Error("missing capture frame")
		}
		b.Stop()
	}
}
