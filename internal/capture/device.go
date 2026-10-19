package capture

import (
	"anykvm/internal/jpegcodec"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
)

// errNoMJPEG is returned by a native backend's OpenWithFormat when the device
// does not support MJPEG output. It signals the fallback wrapper to switch to
// the ffmpeg backend (which transcodes any input into MJPEG).
var errNoMJPEG = errors.New("capture: device does not support MJPEG")

// errFFmpegUnavailable means the optional FFmpeg shared libraries cannot load.
var errFFmpegUnavailable = errors.New("capture: ffmpeg unavailable")

// deviceCaps describes a capture device's chosen capture mode, determined once
// at startup by probing the device without holding it or prompting for camera
// permission.
type deviceCaps struct {
	width  int  // chosen capture width
	height int  // chosen capture height
	fps    int  // chosen capture frame rate
	mjpeg  bool // whether the chosen mode outputs MJPEG (vs raw)
	h264   bool // whether the device can output H.264 natively (passthrough)
	ok     bool // whether probing succeeded
}

// capabilityProber is optionally implemented by native backends to report the
// device's supported formats without starting a capture session. wantW/wantH/
// wantFPS express the preferred (configured) mode; the probe returns the best
// supported mode (preferring the requested resolution and MJPEG).
type capabilityProber interface {
	ProbeCaps(wantW, wantH, wantFPS int) (deviceCaps, error)
}

// fpsReporter is optionally implemented by capture devices that know their
// negotiated capture frame rate after Open. Returns 0 when unknown.
type fpsReporter interface {
	CaptureFPS() int
}

// NewDevice creates a capture device instance.
//
// deviceID: on macOS a device index or name, on Linux a /dev/video* path.
//
// On platforms with a native backend (macOS/Linux) this returns a smart device
// wrapper: probes the capture card's supported formats (resolution/frame rate/
// MJPEG capability) once at startup and caches the result; afterwards:
//   - If the device outputs MJPEG natively, use the native backend (zero-transcode);
//   - macOS encodes raw inputs with system ImageIO/VideoToolbox APIs;
//   - Other platforms try FFmpeg at the probed resolution/rate when needed.
//
// Probing does not start a capture session, so the device is not held, camera
// permission is not triggered, and repeated reopen-relocate cycles on the camera
// are avoided.
func NewDevice(deviceID string) Device {
	// Local screen capture mode: "screen" or "screen:N" (N = display index).
	// This captures the machine running AnyKVM (local-machine-as-target),
	// uses ScreenCaptureKit on macOS and ffmpeg on other platforms.
	if isScreenDevice(deviceID) {
		return newScreenDevice(deviceID)
	}
	if nativeIsFFmpeg {
		// Platform backend owns capture/encoding selection (Windows or Linux).
		return newNativeDevice(deviceID)
	}
	return &smartDevice{
		deviceID:  deviceID,
		captureID: deviceID,
		native:    newNativeDevice(deviceID),
	}
}

// SetCaptureMode forwards explicit input choices to the native backend.
func (s *smartDevice) SetCaptureMode(value string) error {
	s.explicitMode = strings.TrimSpace(value) != ""
	if setter, ok := s.native.(interface{ SetCaptureMode(string) error }); ok {
		return setter.SetCaptureMode(value)
	}
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	if mode.PixelFormat != "" {
		return fmt.Errorf("capture format selection is unavailable on this platform")
	}
	return nil
}

// IsIOSDevice reports whether deviceID selects AVFoundation iOS USB mirroring
// ("ios-usb" or "ios-usb:<name>"). CoreDevice is a separate capture backend.
func IsIOSDevice(deviceID string) bool {
	return isIOSDevice(deviceID)
}

// isIOSDevice reports whether deviceID selects an iOS USB screen-mirroring
// device: "ios-usb" (first iPhone/iPad) or "ios-usb:<name>" (localizedName substring
// match, e.g. "ios-usb:iPad"). macOS only; resolved lazily on each Open so a
// device plugged in later is picked up without restarting.
func isIOSDevice(deviceID string) bool {
	return deviceID == "ios-usb" || strings.HasPrefix(deviceID, "ios-usb:")
}

// iosSelector extracts the optional name filter from an "ios-usb[:name]" device id.
func iosSelector(deviceID string) string {
	if i := strings.IndexByte(deviceID, ':'); i >= 0 {
		return deviceID[i+1:]
	}
	return ""
}

// isScreenDevice reports whether deviceID selects local screen capture.
func isScreenDevice(deviceID string) bool {
	return deviceID == "screen" || strings.HasPrefix(deviceID, "screen:")
}

// screenIndex parses the display index from a "screen[:N]" device id (default 0).
func screenIndex(deviceID string) int {
	if i := strings.IndexByte(deviceID, ':'); i >= 0 {
		var n int
		if _, err := fmt.Sscanf(deviceID[i+1:], "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// smartDevice picks between the native backend and the ffmpeg backend based on
// a one-time capability probe, then delegates to the chosen underlying device.
type smartDevice struct {
	explicitMode bool
	devMu        sync.Mutex // guards the active backend during Capture/Close
	deviceID     string     // configured device id (may be the special "ios-usb[:name]")
	captureID    string     // concrete device name/index used by the backends
	native       Device
	dev          Device // active underlying device (native or ffmpeg), set on Open

	streamOptions *VideoStreamOptions
	caps          deviceCaps
	decodeImage   bool
	jpegQuality   int    // locked MJPEG output quality (0 = default)
	videoCodec    string // desired output codec: "" / "mjpeg" (default), "h264", or "rawvideo"
	videoBitrate  int
	videoChroma   string // H.264 chroma: "" / "420" or "422"
}

func (s *smartDevice) Open() error {
	return s.OpenWithFormat(0, 0, 30)
}

func (s *smartDevice) OpenWithFormat(width, height, fps int) error {
	// Special "ios-usb[:name]" device: resolve to the concrete iOS mirroring
	// device's name on every open (the device appears/disappears with the USB
	// connection), then route through the iOS-specific backend selection.
	// Failure is returned to the caller (bridge logs it and shows the
	// placeholder; the next client connect retries).
	if isIOSDevice(s.deviceID) {
		name, err := resolveIOSDevice(iosSelector(s.deviceID))
		if err != nil {
			return fmt.Errorf("capture: %w", err)
		}
		if name != s.captureID {
			log.Printf("capture: ios device resolved to %q", name)
			s.captureID = name
			s.native = newNativeDevice(name)
		}
		return s.openIOS(width, height, fps)
	}

	// Probe device capabilities once (no session, no permission prompt).
	if !s.caps.ok {
		if prober, ok := s.native.(capabilityProber); ok {
			var err error
			s.caps, err = prober.ProbeCaps(width, height, fps)
			if err != nil {
				if s.explicitMode {
					return err
				}
				s.caps = s.probe(width, height, fps)
			}
		} else {
			s.caps = s.probe(width, height, fps)
		}
	}

	// Determine the capture mode to request. Prefer probed values; fall back to
	// the requested ones (or ffmpeg defaults) when probing is unavailable.
	w, h, r := width, height, fps
	if s.caps.ok {
		w, h, r = s.caps.width, s.caps.height, s.caps.fps
		log.Printf("capture: device caps %dx%d@%d mjpeg=%v h264=%v", w, h, r, s.caps.mjpeg, s.caps.h264)
	}
	// AVFoundation plus ImageIO/VideoToolbox covers normal Mac inputs without
	// FFmpeg. Try it first even when optional libraries are installed.
	if runtime.GOOS == "darwin" && s.videoCodec != "av1" {
		if err := s.openNativeMedia(w, h, r); err == nil {
			return nil
		} else if s.explicitMode || !ffmpegAvailable() {
			return err
		} else {
			log.Printf("capture: native media failed (%v); trying FFmpeg", err)
		}
	}

	// AV1 requires a verified GPU encoder. Do not silently substitute another
	// codec after the client has negotiated AV1.
	if s.videoCodec == "av1" {
		return s.openFFmpegCodec(w, h, r, "av1")
	}

	// H.264 output requested by the active consumer.
	if s.videoCodec == "h264" {
		// Native H.264 passthrough when the device supports it (zero transcode,
		// mirrors native MJPEG passthrough).
		if s.caps.ok && s.caps.h264 {
			if vc, ok := s.native.(interface{ SetVideoCodec(string) }); ok {
				vc.SetVideoCodec("h264")
				s.native.SetDecodeImage(false)
				if err := s.native.OpenWithFormat(w, h, r); err == nil {
					s.setDevice(s.native)
					log.Printf("capture: native H.264 passthrough")
					return nil
				} else {
					log.Printf("capture: native H.264 failed (%v); falling back to ffmpeg", err)
				}
			}
		}
		// Otherwise transcode to H.264 via ffmpeg (single encode from the
		// device output).
		if ffmpegAvailable() {
			if err := s.openFFmpegCodec(w, h, r, "h264"); err == nil {
				log.Printf("capture: ffmpeg H.264 encode")
				return nil
			} else {
				log.Printf("capture: ffmpeg H.264 failed (%v); using MJPEG", err)
			}
		}
		// Fall through to MJPEG if H.264 is unavailable.
	}

	// UYVY output requested: for GUI with GPU-side YUV→RGB conversion.
	// macOS native backend supports direct YUV passthrough (no ffmpeg);
	// other platforms use ffmpeg with uyvy422 pixel format.
	if s.videoCodec == "uyvy" {
		if yp, ok := s.native.(yuvPassthroughSetter); ok {
			yp.SetYUVPassthrough(true)
			s.native.SetDecodeImage(false)
			if e := s.native.OpenWithFormat(w, h, r); e == nil {
				s.setDevice(s.native)
				log.Printf("capture: native YUV passthrough")
				return nil
			} else {
				log.Printf("capture: native YUV failed (%v); falling back to ffmpeg UYVY", e)
				yp.SetYUVPassthrough(false)
			}
		}
		if ffmpegAvailable() {
			if err := s.openFFmpegCodec(w, h, r, "uyvy"); err == nil {
				log.Printf("capture: ffmpeg UYVY output")
				return nil
			} else {
				log.Printf("capture: ffmpeg UYVY failed (%v); using rawvideo", err)
			}
		}
		// Fall through to rawvideo if uyvy is unavailable.
		s.videoCodec = "rawvideo"
	}

	// Rawvideo output requested: ffmpeg emits uncompressed BGRA pixels, avoiding
	// the JPEG encode→pipe→decode cycle when the client needs raw pixels.
	if s.videoCodec == "rawvideo" {
		if ffmpegAvailable() {
			if err := s.openFFmpegCodec(w, h, r, "rawvideo"); err == nil {
				log.Printf("capture: ffmpeg rawvideo output")
				return nil
			} else {
				log.Printf("capture: ffmpeg rawvideo failed (%v); using MJPEG", err)
			}
		}
		// Fall through to MJPEG if rawvideo is unavailable.
	}

	// If the device natively provides MJPEG, use the native backend (no
	// transcoding). Otherwise use ffmpeg to transcode raw -> MJPEG at the
	// supported resolution/fps.
	if s.caps.ok && !s.caps.mjpeg && ffmpegAvailable() {
		if err := s.openFFmpeg(w, h, r); err == nil {
			return nil
		} else {
			log.Printf("capture: ffmpeg backend failed (%v); using native backend", err)
		}
	}

	// Native backend path (MJPEG). Ensure any prior H.264 selection is cleared.
	if vc, ok := s.native.(interface{ SetVideoCodec(string) }); ok {
		vc.SetVideoCodec("")
	}
	s.native.SetDecodeImage(s.decodeImage)
	if err := s.native.OpenWithFormat(w, h, r); err != nil {
		// Native reports it cannot do MJPEG -> try ffmpeg as a last resort.
		if errors.Is(err, errNoMJPEG) && ffmpegAvailable() {
			if ferr := s.openFFmpeg(w, h, r); ferr == nil {
				return nil
			}
		}
		return err
	}
	s.setDevice(s.native)
	return nil
}

// yuvPassthroughSetter is implemented by native backends that can deliver
// device YUV frames unconverted (for the pipe encoder).
type yuvPassthroughSetter interface{ SetYUVPassthrough(bool) }

// setYUVPassthrough toggles YUV passthrough on dev when supported.
func setYUVPassthrough(dev Device, on bool) {
	if yp, ok := dev.(yuvPassthroughSetter); ok {
		yp.SetYUVPassthrough(on)
	}
}

// openIOS skips probing: DAL mirroring devices report no formats. Encode
// their native frames with the same system APIs as other Mac inputs.
func (s *smartDevice) openIOS(width, height, fps int) error {
	return s.openNativeMedia(width, height, fps)
}

func (s *smartDevice) openNativeMedia(width, height, fps int) error {
	codec := s.videoCodec
	if s.streamOptions != nil {
		codec = s.streamOptions.Codec
		if codec == "" {
			codec = "h264"
		}
	}
	switch codec {
	case "h264", "hevc":
		// VideoToolbox accepts the native YUV layout through the in-process
		// encoder. Decode device JPEGs only when transcoding is requested.
		setYUVPassthrough(s.native, true)
		s.native.SetDecodeImage(true)
		pe := newPipeEncoderDevice(s.native, codec, fps)
		pe.enc.SetVideoStreamOptions(s.streamOptions)
		pe.enc.SetVideoBitrate(s.videoBitrate)
		pe.enc.scalableInput = !s.PreserveCaptureMode()
		pe.enc.variableInputRate = isIOSDevice(s.deviceID)
		pe.SetDecodeImage(true)
		if err := pe.OpenWithFormat(width, height, fps); err != nil {
			return err
		}
		s.setDevice(pe)
		log.Printf("capture: native capture + VideoToolbox %s", codec)
	case "rawvideo", "uyvy":
		setYUVPassthrough(s.native, codec == "uyvy")
		s.native.SetDecodeImage(codec == "rawvideo")
		if err := s.native.OpenWithFormat(width, height, fps); err != nil {
			return err
		}
		s.setDevice(s.native)
	case "", "mjpeg":
		setYUVPassthrough(s.native, false)
		j := &jpegEncodeDevice{Device: s.native, quality: s.jpegQuality}
		j.SetDecodeImage(s.decodeImage)
		j.SetJPEGQuality(s.jpegQuality)
		if err := j.OpenWithFormat(width, height, fps); err != nil {
			return err
		}
		s.setDevice(j)
		log.Printf("capture: native capture + %s (device JPEG passthrough)", jpegcodec.Backend())
	default:
		return fmt.Errorf("capture: unsupported native codec %q", codec)
	}
	return nil
}

// SupportsVideoCodec reports whether the device can produce the given capture
// output codec. The bridge consults this before switching codecs, so an
// unsupported codec never triggers a pointless capture reopen.
func (s *smartDevice) SupportsVideoCodec(codec string) bool {
	if codec == "av1" {
		return s.SupportsGameStreamAV1()
	}
	if isIOSDevice(s.deviceID) && codec == "h264" {
		return true // Native VideoToolbox can encode iOS frames without FFmpeg libraries.
	}
	return true
}

// openFFmpeg starts the ffmpeg backend in MJPEG mode at the given (supported) mode.
func (s *smartDevice) openFFmpeg(w, h, fps int) error {
	return s.openFFmpegCodec(w, h, fps, "")
}

// openFFmpegCodec starts the ffmpeg backend with the given output codec
// ("" = MJPEG, "h264" = Annex-B H.264, "rawvideo" = uncompressed BGRA).
func (s *smartDevice) openFFmpegCodec(w, h, fps int, codec string) error {
	ff := NewFFmpegDevice(s.captureID)
	ff.SetDecodeImage(s.decodeImage && codec == "")
	ff.SetJPEGQuality(s.jpegQuality)
	if encoder, ok := ff.(VideoBitrateConfigurer); ok {
		encoder.SetVideoBitrate(s.videoBitrate)
	}
	if configurer, ok := ff.(VideoStreamConfigurer); ok {
		configurer.SetVideoStreamOptions(s.streamOptions)
	}
	if vc, ok := ff.(interface{ SetVideoCodec(string) }); ok {
		vc.SetVideoCodec(codec)
	}
	if cc, ok := ff.(interface{ SetVideoChroma(string) }); ok {
		cc.SetVideoChroma(s.videoChroma)
	}
	if err := ff.OpenWithFormat(w, h, fps); err != nil {
		ff.Close()
		return err
	}
	s.setDevice(ff)
	return nil
}

// probe queries the native backend's capabilities if it supports probing.
func (s *smartDevice) probe(wantW, wantH, wantFPS int) deviceCaps {
	prober, ok := s.native.(capabilityProber)
	if !ok {
		return deviceCaps{} // ok=false: caller uses requested/default mode
	}
	caps, err := prober.ProbeCaps(wantW, wantH, wantFPS)
	if err != nil {
		log.Printf("capture: capability probe failed: %v", err)
		return deviceCaps{}
	}
	return caps
}

func (s *smartDevice) setDevice(dev Device) {
	s.devMu.Lock()
	s.dev = dev
	s.devMu.Unlock()
}

func (s *smartDevice) activeBackend() Device {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	return s.dev
}

func (s *smartDevice) Capture() (*Frame, error) {
	dev := s.activeBackend()
	if dev == nil {
		return nil, fmt.Errorf("capture: device not open")
	}
	return dev.Capture()
}

// CaptureFPS reports the negotiated capture frame rate: the probed device rate
// when available, otherwise the active backend's reported rate. Returns 0 when
// unknown.
func (s *smartDevice) CaptureFPS() int {
	if s.caps.ok && s.caps.fps > 0 {
		return s.caps.fps
	}
	if r, ok := s.activeBackend().(fpsReporter); ok {
		return r.CaptureFPS()
	}
	return 0
}

func (s *smartDevice) Close() error {
	s.devMu.Lock()
	dev := s.dev
	s.dev = nil
	s.devMu.Unlock()
	if dev != nil {
		return dev.Close()
	}
	return nil
}

func (s *smartDevice) SetDecodeImage(v bool) {
	s.decodeImage = v
	if dev := s.activeBackend(); dev != nil {
		dev.SetDecodeImage(v)
	}
}

// SetJPEGQuality records the locked MJPEG quality and updates an active encoder.
func (s *smartDevice) SetJPEGQuality(q int) {
	s.jpegQuality = q
	if dev := s.activeBackend(); dev != nil {
		dev.SetJPEGQuality(q)
	}
}

// SetVideoCodec records the desired output codec ("" / "mjpeg" or "h264").
// Takes effect on the next Open (the bridge reopens the device on switch).
func (s *smartDevice) SetVideoCodec(codec string) {
	s.videoCodec = codec
}

// SetVideoChroma records the desired H.264 chroma ("" / "420" or "422").
// Takes effect on the next Open.
func (s *smartDevice) SetVideoChroma(chroma string) {
	s.videoChroma = chroma
}
