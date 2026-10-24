package bridge

import (
	"anykvm/internal/capture"
	"testing"
	"time"
)

type surfaceOnlyReopenCapture struct{ blockingCapture }

func (*surfaceOnlyReopenCapture) SetGPUSurface(bool) {}

func TestCodecReopenWithGPUCapturePolicies(t *testing.T) {
	for _, surfaceOnly := range []bool{false, true} {
		name := "desktop-damage-and-surface"
		var dev capture.Device = &surfacePolicyCapture{}
		if surfaceOnly {
			name = "surface-only"
			dev = &surfaceOnlyReopenCapture{}
		}
		t.Run(name, func(t *testing.T) {
			b := NewBridge([]*MachineSession{{Name: "Windows", Capture: dev}}, "alt", 30, 0, 0)
			step := func(name string, fn func()) {
				t.Helper()
				done := make(chan struct{})
				go func() { fn(); close(done) }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatalf("%s deadlocked", name)
				}
			}
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			step("initial capture", b.OnClientConnected)
			step("negotiate H264 while capturing", func() { b.SetVideoCodecs(true, false, false) })
			step("switch to JPEG while capturing", func() { b.SetVideoCodecs(false, false, false) })
			step("disconnect", b.OnClientDisconnected)
			step("reconnect", b.OnClientConnected)
			step("renegotiate H264", func() { b.SetVideoCodecs(true, false, false) })
			step("shutdown", b.Stop)
		})
	}
}
