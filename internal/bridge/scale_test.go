package bridge

import (
	"anykvm/internal/capture"
	"testing"
)

func TestScaledTargetOutputLimit(t *testing.T) {
	m := &MachineSession{CapScale: .5}
	for _, frame := range []*capture.Frame{
		{Width: 1440, Height: 900, SourceWidth: 2880, SourceHeight: 1800},
		{Width: 800, Height: 600, SourceWidth: 2880, SourceHeight: 1800},
	} {
		if w, h := m.outputLimit(frame); w != 1440 || h != 900 {
			t.Fatalf("scaled physical limit lost: %dx%d", w, h)
		}
	}
	frame := &capture.Frame{Width: 800, Height: 600}
	if w, h := m.outputLimit(frame); w != 800 || h != 600 {
		t.Fatalf("missing source dimensions scaled twice: %dx%d", w, h)
	}
	m.CapScale, m.CapW, m.CapH = .5, 1920, 1080
	if w, h := m.outputLimit(frame); w != 1920 || h != 1080 {
		t.Fatalf("explicit limit changed: %dx%d", w, h)
	}
}
