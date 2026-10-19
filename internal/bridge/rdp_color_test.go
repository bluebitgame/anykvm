package bridge

import (
	"anykvm/internal/capture"
	"testing"
)

type rdpColorCapture struct {
	outputDesktopCapture
	fullRange, openedFullRange bool
}

func (d *rdpColorCapture) SetRDPAVC420(enabled bool) { d.fullRange = enabled }
func (d *rdpColorCapture) OpenWithFormat(w, h, fps int) error {
	d.openedFullRange = d.fullRange
	return d.outputDesktopCapture.OpenWithFormat(w, h, fps)
}

func TestRDPFullRangeScopedToAVC420(t *testing.T) {
	dev := &rdpColorCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1280, CapH: 720}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 1280, 720)
	dev.waitForRead(t)
	if !dev.openedFullRange {
		t.Fatal("RDP AVC420 did not select full range")
	}
	if err := b.ResizeRDPDesktop(1920, 1080); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if !dev.openedFullRange {
		t.Fatal("RDP resize lost color range")
	}
	b.RequestRDPKeyframe()
	dev.waitForRead(t)
	if !dev.openedFullRange {
		t.Fatal("RDP keyframe restart lost color range")
	}
	b.OnClientDisconnected()
	profile := capture.VideoStreamOptions{Codec: "h264", Width: 1280, Height: 720, FPS: 30, Bitrate: 10000}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.openedFullRange || dev.profile == nil || *dev.profile != profile {
		t.Fatal("RDP color range leaked into GameStream")
	}
	b.StopGameStream()
	b.StartRDPWithCodec("planar")
	dev.waitForRead(t)
	if dev.openedFullRange {
		t.Fatal("RDP Planar retained AVC420 color range")
	}
	b.OnClientDisconnected()
	b.ClaimVNC()
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.openedFullRange {
		t.Fatal("RDP color range leaked into VNC")
	}
}
