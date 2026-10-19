package bridge

import "testing"

type jpegQualityCapture struct {
	blockingCapture
	quality int
}

func (d *jpegQualityCapture) SetJPEGQuality(q int) { d.quality = q }

func TestWebJPEGQualityIsProtocolSpecific(t *testing.T) {
	dev := &jpegQualityCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1280, CapH: 720, FPS: 30}}, "alt", 0, 0, 0)
	b.SetWebJPEGQuality(75)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := b.StartWeb(0); err != nil {
		t.Fatal(err)
	}
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.quality != 75 {
		t.Fatalf("Web quality=%d", dev.quality)
	}
	b.OnClientDisconnected()
	b.ClaimVNC()
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.quality != 0 {
		t.Fatal("Web quality leaked to VNC")
	}
	b.OnClientDisconnected()
}
