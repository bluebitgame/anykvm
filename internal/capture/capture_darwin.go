//go:build darwin

package capture

// #cgo LDFLAGS: -framework AVFoundation -framework CoreMedia -framework CoreMediaIO -framework CoreVideo -framework Cocoa -framework CoreGraphics -framework ImageIO -framework CoreServices -framework CoreImage
//
// #include <stdlib.h>
//
// extern void* avfoundation_start(const char *deviceName, int width, int height, double fps, unsigned int pixel_format);
// extern void  avfoundation_stop(void* session);
// extern void  avfoundation_rate(void* session, int *num, int *den);
// extern int   frame_buf_take(void **sample, long long *age_ns, int wait_ms);
// extern void  capture_sample_release(void *sample);
// extern void* capture_sample_pixel(void *sample);
// extern unsigned long capture_sample_layout(void *sample, int passthrough, int *w, int *h, int *tag);
// extern void* capture_sample_data(void *sample, unsigned long size);
// extern int   capture_sample_copy(void *sample, int tag, unsigned char *dest, unsigned long size);
// extern void  frame_buf_destroy(void);
// extern int   frame_buf_count(void);
// extern int   avfoundation_probe(const char *deviceName, int want_w, int want_h, int want_fps,
//                                 int *out_w, int *out_h, int *out_fps, int *out_mjpeg);
// extern int   avfoundation_resolve_ios_wait(const char *selector, char *out, int outlen, int wait_ms);
// extern void  avfoundation_list_video_devices(char *buf, int maxLen, int include_ios);
import "C"
import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// iosResolveWaitMS — keep in sync with IOS_RESOLVE_WAIT_MS in capture_darwin.m.
const iosResolveWaitMS = 8000

type darwinDevice struct {
	mode         Mode
	sourceFPS    float64
	sourceFormat uint32
	mu           sync.Mutex // serializes native reads and session shutdown
	quality      int
	rate         framerate.Rate
	id           string
	session      unsafe.Pointer
	statsAt      time.Time
	statsFrames  int

	yuvPassthrough bool // store device YUV frames as-is (for the pipe encoder)

	decodeMu    sync.Mutex
	decodeImage bool

	frameBufs [2][]byte // ring buffer to avoid per-frame heap allocation
	bufIdx    int
}

// Pixel-format tags stored alongside frames (mirrored in capture_darwin.m).
const (
	cPixfmtUnknown = 0
	cPixfmtBGRA    = 1
	cPixfmtUYVY422 = 2
	cPixfmtYUYV422 = 3
	cPixfmtNV12    = 4
)

// cPixfmtName maps the C-side tag to the ffmpeg pixel-format name.
var cPixfmtName = map[int]string{
	cPixfmtUYVY422: "uyvy422",
	cPixfmtYUYV422: "yuyv422",
	cPixfmtNV12:    "nv12",
}

// SetYUVPassthrough preserves native YUV when pixels must be materialized,
// instead of converting to BGRA. Must be set before Open.
func (d *darwinDevice) SetYUVPassthrough(v bool) {
	d.yuvPassthrough = v
}

func newNativeDevice(deviceID string) Device {
	return &darwinDevice{id: deviceID, decodeImage: true}
}

// resolveIOSDevice finds an iOS screen-mirroring device (iPhone/iPad connected
// over USB, exposed via CoreMediaIO after the screen-capture opt-in) and
// returns its localizedName for use as a normal capture device ID. sel
// optionally filters by name substring ("" = first iOS device).
//
// CMIO warm-up (enable_screen_capture_devices) runs lazily inside the native
// resolver on first discover/enumerate — no separate warm call needed here.
func resolveIOSDevice(sel string) (string, error) {
	csel := C.CString(sel)
	defer C.free(unsafe.Pointer(csel))

	const bufLen = 256
	cbuf := (*C.char)(C.malloc(bufLen))
	defer C.free(unsafe.Pointer(cbuf))

	if C.avfoundation_resolve_ios_wait(csel, cbuf, bufLen, C.int(iosResolveWaitMS)) != 0 {
		return C.GoString(cbuf), nil
	}

	if sel != "" {
		return "", fmt.Errorf("no iOS device matching %q found (connect iPhone/iPad via USB, unlock and trust this Mac)", sel)
	}
	return "", fmt.Errorf("no iOS device found (connect iPhone/iPad via USB, unlock and trust this Mac)")
}

// nativeIsFFmpeg is false: macOS has a real AVFoundation backend, so the
// smart-fallback wrapper applies when the device does not output MJPEG.
const nativeIsFFmpeg = false

// SetDecodeImage toggles JPEG decoding of captured MJPEG frames.
func (d *darwinDevice) SetDecodeImage(v bool) {
	d.decodeMu.Lock()
	d.decodeImage = v
	d.decodeMu.Unlock()
}

// SetJPEGQuality controls native raw-to-JPEG encoding. Device MJPEG passes through.
func (d *darwinDevice) SetJPEGQuality(q int) { d.mu.Lock(); d.quality = q; d.mu.Unlock() }

func (d *darwinDevice) shouldDecode() bool {
	d.decodeMu.Lock()
	defer d.decodeMu.Unlock()
	return d.decodeImage
}

// ProbeCaps enumerates the device's supported formats without starting a
// capture session (no permission prompt, does not hold the device) and returns
// the best mode, preferring the requested resolution and MJPEG output.
func (d *darwinDevice) ProbeCaps(wantW, wantH, wantFPS int) (deviceCaps, error) {
	if d.mode.Width > 0 || d.mode.FPS > 0 || d.mode.PixelFormat != "" {
		return d.probeMode()
	}
	name := C.CString(d.id)
	defer C.free(unsafe.Pointer(name))

	var ow, oh, ofps, omjpeg C.int
	ok := C.avfoundation_probe(name, C.int(wantW), C.int(wantH), C.int(wantFPS),
		&ow, &oh, &ofps, &omjpeg)
	if ok == 0 {
		return deviceCaps{}, fmt.Errorf("avfoundation: probe found no usable format")
	}
	return deviceCaps{
		width:  int(ow),
		height: int(oh),
		fps:    int(ofps),
		mjpeg:  omjpeg != 0,
		ok:     true,
	}, nil
}

func (d *darwinDevice) Open() error {
	return d.OpenWithFormat(0, 0, 30)
}

func (d *darwinDevice) OpenWithFormat(width, height, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// DAL devices (iOS screen mirroring) can stall and deliver no frames when
	// a session is closed and reopened in quick succession (codec switches,
	// client reconnects). Wait briefly for the first delegate callback and
	// restart the session once if none arrives.
	for attempt := 0; ; attempt++ {
		before := int(C.frame_buf_count())

		name := C.CString(d.id)
		rate := float64(fps)
		if d.sourceFPS > 0 {
			rate = d.sourceFPS
		}
		session := C.avfoundation_start(name, C.int(width), C.int(height), C.double(rate), C.uint(d.sourceFormat))
		C.free(unsafe.Pointer(name))
		if session == nil {
			return fmt.Errorf("capture: failed to start AVFoundation session")
		}
		d.session = session
		var num, den C.int
		C.avfoundation_rate(session, &num, &den)
		d.rate = framerate.New(int(num), int(den))
		d.statsAt = time.Now()
		d.statsFrames = int(C.frame_buf_count())

		if waitFrameCount(before, 2500*time.Millisecond) {
			return nil
		}
		if attempt >= 1 {
			// Keep the session: frames may start later (e.g. the mirrored
			// device is locked); the bridge tolerates an idle stream.
			log.Printf("capture: session running but no frames yet (device locked or idle?)")
			return nil
		}
		log.Printf("capture: no frames within 2.5s; restarting AVFoundation session")
		C.avfoundation_stop(d.session)
		d.session = nil
		time.Sleep(300 * time.Millisecond)
	}
}

// waitFrameCount polls the native frame counter until it exceeds base,
// bounded by timeout. Returns whether a new frame arrived.
func waitFrameCount(base int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if int(C.frame_buf_count()) > base {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func (d *darwinDevice) Capture() (*Frame, error)       { return d.capture(false) }
func (d *darwinDevice) captureNative() (*Frame, error) { return d.capture(true) }
func (d *darwinDevice) capture(keepNative bool) (*Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil, nil
	}
	if elapsed := time.Since(d.statsAt); elapsed >= 5*time.Second {
		frames := int(C.frame_buf_count())
		log.Printf("capture: AVFoundation input %.1f fps (before host encoding)", float64(frames-d.statsFrames)/elapsed.Seconds())
		d.statsFrames, d.statsAt = frames, time.Now()
	}
	var sample unsafe.Pointer
	var age C.longlong
	waitMS := C.int(100)
	if keepNative {
		waitMS = 0
	}
	if C.frame_buf_take(&sample, &age, waitMS) == 0 {
		return nil, nil
	}
	owned := true
	defer func() {
		if owned {
			C.capture_sample_release(sample)
		}
	}()
	capturedAt := time.Now().Add(-time.Duration(age))
	var w, h, tag C.int
	passthrough := C.int(0)
	if d.yuvPassthrough {
		passthrough = 1
	}
	size := C.capture_sample_layout(sample, passthrough, &w, &h, &tag)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 || size == 0 || size > 256*1024*1024 {
		return nil, fmt.Errorf("capture: invalid native sample")
	}
	f := &Frame{CapturedAt: capturedAt, Width: int(w), Height: int(h)}
	if keepNative && C.capture_sample_pixel(sample) != nil {
		f.nativeBuffer = &darwinFrameBuffer{sample: sample, size: size, tag: tag}
		if name, ok := cPixfmtName[int(tag)]; ok {
			f.Format, f.PixFmt = FormatYUV, name
		}
		owned = false
		return f, nil
	}
	if pixel := C.capture_sample_pixel(sample); pixel != nil && !d.shouldDecode() && !d.yuvPassthrough {
		data, err := jpegcodec.EncodePixelBufferOwned(pixel, d.quality)
		if err != nil {
			return nil, err
		}
		f.Buffer, f.Data, f.Format = data, data.Bytes(), FormatJPEG
		return f, err
	}
	if tag == cPixfmtUnknown && !d.shouldDecode() {
		data := C.capture_sample_data(sample, size)
		if data != nil {
			f.Data = unsafe.Slice((*byte)(data), int(size))
			if len(f.Data) < 2 || f.Data[0] != 0xff || f.Data[1] != 0xd8 {
				return nil, fmt.Errorf("capture: unsupported compressed native sample")
			}
			f.Buffer = framebuf.New(f.Data, func() { C.capture_sample_release(sample) })
			f.Format = FormatJPEG
			owned = false
			return f, nil
		}
		// Noncontiguous block buffers need one flattening copy into owned Go storage.
		f.Data = make([]byte, int(size))
		if C.capture_sample_copy(sample, tag, (*C.uchar)(unsafe.Pointer(&f.Data[0])), size) == 0 {
			return nil, fmt.Errorf("capture: compressed sample copy failed")
		}
		if len(f.Data) < 2 || f.Data[0] != 0xff || f.Data[1] != 0xd8 {
			return nil, fmt.Errorf("capture: unsupported compressed native sample")
		}
		f.Buffer, f.Format = framebuf.New(f.Data, nil), FormatJPEG
		return f, nil
	}
	if cap(d.frameBufs[d.bufIdx]) < int(size) {
		d.frameBufs[d.bufIdx] = make([]byte, int(size))
	}
	buf := d.frameBufs[d.bufIdx][:int(size)]
	if C.capture_sample_copy(sample, tag, (*C.uchar)(unsafe.Pointer(&buf[0])), size) == 0 {
		return nil, fmt.Errorf("capture: native sample copy failed")
	}
	d.bufIdx = (d.bufIdx + 1) % len(d.frameBufs)
	f.Data = buf
	if tag == cPixfmtUnknown {
		if len(buf) < 2 || buf[0] != 0xff || buf[1] != 0xd8 {
			return nil, fmt.Errorf("capture: unsupported compressed native sample")
		}
		f.Format = FormatJPEG
		if d.shouldDecode() {
			pixels, width, height, err := jpegcodec.DecodeBGRA(buf)
			if err != nil {
				return nil, fmt.Errorf("capture: jpeg decode: %w", err)
			}
			f.Data, f.Width, f.Height, f.Format = pixels, width, height, FormatRaw
		}
	} else if name, ok := cPixfmtName[int(tag)]; ok {
		f.Format, f.PixFmt = FormatYUV, name
	} else {
		f.Format = FormatRaw
	}
	return f, nil
}

// The sample owns its CVPixelBuffer. Native submission retains the buffer;
// releasing this lease cannot invalidate a picture still being encoded.
type darwinFrameBuffer struct {
	sample unsafe.Pointer
	size   C.ulong
	tag    C.int
}

func (b *darwinFrameBuffer) Submit(e videoenc.PixelBufferSubmitter, id uint64, force bool) (bool, error) {
	return e.SubmitPixelBuffer(C.capture_sample_pixel(b.sample), id, force)
}
func (b *darwinFrameBuffer) Materialize(f *Frame) error {
	f.Data = make([]byte, int(b.size))
	if C.capture_sample_copy(b.sample, b.tag, (*C.uchar)(unsafe.Pointer(&f.Data[0])), b.size) == 0 {
		return fmt.Errorf("capture: native fallback copy failed")
	}
	return nil
}
func (b *darwinFrameBuffer) Release() {
	if b.sample != nil {
		C.capture_sample_release(b.sample)
		b.sample = nil
	}
}

func (d *darwinDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		C.avfoundation_stop(d.session)
		d.session = nil
	}
	C.frame_buf_destroy()
	return nil
}

// ListNativeVideoDevices returns AVFoundation video device names.
// When includeIOS is true, warms CMIO DAL and waits for USB iOS mirrors using
// the same path and timing as -video ios-usb (IOS_RESOLVE_WAIT_MS).
func ListNativeVideoDevices(includeIOS bool) []string {
	buf := make([]C.char, 65536)
	inc := C.int(0)
	if includeIOS {
		inc = 1
	}
	C.avfoundation_list_video_devices(&buf[0], 65536, inc)
	raw := C.GoString(&buf[0])
	if raw == "" {
		return nil
	}
	var devices []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		// line is "idx\tname" — take the name.
		if idx := strings.IndexByte(line, '\t'); idx >= 0 {
			devices = append(devices, line[idx+1:])
		}
	}
	return devices
}

func (d *darwinDevice) CaptureFrameRate() framerate.Rate { return d.rate }
