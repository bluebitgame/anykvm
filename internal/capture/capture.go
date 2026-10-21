package capture

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"anykvm/internal/videotiming"
	"errors"
	"image"
	"strconv"
	"time"
)

// ErrStreamStopped means the current capture session cannot produce more frames.
// The owner must close it; repeatedly calling Capture cannot recover it.
var ErrStreamStopped = errors.New("capture stream stopped")

// VideoFormat identifies how a frame's bytes are encoded. A frame carries one
// buffer tagged with its format instead of separate fields per codec.
type VideoFormat uint8

const (
	FormatRaw     VideoFormat = iota // BGRA pixels (Width*Height*4)
	FormatJPEG                       // MJPEG/JPEG bytes
	FormatH264                       // one H.264 Annex-B access unit
	FormatHEVC                       // one HEVC Annex-B access unit
	FormatYUV                        // packed/planar YUV bytes; PixFmt names the exact layout
	FormatAV1                        // one AV1 low-overhead temporal unit
	FormatSurface                    // immutable GPU snapshot, owned by Buffer; Data is empty
)

// String returns a short label for logging.
func (f VideoFormat) String() string {
	switch f {
	case FormatRaw:
		return "raw"
	case FormatJPEG:
		return "jpeg"
	case FormatH264:
		return "h264"
	case FormatHEVC:
		return "hevc"
	case FormatAV1:
		return "av1"
	case FormatYUV:
		return "yuv"
	case FormatSurface:
		return "gpu"
	default:
		return "unknown"
	}
}

// Frame represents one frame of captured video data.
type Frame struct {
	Damage      *jpegcodec.DesktopDamage // Immutable advisory source damage; nil requires full comparison.
	JPEGRegions *jpegcodec.RestartJPEG   // Immutable CPU compressed snapshot, VNC only; Data is empty.
	Timing      *videotiming.Trace
	// Buffer owns immutable Data when non-nil. Frame copies borrow this reference.
	Buffer  *framebuf.Buffer
	Surface *gpuconv.Surface
	// Used only within the native capture -> encoder call. It never escapes to
	// frame servers; the pipe releases its lease after native submission.
	nativeBuffer nativeFrameBuffer
	// CapturedAt is the source presentation time, or earliest available capture
	// time when the device clock cannot be mapped; zero when unavailable.
	CapturedAt                time.Time
	Data                      []byte      // payload bytes, encoding specified by Format
	Format                    VideoFormat // FormatRaw, FormatJPEG, FormatH264, or FormatYUV
	Width                     int
	Height                    int
	Rotation                  int             // clockwise display rotation in degrees; pixels/encoded dimensions remain unchanged
	SourceWidth, SourceHeight int             // content aspect ratio before encoding/padding
	ContentRect               image.Rectangle // desktop content in frame pixels; empty when unspecified
	// PixFmt is the ffmpeg pixel-format name for FormatYUV frames
	// ("uyvy422", "yuyv422", "nv12"). Empty for other formats.
	PixFmt string
}

type nativeFrameBuffer interface {
	Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error)
	Materialize(*Frame) error
	Release()
}

// Device is the capture device interface.
type Device interface {
	Open() error
	OpenWithFormat(width, height, fps int) error
	// Capture transfers a frame reference. Release it after reading; servers
	// must retain owned Buffer (or copy legacy reusable Data) before returning.
	Capture() (*Frame, error)
	Close() error
	// SetDecodeImage controls whether MJPEG capture should decode JPEG to BGRA
	// (Format=FormatRaw) or pass through the compressed bytes (Format=FormatJPEG).
	SetDecodeImage(bool)
	// SetJPEGQuality sets JPEG output quality (1-100), 0=use default.
	// Device-compressed MJPEG is passed through at the device's own quality.
	SetJPEGQuality(int)
}

// OutputSizeConfigurer lets a capture backend scale its output independently
// of OpenWithFormat's preferred source dimensions. Zero clears the override.
// Apply before opening; a different output size requires reopening capture.
type OutputSizeConfigurer interface {
	SetOutputSize(width, height int)
}

// OutputSizePolicyConfigurer restricts output overrides without changing the
// device's selected input mode. Apply before opening; false restores defaults.
type OutputSizePolicyConfigurer interface {
	SetOutputDownscaleOnly(bool)
}

// OutputSizeLimitConfigurer supplies the configured Target ceiling separately
// from a client's changing output request. Zero clears the limit before open.
type OutputSizeLimitConfigurer interface {
	SetOutputSizeLimit(width, height int)
}

// FrameNotifier signals new video output or a terminal capture error. Consumers
// still call Capture to take ownership. The channel must not be closed.
type FrameNotifier interface {
	CaptureReady() <-chan struct{}
}

// GPUSurfaceConfigurer enables immutable GPU capture snapshots for a consumer
// that accepts FormatSurface. Apply before opening, independently of raw codec
// selection; other protocols continue to receive their usual pixel/byte frames.
type GPUSurfaceConfigurer interface{ SetGPUSurface(bool) }

// DesktopDamageConfigurer requests advisory desktop move/dirty metadata.
type DesktopDamageConfigurer interface{ SetDesktopDamage(bool) }

// RDPAVC420Configurer selects full-range BT.709 for the native RDP H.264
// path. Apply before opening; other protocols retain the backend's defaults.
type RDPAVC420Configurer interface {
	SetRDPAVC420(enabled bool)
}

// KeyframeRestartPolicy identifies sender-driven sources that cannot generate
// an IDR by reopening capture. Closing them would disconnect the remote sender.
// Backends without this policy retain the usual capture restart fallback.
type KeyframeRestartPolicy interface {
	CanRestartForKeyframe() bool
}

// jpegQualityToQV maps a JPEG quality (1-100, higher=better) to ffmpeg's mjpeg
// -q:v scale (2-31, LOWER=better). Returns "" when quality <= 0 (use default).
func jpegQualityToQV(quality int) string {
	if quality <= 0 {
		return ""
	}
	if quality > 100 {
		quality = 100
	}
	if quality < 1 {
		quality = 1
	}
	// Linear map: q=100 -> 2 (best), q=1 -> 31 (worst).
	qv := 2 + (100-quality)*(31-2)/99
	return strconv.Itoa(qv)
}

// Release relinquishes this captured frame. Call after all synchronous readers
// finish; asynchronous readers must retain Buffer before Capture is called again.
func (f *Frame) Release() {
	if f != nil {
		f.Buffer.Release()
		f.Buffer = nil
		f.Data = nil
		f.Surface = nil
		f.JPEGRegions = nil
		f.Damage = nil
	}
}

// JPEGRegionsConfigurer selects region-based JPEG capture for the exclusive VNC
// session: compact GPU JPEG readback or raw pixels with desktop damage metadata.
type JPEGRegionsConfigurer interface{ SetJPEGRegions(bool) }
