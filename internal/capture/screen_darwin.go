//go:build darwin

package capture

/*
#cgo LDFLAGS: -framework ScreenCaptureKit -framework VideoToolbox
#include <stdlib.h>
void bk_screen_list_displays(char *out, int outlen);
void *bk_screen_open(int index, unsigned int display_id, int width, int height, int fps, int codec, int bitrate, int hide, int preserve_aspect, int disable_low_latency, int h264_profile, int encoder_buffer_frames, int rdp_full_range, int capture_audio, int downscale_only, int max_w, int max_h, char *error, int error_size);
int bk_screen_audio_read(void *screen, void *out, int capacity, char *error, int error_size);
int bk_screen_take_geometry(void *screen, void **lease, void **data, void **pixel, int *size, int *w, int *h, int *sw, int *sh, int *content_rect, long long *age_ns, char *error, int error_size);
void bk_screen_release(void *lease);
void bk_screen_lease_timing(void *lease, long long *present, long long *display, long long *acquired, long long *submitted, long long *completed, long long *now, int *repeated);
void bk_screen_lease_texture_timing(void *lease, long long *ready_start, long long *ready_end, long long *imported);
int bk_screen_copy_pixel(void *pixel, int nv12, unsigned char *dest, size_t size);
void bk_screen_close(void *screen);
int bk_screen_request_keyframe(void *screen);
int bk_screen_idle_status(void *screen, char *detail, int size);
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"anykvm/internal/videotiming"
	"encoding/binary"
	"fmt"
	"image"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// screenDevice keeps capture and hardware encoding in process. No raw pixels
// cross into Go when H.264/HEVC output is requested.
type screenDevice struct {
	mu                      sync.Mutex // protects the native handle against Capture/Close races
	index                   int
	session                 unsafe.Pointer
	ready                   chan struct{}
	notification            uintptr
	hideCursor, decodeImage bool
	codec                   string
	encoderBufferFrames     int
	quality, fps            int
	bitrate                 int
	bitrateAuto             bool
	streamOptions           *VideoStreamOptions
	display                 unsafe.Pointer // retained across capture restarts
	desktopRequest          *DesktopSize
	preserveAspect          bool
	contentRect             image.Rectangle
	outputW, outputH        int
	outputDownscaleOnly     bool
	outputMaxW, outputMaxH  int
	statsAt                 time.Time
	rdpAVC420               bool
	fullRangeDisabled       bool // retain a failed full-range negotiation across recovery opens
	audioRequested          bool // configured only by the capture-provided Auto audio source
}

func (d *screenDevice) SetRDPAVC420(enabled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rdpAVC420 != enabled {
		d.fullRangeDisabled = false
	}
	d.rdpAVC420 = enabled
}

// ListScreens reports active displays in the same order used by screen:N.
// CoreGraphics enumeration does not need screen-recording permission.
func ListScreens() []Screen {
	const bufferSize = 4096
	buf := make([]C.char, bufferSize)
	C.bk_screen_list_displays(&buf[0], bufferSize)
	var screens []Screen
	for _, line := range strings.Split(C.GoString(&buf[0]), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		index, indexErr := strconv.Atoi(parts[0])
		width, widthErr := strconv.Atoi(parts[1])
		height, heightErr := strconv.Atoi(parts[2])
		if indexErr != nil || widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
			continue
		}
		screens = append(screens, Screen{Index: index, Width: width, Height: height})
	}
	return screens
}

func newScreenDevice(id string) Device {
	return &screenDevice{index: screenIndex(id), decodeImage: true}
}
func (d *screenDevice) SetHideCursor(v bool)                        { d.hideCursor = v }
func (d *screenDevice) SetDecodeImage(v bool)                       { d.mu.Lock(); d.decodeImage = v; d.mu.Unlock() }
func (d *screenDevice) SetJPEGQuality(q int)                        { d.mu.Lock(); d.quality = q; d.mu.Unlock() }
func (d *screenDevice) SetVideoCodec(s string)                      { d.codec = s }
func (d *screenDevice) SetVideoChroma(string)                       {} // hardware encoding uses 4:2:0
func (d *screenDevice) SetVideoStreamOptions(o *VideoStreamOptions) { d.streamOptions = o }
func (d *screenDevice) SetVideoBitrate(kbps int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bitrate = kbps
}
func (d *screenDevice) SetVideoBitrateAuto(auto bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bitrateAuto = auto
}
func (d *screenDevice) SetEncoderBufferFrames(frames int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.encoderBufferFrames = frames
}

func (d *screenDevice) SetOutputSize(w, h int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.outputW, d.outputH = w, h
}
func (d *screenDevice) SetOutputDownscaleOnly(v bool) {
	d.mu.Lock()
	d.outputDownscaleOnly = v
	d.mu.Unlock()
}
func (d *screenDevice) SetOutputSizeLimit(w, h int) {
	d.mu.Lock()
	d.outputMaxW, d.outputMaxH = w, h
	d.mu.Unlock()
}
func (d *screenDevice) SupportsVideoCodec(s string) bool {
	if s == "hevc" {
		return d.SupportsGameStreamHEVC()
	}
	return s == "" || s == "mjpeg" || s == "rawvideo" || s == "nv12" || s == "h264" || s == "hevc"
}
func (d *screenDevice) CaptureFPS() int         { return d.fps }
func (d *screenDevice) CanRecoverCapture() bool { return true }
func (d *screenDevice) Open() error             { return d.OpenWithFormat(0, 0, 0) }

// AudioDevice shares the screen's SCStream, rather than opening a microphone.
// It is selected before Open only for Audio: Auto. Reopens retain the request.
func (d *screenDevice) AudioDevice() AudioDevice {
	d.mu.Lock()
	d.audioRequested = true
	d.mu.Unlock()
	return &screenAudioDevice{screen: d}
}

type screenAudioDevice struct {
	screen *screenDevice
	mu     sync.Mutex
	opened bool
}

func (a *screenAudioDevice) Open() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.screen.mu.Lock()
	defer a.screen.mu.Unlock()
	if a.screen.session == nil {
		return ErrStreamStopped
	}
	var message [512]C.char
	if C.bk_screen_audio_read(a.screen.session, nil, 0, &message[0], C.int(len(message))) < 0 {
		return fmt.Errorf("ScreenCaptureKit audio: %s", C.GoString(&message[0]))
	}
	a.opened = true
	return nil
}

func (a *screenAudioDevice) Capture() (*AudioFrame, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.opened {
		return nil, ErrStreamStopped
	}
	d := a.screen
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		// Video recovery closes and reopens this handle without recreating the
		// audio reader. Keep it alive until Close explicitly stops this source.
		return nil, nil
	}
	var data [960 * 2 * 4]byte // Up to 20ms of stereo Float32.
	var message [512]C.char
	n := int(C.bk_screen_audio_read(d.session, unsafe.Pointer(&data[0]), C.int(len(data)), &message[0], C.int(len(message))))
	if n < 0 {
		return nil, fmt.Errorf("ScreenCaptureKit audio: %s", C.GoString(&message[0]))
	}
	if n == 0 {
		return nil, nil
	}
	return &AudioFrame{PCM: screenAudioPCM(data[:n]), SampleRate: AudioSampleRate, Channels: AudioChannels}, nil
}

func (a *screenAudioDevice) Close() error {
	a.mu.Lock()
	a.opened = false
	a.mu.Unlock()
	return nil // Video owns the shared stream's lifecycle.
}

func screenAudioPCM(data []byte) []byte {
	pcm := make([]byte, len(data)/4*2)
	for i := 0; i+4 <= len(data); i += 4 {
		f := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[i:])))
		if math.IsNaN(f) {
			f = 0
		}
		f = math.Max(-1, math.Min(1, f))
		n := int16(math.Round(math.Max(-32768, math.Min(32767, f*32768))))
		binary.LittleEndian.PutUint16(pcm[i/2:], uint16(n))
	}
	return pcm
}

func (d *screenDevice) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		return fmt.Errorf("ScreenCaptureKit: device already open")
	}
	if d.outputW > 0 && d.outputH > 0 {
		w, h = d.outputW, d.outputH
	}
	bitrate := defaultH264BitrateKbps
	if d.bitrateAuto {
		bitrate = 0
	} else if d.bitrate > 0 {
		bitrate = d.bitrate
	}
	if o := d.streamOptions; o != nil {
		w, h, fps, bitrate = o.Width, o.Height, o.FPS, o.Bitrate
		d.codec = o.Codec
		if d.codec == "" {
			d.codec = "h264"
		}
	}
	if !d.SupportsVideoCodec(d.codec) {
		return fmt.Errorf("unsupported screen codec %q", d.codec)
	}
	if d.outputDownscaleOnly && (d.codec == "h264" || d.codec == "hevc") &&
		(d.outputMaxW > 0 && d.outputMaxW < 2 || d.outputMaxH > 0 && d.outputMaxH < 2) {
		return fmt.Errorf("ScreenCaptureKit: Target resolution limit is too small for video encoding")
	}
	if w < 0 || h < 0 || w > 8192 || h > 8192 || fps < 0 || fps > 240 || bitrate < 0 {
		return fmt.Errorf("ScreenCaptureKit: invalid dimensions, frame rate or bitrate")
	}
	// The native API accepts a signed 32-bit kbps value. Do not impose a
	// hardware bitrate ceiling here; VideoToolbox validates its capabilities.
	if bitrate > math.MaxInt32 {
		return fmt.Errorf("ScreenCaptureKit: bitrate exceeds native integer range")
	}
	codec := 0
	if d.codec == "h264" {
		codec = 1
	} else if d.codec == "hevc" {
		codec = 2
	} else if d.codec == "nv12" {
		codec = 3 // Native video-range NV12 pixels, without VideoToolbox encoding.
	}
	encoderFrames, err := screenEncoderFrameLimit(d.codec, d.encoderBufferFrames)
	if err != nil {
		return err
	}
	preserveAspect := 0
	if d.streamOptions != nil || d.desktopRequest != nil || d.outputW > 0 {
		preserveAspect = 1
	}
	hide := 0
	if d.hideCursor {
		hide = 1
	}
	disableLowLatency := C.int(0)
	if d.streamOptions != nil && d.streamOptions.DisableLowLatencyRateControl {
		disableLowLatency = 1
	}
	profileIndex := C.int(0)
	profile := "constrained_baseline"
	if d.streamOptions != nil && d.streamOptions.H264Profile != "" {
		profile = d.streamOptions.H264Profile
	}
	switch profile {
	case "constrained_baseline":
	case "baseline":
		profileIndex = 1
	case "main":
		profileIndex = 2
	case "high":
		profileIndex = 3
	default:
		return fmt.Errorf("invalid H.264 profile %q", profile)
	}
	var message [1024]C.char
	fullRange := C.int(0)
	if d.rdpAVC420 && codec == 1 && d.streamOptions == nil && !d.fullRangeDisabled {
		fullRange = 1
	}
	open := func(full C.int) unsafe.Pointer {
		audio := C.int(0)
		if d.audioRequested {
			audio = 1
		}
		downscaleOnly := C.int(0)
		if d.outputDownscaleOnly {
			downscaleOnly = 1
		}
		return C.bk_screen_open(C.int(d.index), C.uint(screenDisplayID(d.display)), C.int(w), C.int(h), C.int(fps), C.int(codec), C.int(bitrate), C.int(hide), C.int(preserveAspect), disableLowLatency, profileIndex, C.int(encoderFrames), full, audio, downscaleOnly, C.int(d.outputMaxW), C.int(d.outputMaxH), &message[0], C.int(len(message)))
	}
	d.session = open(fullRange)
	if d.session == nil && fullRange != 0 {
		log.Printf("capture: RDP full-range startup failed: %s; falling back to existing video-range path", C.GoString(&message[0]))
		d.session = open(0)
		// Permission/display failures can affect both formats. Cache a format
		// fallback only when the original path actually opened successfully.
		d.fullRangeDisabled = d.session != nil
	}
	if d.session == nil {
		return fmt.Errorf("ScreenCaptureKit: %s", C.GoString(&message[0]))
	}
	d.attachFrameReadyLocked()
	d.preserveAspect = preserveAspect != 0
	d.contentRect = image.Rectangle{}
	d.fps = fps
	if codec == 1 || codec == 2 {
		log.Printf("capture: native buffer_frames=%d (configured=%d)", encoderFrames, d.encoderBufferFrames)
	}
	d.statsAt = time.Now()
	encoder := jpegcodec.Backend()
	switch d.codec {
	case "h264", "hevc":
		encoder = "VideoToolbox (hardware)"
	case "rawvideo", "nv12":
		encoder = "none (pixels)"
	}
	cadence := "native"
	if fps > 0 {
		cadence = strconv.Itoa(fps)
	}
	log.Printf("capture: ScreenCaptureKit screen:%d codec=%s encoder=%q fps=%s", d.index, d.codec, encoder, cadence)
	if d.codec == "h264" {
		if bitrate == 0 {
			log.Printf("capture: encoder bitrate=auto lowdelay=%t profile=%s", disableLowLatency == 0, profile)
		} else {
			log.Printf("capture: encoder bitrate=%d kbps lowdelay=%t profile=%s", bitrate, disableLowLatency == 0, profile)
		}
	}
	return nil
}
func (d *screenDevice) Capture() (*Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil, nil
	}
	// Include native counters even while frames are arriving: idle-only
	// diagnostics cannot distinguish a low source rate from encoder drops.
	if time.Since(d.statsAt) >= 5*time.Second {
		var detail [512]C.char
		C.bk_screen_idle_status(d.session, &detail[0], C.int(len(detail)))
		log.Printf("capture: screen:%d counters (cumulative): %s", d.index, C.GoString(&detail[0]))
		d.statsAt = time.Now()
	}
	var lease, data, pixel unsafe.Pointer
	var size, w, h, sw, sh C.int
	var rect [4]C.int
	var age C.longlong
	var message [1024]C.char
	result := C.bk_screen_take_geometry(d.session, &lease, &data, &pixel, &size, &w, &h, &sw, &sh, &rect[0], &age, &message[0], C.int(len(message)))
	// Translate the host-clock PTS into Go's monotonic clock before copying pixels.
	capturedAt := time.Now().Add(-time.Duration(age))
	if result < 0 {
		if result == -2 {
			// Full-range verification failed. The bridge will reopen
			// this device with video-range pixels.
			d.fullRangeDisabled = true
			log.Printf("capture: RDP full-range verification failed: %s; next open uses video range", C.GoString(&message[0]))
		}
		return nil, fmt.Errorf("%w: ScreenCaptureKit: %s", ErrStreamStopped, C.GoString(&message[0]))
	}
	if result == 0 {
		return nil, nil
	}
	defer func() {
		if lease != nil {
			C.bk_screen_release(lease)
		}
	}()
	if size <= 0 || size > 256*1024*1024 {
		return nil, fmt.Errorf("ScreenCaptureKit: invalid frame size")
	}
	f := &Frame{CapturedAt: capturedAt, Width: int(w), Height: int(h), SourceWidth: int(sw), SourceHeight: int(sh), Format: FormatRaw}
	if videotiming.Sample() {
		var present, display, acquired, submitted, completed, now C.longlong
		var readyStart, readyEnd, imported C.longlong
		var repeated C.int
		C.bk_screen_lease_texture_timing(lease, &readyStart, &readyEnd, &imported)
		C.bk_screen_lease_timing(lease, &present, &display, &acquired, &submitted, &completed, &now, &repeated)
		anchor := time.Now()
		mapTime := func(ns C.longlong) time.Time {
			if ns == 0 {
				return time.Time{}
			}
			return anchor.Add(time.Duration(ns - now))
		}
		path := "ScreenCaptureKit/pixels"
		if d.codec == "h264" || d.codec == "hevc" {
			path = "ScreenCaptureKit/VideoToolbox/" + d.codec
		}
		trace := &videotiming.Trace{Present: mapTime(present), Display: mapTime(display), SourceClock: "SCStream.synchronizationClock", Acquired: mapTime(acquired), TextureReadyStart: mapTime(readyStart), TextureReadyEnd: mapTime(readyEnd), TextureImported: mapTime(imported), EncodeSubmit: mapTime(submitted), EncodeComplete: mapTime(completed), Path: path, Repeated: repeated != 0}
		defer func() {
			trace.Ready = time.Now()
			f.Timing = trace
			trace.Capture()
		}()
	}
	f.ContentRect = image.Rect(int(rect[0]), int(rect[1]), int(rect[0]+rect[2]), int(rect[1]+rect[3]))
	actualContent := !f.ContentRect.Empty()
	if !actualContent {
		f.ContentRect = screenContentRect(f.Width, f.Height, f.SourceWidth, f.SourceHeight, d.preserveAspect)
	}
	if f.ContentRect != d.contentRect {
		log.Printf("capture: screen:%d frame=%dx%d source=%dx%d content=%v metadata=%t", d.index, f.Width, f.Height, f.SourceWidth, f.SourceHeight, f.ContentRect, actualContent)
		d.contentRect = f.ContentRect
	}
	if pixel != nil && !d.decodeImage && d.codec != "rawvideo" && d.codec != "nv12" {
		bytes, err := jpegcodec.EncodePixelBufferOwned(pixel, d.quality)
		if err != nil {
			return nil, err
		}
		f.Buffer, f.Format = bytes, FormatJPEG
		f.Data = bytes.Bytes()
		return f, nil
	}
	if pixel != nil {
		f.Data = make([]byte, int(size))
		nv12 := C.int(0)
		if d.codec == "nv12" {
			nv12 = 1
		}
		if C.bk_screen_copy_pixel(pixel, nv12, (*C.uchar)(unsafe.Pointer(&f.Data[0])), C.size_t(size)) == 0 {
			return nil, fmt.Errorf("ScreenCaptureKit: pixel copy failed")
		}
	} else {
		if data == nil {
			return nil, fmt.Errorf("ScreenCaptureKit: missing frame data")
		}
		f.Data = unsafe.Slice((*byte)(data), int(size))
		owned := lease
		f.Buffer = framebuf.New(f.Data, func() { C.bk_screen_release(owned) })
		lease = nil
	}
	if f.Buffer == nil {
		f.Buffer = framebuf.New(f.Data, nil)
	}
	switch d.codec {
	case "h264":
		f.Format = FormatH264
	case "hevc":
		f.Format = FormatHEVC
	case "nv12":
		f.Format, f.PixFmt = FormatYUV, "nv12"
	}
	return f, nil
}
func (d *screenDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		d.detachFrameReadyLocked()
		C.bk_screen_close(d.session)
		d.session = nil
	}
	return nil
}

// CaptureIdleStatus distinguishes a static desktop from an encoder that has
// stopped returning submitted frames. Called only after a prolonged idle read.
func (d *screenDevice) CaptureIdleStatus() (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return false, "ScreenCaptureKit closed"
	}
	var detail [1024]C.char
	expected := C.bk_screen_idle_status(d.session, &detail[0], C.int(len(detail)))
	return expected != 0, C.GoString(&detail[0])
}

func (d *screenDevice) RequestKeyframe() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil || C.bk_screen_request_keyframe(d.session) == 0 {
		return fmt.Errorf("ScreenCaptureKit: encoder unavailable for keyframe request")
	}
	return nil
}

// Query the system hardware declaration without creating a capture/encode session.
func (d *screenDevice) SupportsGameStreamHEVC() bool {
	return videoenc.SupportsHEVC()
}

// AVFoundation inputs share the system HEVC hardware declaration.
func (s *smartDevice) SupportsGameStreamHEVC() bool {
	return (&screenDevice{}).SupportsGameStreamHEVC()
}
