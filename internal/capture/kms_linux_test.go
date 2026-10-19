//go:build linux && cgo

package capture

import (
	"bytes"
	"image/jpeg"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLinuxKMSVulkanJPEG(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_KMS_JPEG") == "" {
		t.Skip("set ANYKVM_TEST_KMS_JPEG=1 on a Linux KMS/Vulkan host")
	}
	t.Setenv("ANYKVM_LINUX_GPU_JPEG", "1")
	id := os.Getenv("ANYKVM_TEST_KMS_SCREEN")
	if id == "" {
		id = "screen"
	}
	d := newScreenDevice(id).(*kmsDevice)
	d.SetDecodeImage(false)
	d.SetVideoCodec("mjpeg")
	d.SetJPEGQuality(85)
	for range 2 {
		if err := d.OpenWithFormat(638, 478, 30); err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		frame := captureOne(t, d, 10*time.Second)
		image, err := jpeg.Decode(bytes.NewReader(frame.Data))
		valid := err == nil && image.Bounds().Dx() == frame.Width && image.Bounds().Dy() == frame.Height
		frame.Release()
		if !valid || frame.Format != FormatJPEG || d.jpegEncoder == nil || d.jpegDisabled || d.gpuFrames == 0 || d.readbacks != 0 {
			t.Fatalf("Vulkan JPEG path: decode=%v GPU=%d pixel readbacks=%d disabled=%v", err, d.gpuFrames, d.readbacks, d.jpegDisabled)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if d.jpegEncoder != nil {
			t.Fatal("JPEG encoder survived capture close")
		}
	}
}

func TestLinuxKMSVulkanVideo(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_KMS") == "" {
		t.Skip("set ANYKVM_TEST_KMS=1 on the Linux DRM/Vulkan Video host")
	}
	id := os.Getenv("ANYKVM_TEST_KMS_SCREEN")
	if id == "" {
		id = "screen"
	}
	d := newScreenDevice(id).(*kmsDevice)
	d.SetVideoCodec("h264")
	d.SetVideoStreamOptions(&VideoStreamOptions{Width: 1280, Height: 720, FPS: 60, Bitrate: 12000})
	for session := 0; session < 2; session++ {
		if err := d.OpenWithFormat(0, 0, 0); err != nil {
			t.Fatal(err)
		}
		if !d.CaptureFrameRate().Valid() {
			t.Fatal("missing physical display refresh")
		}
		var elapsed []time.Duration
		for n := 0; n < 12; n++ {
			if n == 6 {
				if err := d.RequestKeyframe(); err != nil {
					t.Fatal(err)
				}
			}
			begin := time.Now()
			deadline := begin.Add(10 * time.Second)
			for {
				f, err := d.Capture()
				if err != nil {
					d.Close()
					t.Fatal(err)
				}
				if f != nil {
					if (n == 0 || n == 6) && (!kmsNAL(f.Data, 7) || !kmsNAL(f.Data, 8) || !kmsNAL(f.Data, 5)) {
						f.Release()
						d.Close()
						t.Fatal("KMS Vulkan frame lacks requested recovery headers/IDR")
					}
					if f.Format != FormatH264 || !strings.Contains(d.enc.encoder.Name(), "vulkan_native") {
						f.Release()
						d.Close()
						t.Fatal("missing native Vulkan H.264")
					}
					elapsed = append(elapsed, time.Since(begin))
					f.Release()
					break
				}
				if time.Now().After(deadline) {
					d.Close()
					t.Fatal("KMS timed out")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if d.gpuFrames < 12 || d.readbacks != 0 {
			d.Close()
			t.Fatalf("GPU frames=%d pixel readbacks=%d", d.gpuFrames, d.readbacks)
		}
		sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
		t.Logf("capture + encode including pacing: p50=%s max=%s", elapsed[len(elapsed)/2], elapsed[len(elapsed)-1])
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func kmsNAL(data []byte, kind byte) bool {
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3]&31 == kind {
			return true
		}
	}
	return false
}
