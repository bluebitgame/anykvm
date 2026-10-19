//go:build linux && cgo

package capture

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLinuxV4L2Capture(t *testing.T) {
	path := os.Getenv("ANYKVM_TEST_LINUX_VIDEO")
	if path == "" {
		t.Skip("set ANYKVM_TEST_LINUX_VIDEO to the capture card /dev/video path")
	}
	modes, err := ListVideoModes(path)
	if err != nil || len(modes) == 0 {
		t.Fatalf("device modes: %+v %v", modes, err)
	}
	value := os.Getenv("ANYKVM_TEST_LINUX_VIDEO_MODE")
	if value == "" {
		m := modes[0]
		value = fmt.Sprintf("%dx%d@%.3f:%s", m.Width, m.Height, m.MaxFPS, m.PixelFormat)
	}
	request, err := ParseMode(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("device=%q selected=%s modes=%+v", path, value, modes)
	d := newNativeDevice(path).(*v4l2Device)
	if err := ConfigureMode(d, value); err != nil {
		t.Fatal(err)
	}
	wantRaw := request.PixelFormat != "h264"
	d.SetDecodeImage(wantRaw)
	if wantRaw {
		d.SetVideoCodec("rawvideo")
	} else {
		d.SetVideoCodec("h264")
	}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Querying modes while streaming must use pre-capture capabilities.
	if active, err := ListVideoModes(path); err != nil || len(active) == 0 {
		t.Fatal("modes unavailable during capture", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil {
			time.Sleep(time.Millisecond * 5)
			continue
		}
		defer frame.Release()
		if (wantRaw && (frame.Format != FormatRaw || len(frame.Data) != frame.Width*frame.Height*4)) || (!wantRaw && (frame.Format != FormatH264 || len(frame.Data) == 0)) {
			t.Fatal("invalid BGRA capture frame")
		}
		if request.Width > 0 && (frame.Width != request.Width || frame.Height != request.Height) {
			t.Fatal("capture silently changed dimensions")
		}
		rate := d.CaptureFrameRate()
		if !rate.Valid() || (request.FPS > 0 && math.Abs(float64(rate.Num)/float64(rate.Den)-request.FPS) > 0.001) {
			t.Fatalf("incorrect capture rate: %+v", rate)
		}
		return
	}
	t.Fatal("no capture frames")
}
func TestLinuxALSAInput(t *testing.T) {
	device := os.Getenv("ANYKVM_TEST_LINUX_AUDIO")
	if device == "" {
		t.Skip("set ANYKVM_TEST_LINUX_AUDIO to an ALSA capture input")
	}
	t.Logf("audio devices=%v", ListNativeAudioDevices())
	d := newPlatformAudioDevice(device)
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil {
			time.Sleep(time.Millisecond * 5)
			continue
		}
		if frame.SampleRate != 48000 || frame.Channels != 2 || len(frame.PCM) == 0 || len(frame.PCM)%4 != 0 {
			t.Fatal("invalid stereo PCM")
		}
		return
	}
	t.Fatal("no audio samples")
}
func TestLinuxV4L2ModeValidation(t *testing.T) {
	d := newNativeDevice("/dev/video-anykvm-test").(*v4l2Device)
	for _, value := range []string{"1920x1080@59.94:nv12", "1280x720@60:mjpeg", "1920x1080@30:yuyv422", "640x480@30:fourcc_33424752"} {
		if err := ConfigureMode(d, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := ConfigureMode(d, "1920x1080@60:invalid"); err == nil {
		t.Fatal("invalid format accepted")
	}
	if err := d.Close(); err != nil {
		t.Fatal("closing unopened source", err)
	}
	if _, err := ListVideoModes("screen:0"); err == nil {
		t.Fatal("screen accepted as V4L2 input")
	}
	if err := d.Open(); err == nil {
		d.Close()
		t.Fatal("missing V4L2 source opened")
	}
}

func TestLinuxV4L2NV12Direct(t *testing.T) {
	path, mode := os.Getenv("ANYKVM_TEST_LINUX_VIDEO"), os.Getenv("ANYKVM_TEST_LINUX_VIDEO_MODE")
	if path == "" || !strings.HasSuffix(mode, ":nv12") {
		t.Skip("set video device and explicit :nv12 mode")
	}
	d := newNativeDevice(path).(*v4l2Device)
	d.SetVideoCodec("h264")
	d.SetDecodeImage(false)
	if err := ConfigureMode(d, mode); err != nil {
		t.Fatal(err)
	}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	count := 0
	deadline := time.Now().Add(20 * time.Second)
	for count < 60 && time.Now().Before(deadline) {
		f, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		if f.Format != FormatH264 || len(f.Data) == 0 {
			f.Release()
			t.Fatal("missing encoded NV12 frame")
		}
		f.Release()
		count++
	}
	if count != 60 {
		t.Fatal("capture/encoder stalled while returning borrowed buffers")
	}
	t.Logf("DMA-BUF direct=%t fallback=%t encoder=%s", d.nativeLogged, d.fallbackLogged, d.enc.encoder.Name())
	if !d.nativeLogged && !d.fallbackLogged {
		t.Fatal("NV12 did not bypass BGRA conversion")
	}
}
func TestLinuxV4L2MJPEGPassthrough(t *testing.T) {
	path, mode := os.Getenv("ANYKVM_TEST_LINUX_VIDEO"), os.Getenv("ANYKVM_TEST_LINUX_VIDEO_MODE")
	if path == "" || !strings.HasSuffix(mode, ":mjpeg") {
		t.Skip("set video device and explicit :mjpeg mode")
	}
	d := newNativeDevice(path).(*v4l2Device)
	d.SetVideoCodec("mjpeg")
	d.SetDecodeImage(false)
	if err := ConfigureMode(d, mode); err != nil {
		t.Fatal(err)
	}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		defer f.Release()
		if f.Format != FormatJPEG || len(f.Data) < 2 || f.Data[0] != 0xff || f.Data[1] != 0xd8 || d.enc.encoder != nil || d.decoder != nil {
			t.Fatal("MJPEG was decoded or re-encoded")
		}
		return
	}
	t.Fatal("no MJPEG frames")
}
