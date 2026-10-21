package rdp

import (
	"anykvm/internal/capture"
	"os"
	"runtime"
	"testing"
	"time"
)

// Exercises capture -> immutable snapshot -> GPU dirty compare -> resident
// encoding on actual hardware, including capture shutdown while a frame lives.
func TestProgressiveGPUSurfaceHardware(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_RDP_GPU_SURFACE") != "1" {
		t.Skip("set ANYKVM_TEST_RDP_GPU_SURFACE=1 on a native Windows desktop or Linux KMS GPU host")
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Fatal("GPU surface capture requires Windows or Linux")
	}
	id := os.Getenv("ANYKVM_TEST_RDP_GPU_SOURCE")
	if id == "" {
		id = "screen"
	}
	dev := capture.NewDevice(id)
	defer dev.Close()
	surface, ok := dev.(capture.GPUSurfaceConfigurer)
	if !ok {
		t.Fatal("capture has no GPU surface path")
	}
	codec, ok := dev.(interface{ SetVideoCodec(string) })
	if !ok {
		t.Fatal("capture has no raw routing")
	}
	surface.SetGPUSurface(true)
	codec.SetVideoCodec("rawvideo")
	dev.SetDecodeImage(true)
	if err := dev.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatal(err)
	}
	var frame *capture.Frame
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := dev.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			frame = f
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if frame == nil {
		t.Fatal("no GPU frame captured")
	}
	defer frame.Release()
	if frame.Format != capture.FormatSurface || frame.Surface == nil || frame.Buffer == nil || len(frame.Data) != 0 {
		t.Fatal("capture returned CPU pixels")
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	} // Snapshot must outlive capture/context shutdown.
	native, err := newGPUProgressive("gpu", 6)
	if err != nil {
		t.Fatal(err)
	}
	e := &progressiveEncoder{native: native, required: true, first: true, quantization: 6, refineBits: 2}
	defer e.close()
	const w, h = 129, 131
	rects, err := e.prepareSurface(frame.Surface, w, h, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rects) != 9 {
		t.Fatalf("first surface has %d dirty tiles", len(rects))
	}
	for i := 0; i < len(rects); {
		end := progressiveBatchEnd(rects, i)
		out, err := e.encodeSurface(w, h, rects[i:end], 1)
		if err != nil || len(out) == 0 {
			t.Fatalf("resident FIRST: %v", err)
		}
		i = end
	}
	if err = e.commitSurface(); err != nil {
		t.Fatal(err)
	}
	for len(e.pendingRects(8)) != 0 {
		if _, err = e.upgrade(e.pendingRects(8), 2); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.coefficients) != 0 {
		t.Fatal("coefficients were read back to CPU")
	}
	rects, err = e.prepareSurface(frame.Surface, w, h, false)
	if err != nil || len(rects) != 0 {
		t.Fatalf("unchanged snapshot: tiles=%d err=%v", len(rects), err)
	}
	rects, err = e.prepareSurface(frame.Surface, w, h, true)
	if err != nil || len(rects) != 9 {
		t.Fatalf("refresh snapshot: tiles=%d err=%v", len(rects), err)
	}
	rects, err = e.prepareSurface(frame.Surface, 67, 65, false)
	if err != nil || len(rects) != 4 {
		t.Fatalf("resized snapshot: tiles=%d err=%v", len(rects), err)
	}
}
