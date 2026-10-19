package capture

import (
	"sync"
	"testing"
	"time"
)

// fakeRawSource is a Device producing solid-color frames at ~60fps, mimicking
// the native iOS capture source. pixfmt "" produces BGRA (FormatRaw);
// "uyvy422"/"yuyv422"/"nv12" produce FormatYUV frames.
type fakeRawSource struct {
	w, h   int
	pixfmt string

	mu     sync.Mutex
	open   bool
	last   time.Time
	closed int
}

func (f *fakeRawSource) Open() error { return f.OpenWithFormat(0, 0, 30) }
func (f *fakeRawSource) OpenWithFormat(w, h, fps int) error {
	f.mu.Lock()
	f.open = true
	f.mu.Unlock()
	return nil
}
func (f *fakeRawSource) SetDecodeImage(bool) {}
func (f *fakeRawSource) SetJPEGQuality(int)  {}

func (f *fakeRawSource) Capture() (*Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return nil, nil
	}
	if time.Since(f.last) < 16*time.Millisecond {
		return nil, nil
	}
	f.last = time.Now()
	if f.pixfmt != "" {
		data := make([]byte, yuvFrameSize(f.pixfmt, f.w, f.h))
		for i := range data {
			data[i] = 0x80
		}
		return &Frame{Data: data, Format: FormatYUV, Width: f.w, Height: f.h, PixFmt: f.pixfmt}, nil
	}
	data := make([]byte, f.w*f.h*4)
	for i := 0; i < len(data); i += 4 {
		data[i] = 0x40 // B
		data[i+1] = 0x80
		data[i+2] = 0xC0
		data[i+3] = 0xFF
	}
	return &Frame{Data: data, Format: FormatRaw, Width: f.w, Height: f.h}, nil
}

func (f *fakeRawSource) Close() error {
	f.mu.Lock()
	f.open = false
	f.closed++
	f.mu.Unlock()
	return nil
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
}

// TestPipeEncoderMJPEG verifies raw frames encode without FFmpeg libraries.
func TestPipeEncoderMJPEG(t *testing.T) {
	src := &fakeRawSource{w: 320, h: 240}
	pe := newPipeEncoderDevice(src, "", 30)
	pe.SetDecodeImage(false)
	if err := pe.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pe.Close()

	f := captureOne(t, pe, 5*time.Second)
	if f.Format != FormatJPEG {
		t.Fatalf("format = %v, want jpeg", f.Format)
	}
	if f.Width != 320 || f.Height != 240 {
		t.Fatalf("size = %dx%d, want 320x240", f.Width, f.Height)
	}
}

// TestPipeEncoderH264 verifies the stdin-fed encoder produces H.264 access units.
func TestPipeEncoderH264(t *testing.T) {
	requireFFmpeg(t)

	src := &fakeRawSource{w: 320, h: 240}
	pe := newPipeEncoderDevice(src, "h264", 30)
	if err := pe.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pe.Close()

	f := captureOne(t, pe, 5*time.Second)
	if f.Format != FormatH264 {
		t.Fatalf("format = %v, want h264", f.Format)
	}
}

// TestPipeEncoderYUV verifies YUV passthrough input (uyvy422 and nv12) is
// accepted by the encoder and produces output frames.
func TestPipeEncoderYUV(t *testing.T) {
	requireFFmpeg(t)

	for _, pixfmt := range []string{"uyvy422", "nv12"} {
		t.Run(pixfmt, func(t *testing.T) {
			src := &fakeRawSource{w: 320, h: 240, pixfmt: pixfmt}
			pe := newPipeEncoderDevice(src, "", 30)
			pe.SetDecodeImage(false)
			if err := pe.OpenWithFormat(0, 0, 30); err != nil {
				t.Fatalf("open: %v", err)
			}
			defer pe.Close()

			f := captureOne(t, pe, 5*time.Second)
			if f.Format != FormatJPEG {
				t.Fatalf("format = %v, want jpeg", f.Format)
			}
		})
	}
}

// TestPipeEncoderCodecSwitch mimics the bridge's codec-switch reopen: an MJPEG
// session is closed while a consumer goroutine is blocked in Capture, then an
// H.264 session opens on the same source. Regression test for the shutdown
// deadlock (Close waiting on the capture mutex held by a starved reader).
func TestPipeEncoderCodecSwitch(t *testing.T) {
	requireFFmpeg(t)

	src := &fakeRawSource{w: 320, h: 240}
	pe := newPipeEncoderDevice(src, "", 30)
	pe.SetDecodeImage(false)
	if err := pe.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatalf("open mjpeg: %v", err)
	}

	// Consumer loop like the bridge captureLoop.
	stop := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			pe.Capture()
		}
	}()

	time.Sleep(300 * time.Millisecond) // let frames flow

	closed := make(chan struct{})
	go func() {
		pe.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe encoder Close deadlocked")
	}
	close(stop)
	<-consumerDone

	// Immediately reopen with H.264, as the bridge does on codec switch.
	pe2 := newPipeEncoderDevice(src, "h264", 30)
	if err := pe2.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatalf("reopen h264: %v", err)
	}
	defer pe2.Close()

	f := captureOne(t, pe2, 5*time.Second)
	if f.Format != FormatH264 {
		t.Fatalf("format after switch = %v, want h264", f.Format)
	}
}

// fakeYUVSource produces uyvy422 FormatYUV frames, mimicking the native iOS
// source in YUV passthrough mode.
type fakeYUVSource struct {
	fakeRawSource
}

func (f *fakeYUVSource) Capture() (*Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return nil, nil
	}
	if time.Since(f.last) < 16*time.Millisecond {
		return nil, nil
	}
	f.last = time.Now()
	data := make([]byte, f.w*f.h*2)
	for i := 0; i < len(data); i += 2 {
		data[i] = 0x80   // U/V neutral
		data[i+1] = 0x60 // Y
	}
	return &Frame{Data: data, Format: FormatYUV, PixFmt: "uyvy422", Width: f.w, Height: f.h}, nil
}

// TestPipeEncoderYUVPassthrough verifies uyvy422 input is fed to ffmpeg
// without BGRA conversion and still produces JPEG output.
func TestPipeEncoderYUVPassthrough(t *testing.T) {
	requireFFmpeg(t)

	src := &fakeYUVSource{fakeRawSource{w: 320, h: 240}}
	pe := newPipeEncoderDevice(src, "", 30)
	pe.SetDecodeImage(false)
	if err := pe.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pe.Close()

	f := captureOne(t, pe, 5*time.Second)
	if f.Format != FormatJPEG {
		t.Fatalf("format = %v, want jpeg", f.Format)
	}
	if f.Width != 320 || f.Height != 240 {
		t.Fatalf("size = %dx%d, want 320x240", f.Width, f.Height)
	}
}

func captureOne(t *testing.T, d Device, timeout time.Duration) *Frame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := d.Capture()
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if f != nil && len(f.Data) > 0 {
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no frame within timeout")
	return nil
}

func TestPipeFrameValidation(t *testing.T) {
	for _, tc := range []struct {
		name, format        string
		width, height, size int
		valid               bool
	}{
		{"unknown empty", "unknown", 2, 2, 0, false},
		{"truncated nv12", "nv12", 3, 3, 12, false},
		{"odd nv12", "nv12", 3, 3, 17, true},
		{"odd packed", "uyvy422", 3, 3, 24, true},
		{"even nv12", "nv12", 4, 4, 24, true},
		{"even packed", "yuyv422", 4, 4, 32, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := &Frame{Format: FormatYUV, PixFmt: tc.format, Width: tc.width, Height: tc.height, Data: make([]byte, tc.size)}
			if got := validPipeFrame(frame); got != tc.valid {
				t.Fatalf("valid = %v, want %v", got, tc.valid)
			}
		})
	}
}
