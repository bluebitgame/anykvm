package bridge

import (
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
	"sync/atomic"
	"testing"
)

func TestGPUSurfaceFrameOwnershipAndRouting(t *testing.T) {
	freed := atomic.Int32{}
	f := Frame{Format: capture.FormatSurface, Surface: &gpuconv.Surface{SharedTexture: 1, Width: 64, Height: 64}, Buffer: framebuf.New(nil, func() { freed.Add(1) })}
	retained := f.Retain()
	f.Release()
	if retained.Surface == nil || retained.Buffer == nil || len(retained.Data) != 0 || freed.Load() != 0 {
		t.Fatal("surface reference was copied as empty pixel data")
	}
	rdp, web, viewer := &takeoverServer{}, &takeoverServer{}, &takeoverServer{}
	b := &Bridge{rdpSrv: rdp, wsSrv: web, viewerSrv: viewer}
	b.pushFrame(retained, nil)
	if len(rdp.frames) != 1 || rdp.frames[0].Surface != retained.Surface || len(web.frames) != 0 || len(viewer.frames) != 0 {
		t.Fatal("GPU frame reached a byte-only consumer")
	}
	retained.Release()
	if freed.Load() != 1 {
		t.Fatal("GPU snapshot leaked")
	}
}

type surfacePolicyCapture struct {
	rdpCapture
	surface atomic.Bool
}

func (d *surfacePolicyCapture) SetGPUSurface(v bool) { d.surface.Store(v) }

func TestRDPProgressiveRequestsGPUSurface(t *testing.T) {
	for _, mode := range []string{"auto", "go", "native", "gpu", "vulkan", "d3d11"} {
		for _, codec := range []string{"progressive", "planar", "h264"} {
			t.Run(mode+"/"+codec, func(t *testing.T) {
				t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", mode)
				d := &surfacePolicyCapture{}
				b := NewBridge([]*MachineSession{{Capture: d}}, "alt", 30, 0, 0)
				if err := b.Start(); err != nil {
					t.Fatal(err)
				}
				defer b.Stop()
				b.StartRDPWithCodec(codec)
				d.waitForRead(t)
				want := codec == "progressive" && (mode == "gpu" || mode == "vulkan" || mode == "d3d11")
				if b.wantSurface != want || d.surface.Load() != want {
					t.Fatalf("surface request=%v capture=%v want %v", b.wantSurface, d.surface.Load(), want)
				}
			})
		}
	}
}
