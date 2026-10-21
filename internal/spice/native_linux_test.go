//go:build linux && cgo && spice

package spice

import (
	"os"
	"strings"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

func TestGLibOpenFailureCleanup(t *testing.T) {
	// Exercises the real GLib thread and partial-open teardown without a GPU.
	for i := 0; i < 3; i++ {
		session, err := openGLib("/tmp/anykvm-spice-test-missing.sock", "", "/dev/null")
		if err == nil {
			session.close()
			t.Fatal("accepted a non-DRM render node")
		}
	}
}

// Explicit opt-in: this requires a running local SPICE GL VM and a Vulkan
// Video capable GPU. It is never counted as passed on an unconfigured host.
func TestNativeSPICEDMABUF(t *testing.T) {
	socket := os.Getenv("ANYKVM_SPICE_TEST_SOCKET")
	if socket == "" {
		t.Skip("set ANYKVM_SPICE_TEST_SOCKET on a Linux GPU host with a running SPICE GL VM")
	}
	d := newDevice("spice:"+socket, Options{RenderNode: os.Getenv("ANYKVM_SPICE_TEST_RENDER_NODE"), Timeout: 15 * time.Second, PasswordEnv: os.Getenv("ANYKVM_SPICE_TEST_PASSWORD_ENV")}).(*Device)
	if err := d.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		idr, sps := false, false
		for _, nal := range videodec.SplitNALs(f.Data) {
			if len(nal) == 0 {
				continue
			}
			if nal[0]&31 == 5 {
				idr = true
			}
			if nal[0]&31 == 7 {
				info, err := videodec.ParseSPS(nal)
				if err != nil {
					t.Fatal(err)
				}
				sps = info.Width == f.Width && info.Height == f.Height
			}
		}
		f.Release()
		if !idr || !sps {
			t.Fatal("hardware path did not return an independently decodable H.264 IDR")
		}
		first = true
		break
	}
	if !first {
		t.Fatal("no GPU frame received")
	}
	// Request an IDR after acknowledging the scanout. It must also work with a
	// completely static guest desktop, using only the encoder-owned GPU image.
	d.RequestKeyframe()
	f, err := d.Capture()
	if err != nil || f == nil {
		t.Fatalf("idle IDR: %v", err)
	}
	f.Release()
	stats := d.Stats()
	if stats.GPUFrames < 1 || stats.CPUReadbacks != 0 {
		t.Fatalf("GPU-only assertion failed: %+v", stats)
	}
	t.Logf("SPICE DMA-BUF -> Vulkan Video H.264: %+v", stats)
}

func TestNativeSourceBuildSelection(t *testing.T) {
	device, ok := NewCapture("spice:/tmp/test.sock", Options{})
	if !ok {
		t.Fatal("SPICE source was not selected")
	}
	if _, ok := device.(interface{ AudioDevice() capture.AudioDevice }); !ok {
		t.Fatal("SPICE source audio provider missing")
	}
	if _, ok := device.(interface {
		CaptureCursor() (*capture.CursorState, error)
	}); !ok {
		t.Fatal("SPICE source cursor provider missing")
	}
	if _, ok := device.(*Device); !ok {
		t.Fatal("native SPICE implementation missing from tagged Linux build")
	}
	if err := ValidateSource("spice://127.0.0.1:5900", Options{}); err == nil || !strings.Contains(err.Error(), "DMA-BUF") {
		t.Fatal("accepted TCP for GPU-only path")
	}
}
