package bridge

import "testing"

func TestViewerStartsCaptureAndSwitchesTarget(t *testing.T) {
	first, second := &blockingCapture{}, &blockingCapture{}
	b := NewBridge([]*MachineSession{{Capture: first, MouseMode: "absolute"}, {Capture: second, MouseMode: "relative"}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartViewer()
	first.waitForRead(t)
	if !b.needPixels.Load() || b.MouseMode() != "absolute" {
		t.Fatal("viewer must request pixels and use the first target's mouse mode")
	}
	if err := b.KeyDown(0xe2); err != nil {
		t.Fatal(err)
	}
	if err := b.KeyDown(0x1f); err != nil {
		t.Fatal(err)
	} // Alt+2.
	second.waitForRead(t)
	if b.MouseMode() != "relative" {
		t.Fatal("mouse mode did not follow switched target")
	}
	b.OnClientDisconnected()
}

func TestRemoteClientTakesOverViewer(t *testing.T) {
	dev := &blockingCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
	closed := false
	b.SetViewer(&takeoverServer{close: func() { closed = true; b.OnClientDisconnected() }}, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartViewer()
	dev.waitForRead(t)
	b.ClaimWS()
	b.OnClientConnected()
	if !closed {
		t.Fatal("remote client did not close the viewer")
	}
	b.OnClientDisconnected()
}
