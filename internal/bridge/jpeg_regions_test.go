package bridge

import (
	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"os"
	"sync/atomic"
	"testing"
)

type regionPolicyCapture struct {
	blockingCapture
	regions atomic.Bool
}

func (d *regionPolicyCapture) SetJPEGRegions(v bool) { d.regions.Store(v) }

func TestVNCJPEGRegionCapturePolicy(t *testing.T) {
	for _, proto := range []string{"vnc", "ws", "rdp", "viewer"} {
		t.Run(proto, func(t *testing.T) {
			d := &regionPolicyCapture{}
			b := NewBridge([]*MachineSession{{Capture: d}}, "alt", 30, 0, 0)
			b.claimSession(proto)
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			b.OnClientConnected()
			d.waitForRead(t)
			if d.regions.Load() != (proto == "vnc") {
				t.Fatalf("%s compact GPU JPEG policy=%v", proto, d.regions.Load())
			}
		})
	}
}

func TestVNCJPEGRegionSnapshotRouting(t *testing.T) {
	data, err := os.ReadFile("../jpegcodec/testdata/restart_base.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := jpegcodec.ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{JPEGRegions: snapshot, Format: capture.FormatJPEG, Width: snapshot.Width, Height: snapshot.Height}
	retained := frame.Retain()
	frame.Release()
	if retained.JPEGRegions != snapshot || len(retained.Data) != 0 {
		t.Fatal("snapshot lifetime lost")
	}
	rdp, web, viewer := &takeoverServer{}, &takeoverServer{}, &takeoverServer{}
	b := &Bridge{rdpSrv: rdp, wsSrv: web, viewerSrv: viewer}
	b.pushFrame(retained, nil)
	if len(rdp.frames) != 0 || len(web.frames) != 0 || len(viewer.frames) != 0 {
		t.Fatal("compressed snapshot reached byte-only consumer")
	}
	retained.Release()
	if retained.JPEGRegions != nil {
		t.Fatal("released frame retained snapshot")
	}
}
