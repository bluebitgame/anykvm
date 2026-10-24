//go:build linux && cgo

package capture

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLinuxKMSVulkanJPEG(t *testing.T) {
	pi := os.Getenv("ANYKVM_TEST_KMS_RPI") != ""
	if os.Getenv("ANYKVM_TEST_KMS_JPEG") == "" && !pi {
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
	for _, size := range [][2]int{{638, 478}, {1280, 720}} {
		if err := d.OpenWithFormat(size[0], size[1], 30); err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		for index := 0; index < 6; index++ {
			before := d.enc.gpuFrames
			frame := captureOne(t, d, 10*time.Second)
			image, err := jpeg.Decode(bytes.NewReader(frame.Data))
			valid := err == nil && image.Bounds().Dx() == frame.Width && image.Bounds().Dy() == frame.Height
			frame.Release()
			if !valid || frame.Format != FormatJPEG || d.enc.jpegEncoder == nil || d.enc.jpegDisabled || d.enc.gpuFrames != before+1 || d.enc.readbacks != 0 || d.enc.converter != nil {
				t.Fatalf("Vulkan JPEG path: decode=%v GPU=%d pixel readbacks=%d disabled=%v", err, d.enc.gpuFrames, d.enc.readbacks, d.enc.jpegDisabled)
			}
		}

		d.SetJPEGRegions(true)
		for index := 0; index < 3; index++ {
			frame := captureOne(t, d, 10*time.Second)
			snapshot := frame.JPEGRegions
			if snapshot == nil || len(frame.Data) != 0 || d.enc.jpegDisabled || d.enc.readbacks != 0 {
				frame.Release()
				t.Fatal("compact Vulkan JPEG mode fell back or returned full JPEG")
			}
			data, err := snapshot.Crop(image.Rect(0, 0, frame.Width, frame.Height))
			if err != nil {
				frame.Release()
				t.Fatal(err)
			}
			decoded, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil || decoded.Bounds().Dx() != frame.Width || decoded.Bounds().Dy() != frame.Height {
				frame.Release()
				t.Fatal("invalid compact Vulkan snapshot", err)
			}
			frame.Release()
		}
		d.SetJPEGRegions(false)
		if pi {
			major, minor := d.enc.jpegEncoder.DRMDevice()
			for _, device := range []struct {
				major, minor uint32
				driver       string
			}{
				{uint32(d.output.device_major), uint32(d.output.device_minor), "vc4-drm"},
				{major, minor, "v3d"},
			} {
				path := fmt.Sprintf("/sys/dev/char/%d:%d/device/driver", device.major, device.minor)
				driver, err := os.Readlink(path)
				if err != nil || filepath.Base(driver) != device.driver {
					t.Fatalf("Pi DRM device %s: driver=%q want=%q err=%v", path, driver, device.driver, err)
				}
			}
			if major == uint32(d.output.device_major) && minor == uint32(d.output.device_minor) {
				t.Fatal("Pi test did not exercise separate display/render DRM devices")
			}
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if d.enc.jpegEncoder != nil {
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
		if d.enc.gpuFrames < 12 || d.enc.readbacks != 0 {
			d.Close()
			t.Fatalf("GPU frames=%d pixel readbacks=%d", d.enc.gpuFrames, d.enc.readbacks)
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
