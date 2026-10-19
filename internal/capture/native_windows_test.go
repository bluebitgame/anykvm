//go:build windows && cgo

package capture

import (
	"anykvm/internal/avmedia"
	"bytes"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWindowsDesktopComputeJPEG(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WINDOWS_GPU_JPEG") == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_GPU_JPEG=1 on an interactive Windows GPU host")
	}
	t.Setenv("ANYKVM_WINDOWS_GPU_JPEG", "1")
	id := os.Getenv("ANYKVM_TEST_WINDOWS_SCREEN")
	if id == "" {
		id = "screen:0"
	}
	d := newWindowsDevice(id).(*windowsDevice)
	d.SetDecodeImage(false)
	d.SetVideoCodec("mjpeg")
	d.SetOutputSize(638, 478)
	d.SetJPEGQuality(85)
	// Reopen to check that compute resources do not survive capture teardown.
	for range 2 {
		if err := d.OpenWithFormat(638, 478, 30); err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		f := captureOne(t, d, 15*time.Second)
		img, err := jpeg.Decode(bytes.NewReader(f.Data))
		valid := err == nil && img.Bounds().Dx() == f.Width && img.Bounds().Dy() == f.Height
		f.Release()
		if !valid || f.Format != FormatJPEG || d.gpuJPEG.encoder == nil || d.gpuJPEG.disabled || d.textureFrames == 0 || d.readbacks != 0 {
			t.Fatalf("JPEG GPU path: decode=%v textureFrames=%d readbacks=%d disabled=%v", err, d.textureFrames, d.readbacks, d.gpuJPEG.disabled)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if d.gpuJPEG.encoder != nil {
			t.Fatal("encoder survived capture close")
		}
	}
}

// Run on an interactive Windows host with the matching display adapter.
func TestWindowsDesktopNVENCTexture(t *testing.T) {
	testWindowsDesktopTexture(t, "NVENC", "nvenc_native")
}
func TestWindowsDesktopAMFTexture(t *testing.T) {
	testWindowsDesktopTexture(t, "AMF", "amf_native")
}
func TestWindowsDesktopQSVTexture(t *testing.T) {
	testWindowsDesktopTexture(t, "QSV", "qsv_native")
}
func testWindowsDesktopTexture(t *testing.T, vendor, encoder string) {
	t.Helper()
	if os.Getenv("ANYKVM_TEST_WINDOWS_"+vendor) == "" {
		t.Skipf("set ANYKVM_TEST_WINDOWS_%s=1 on the matching Windows GPU host", vendor)
	}
	if avmedia.Enabled {
		t.Skip("requires a native-only build")
	}
	id := os.Getenv("ANYKVM_TEST_WINDOWS_SCREEN")
	if id == "" {
		id = "screen:0"
	}
	d := newWindowsDevice(id).(*windowsDevice)
	d.SetVideoCodec("h264")
	d.SetVideoStreamOptions(&VideoStreamOptions{Width: 1280, Height: 720, FPS: 60, Bitrate: 12000})
	for session := 0; session < 2; session++ {
		fps := 60
		if session == 1 {
			fps = 0
			d.SetVideoStreamOptions(nil)
		}
		if err := d.OpenWithFormat(1280, 720, fps); err != nil {
			t.Fatal(err)
		}
		if fps == 0 && (d.captureRate != d.sourceRate || d.enc.requestedRate != d.captureRate) {
			d.Close()
			t.Fatal("omitted FPS did not follow the display rate")
		}
		f := captureOne(t, d, 15*time.Second)
		if !d.adapter.Set || d.enc.encodeOptions.WindowsAdapter != d.adapter {
			f.Release()
			d.Close()
			t.Fatalf("capture adapter=%s encoder requested=%s", d.adapter, d.enc.encodeOptions.WindowsAdapter)
		}
		t.Logf("desktop adapter=%s encoder=%s", d.adapter, d.enc.encoder.Name())
		if f.Format != FormatH264 || len(f.Data) == 0 {
			f.Release()
			d.Close()
			t.Fatal("missing H.264 desktop")
		}
		f.Release()
		if err := d.RequestKeyframe(); err != nil {
			d.Close()
			t.Fatal(err)
		}
		repeat := captureOne(t, d, 5*time.Second)
		if repeat.Format != FormatH264 || d.enc.forceIDR.Load() {
			repeat.Release()
			d.Close()
			t.Fatal("static desktop did not honor IDR request")
		}
		repeat.Release()
		if !strings.Contains(d.enc.encoder.Name(), encoder) || d.textureFrames == 0 || d.readbacks != 0 {
			name := d.enc.encoder.Name()
			d.Close()
			t.Fatalf("encoder=%s GPU frames=%d readbacks=%d", name, d.textureFrames, d.readbacks)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestWindowsMediaFoundationCamera(t *testing.T) {
	id := os.Getenv("ANYKVM_TEST_WINDOWS_CAMERA")
	if id == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_CAMERA to a listed device name or index")
	}
	modes, err := ListVideoModes(id)
	if err != nil || len(modes) == 0 {
		t.Fatalf("capture modes: %+v %v", modes, err)
	}
	value := os.Getenv("ANYKVM_TEST_WINDOWS_CAMERA_MODE")
	if value == "" {
		mode := modes[0]
		value = fmt.Sprintf("%dx%d@%.3f:%s", mode.Width, mode.Height, mode.MaxFPS, mode.PixelFormat)
	}
	request, err := ParseMode(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("device=%q selected=%s modes=%+v", id, value, modes)
	d := newWindowsDevice(id).(*windowsDevice)
	if err := d.SetCaptureMode(value); err != nil {
		t.Fatal(err)
	}
	d.SetDecodeImage(true)
	if err := d.OpenWithFormat(request.Width, request.Height, int(math.Round(request.FPS))); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f := captureOne(t, d, 15*time.Second)
	defer f.Release()
	if f.Format != FormatRaw || len(f.Data) != f.Width*f.Height*4 {
		t.Fatal("invalid native camera pixels")
	}
	if !d.CaptureFrameRate().Valid() {
		t.Fatal("missing source frame rate")
	}
	if request.Width > 0 && (f.Width != request.Width || f.Height != request.Height) {
		t.Fatalf("silently substituted size %dx%d", f.Width, f.Height)
	}
	rate := d.CaptureFrameRate()
	if request.FPS > 0 && math.Abs(float64(rate.Num)/float64(rate.Den)-request.FPS) > 0.001 {
		t.Fatalf("silently substituted FPS %+v", rate)
	}
	if err := d.SetCaptureMode(value); err == nil {
		t.Fatal("changed input mode while capture was open")
	}

}
func TestWindowsMediaFoundationAudio(t *testing.T) {
	id := os.Getenv("ANYKVM_TEST_WINDOWS_AUDIO")
	if id == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_AUDIO to a listed input name or index")
	}
	d := newWindowsAudioDevice(id)
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
		if f != nil {
			if len(f.PCM) == 0 || len(f.PCM)%4 != 0 || f.SampleRate != 48000 || f.Channels != 2 {
				t.Fatal("invalid PCM format")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no audio input samples")
}
func TestWindowsInvalidScreenSelector(t *testing.T) {
	for _, id := range []string{"screen:-1", "screen:nope", "screen:2147483648"} {
		d := newWindowsDevice(id)
		if err := d.Open(); err == nil {
			d.Close()
			t.Fatalf("accepted %q", id)
		}
	}
}

func TestWindowsCaptureModeValidation(t *testing.T) {
	screen := newWindowsDevice("screen:0").(*windowsDevice)
	if err := screen.SetCaptureMode("1920x1080@60:nv12"); err == nil {
		t.Fatal("screen accepted camera format")
	}
	camera := newWindowsDevice("test camera").(*windowsDevice)
	for _, mode := range []string{"1920x1080@59.94:nv12", "1920x1080@60:mjpeg", "1280x720@30:fourcc_30323449"} {
		if err := ConfigureMode(camera, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := ConfigureMode(camera, "1920x1080@60:invalid"); err == nil {
		t.Fatal("invalid format accepted")
	}
	if _, err := ListVideoModes("screen:0"); err == nil {
		t.Fatal("screen mode enumeration accepted")
	}
}

func TestWindowsCameraMJPEGPassthrough(t *testing.T) {
	id := os.Getenv("ANYKVM_TEST_WINDOWS_CAMERA")
	if id == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_CAMERA")
	}
	modes, err := ListVideoModes(id)
	if err != nil {
		t.Fatal(err)
	}
	mode := ""
	for _, m := range modes {
		if m.PixelFormat == "mjpeg" {
			mode = fmt.Sprintf("%dx%d@%.3f:mjpeg", m.Width, m.Height, m.MaxFPS)
			break
		}
	}
	if mode == "" {
		t.Skip("device has no native MJPEG mode")
	}
	d := newWindowsDevice(id).(*windowsDevice)
	d.SetVideoCodec("mjpeg")
	d.SetDecodeImage(false)
	if err := d.SetCaptureMode(mode); err != nil {
		t.Fatal(err)
	}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	frame := captureOne(t, d, 15*time.Second)
	defer frame.Release()
	if frame.Format != FormatJPEG || frame.Buffer == nil || len(frame.Data) < 2 || frame.Data[0] != 0xff || frame.Data[1] != 0xd8 {
		d.Close()
		t.Fatal("native JPEG packet not preserved")
	}
	before := append([]byte(nil), frame.Data...)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(frame.Data) {
		t.Fatal("JPEG lease invalidated by capture close")
	}
	if d.enc.encoder != nil || d.readbacks != 0 {
		t.Fatal("MJPEG passthrough invoked pixel encoder/readback")
	}
}

func TestWindowsCameraNV12Direct(t *testing.T) {
	id := os.Getenv("ANYKVM_TEST_WINDOWS_CAMERA")
	mode := os.Getenv("ANYKVM_TEST_WINDOWS_CAMERA_MODE")
	if id == "" || !strings.HasSuffix(mode, ":nv12") {
		t.Skip("set camera and explicit :nv12 mode")
	}
	d := newWindowsDevice(id).(*windowsDevice)
	d.SetVideoCodec("h264")
	d.SetDecodeImage(false)
	if err := d.SetCaptureMode(mode); err != nil {
		t.Fatal(err)
	}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := 0; i < 30; i++ {
		f := captureOne(t, d, 15*time.Second)
		if f.Format != FormatH264 || len(f.Data) == 0 {
			f.Release()
			t.Fatal("missing H.264")
		}
		f.Release()
	}
	t.Logf("NV12 borrowed frames=%d packed fallback frames=%d", d.textureFrames, d.readbacks)
	if d.textureFrames == 0 {
		t.Fatal("NV12 did not use borrowed frame input")
	}
}
