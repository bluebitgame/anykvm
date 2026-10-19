// Package videoenc provides in-process hardware video encoding. It does not
// execute FFmpeg. Windows calls NVENC, AMF and Intel VPL directly in native-only
// builds and through libavcodec with the ffmpeg tag; Media Foundation stays
// native. Linux native builds use Vulkan Video H.264; Raspberry Pi uses V4L2.
// Optional libavcodec also supplies Linux GPU backends and x264 fallbacks. Each Encode returns an Annex-B access unit or
// low-overhead AV1 temporal unit.
package videoenc

import (
	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"errors"
	"fmt"
	"unsafe"
)

var ErrUnavailable = errors.New("native hardware encoder unavailable")

type Options struct {
	// Windows desktop output identity. Set distinguishes an exact zero LUID
	// from unconstrained device/camera encoding. Value fields keep Options comparable.
	WindowsAdapter WindowsAdapter
	// DRM primary-device identity supplied by native Linux capture. Zero lets
	// Vulkan select a device; screen capture always supplies an exact match.
	DRMDeviceMajor, DRMDeviceMinor uint32
	H264Profile                    string         // Native VideoToolbox: empty/constrained_baseline, baseline, main or high.
	DisableLowLatencyRateControl   bool           // Native VideoToolbox H.264 only; zero keeps low-latency rate control enabled.
	Rate                           framerate.Rate // Exact rate; takes precedence over the integer FPS fallback.
	Width, Height, FPS, Bitrate    int            // bitrate in kbps; zero is allowed when OmitBitrate is true
	OmitBitrate                    bool           // do not set a target bitrate; let the platform encoder choose
	Codec                          string         // h264, hevc or av1; availability depends on the platform and GPU
}

type WindowsAdapter struct {
	LUID     uint64
	VendorID uint32
	Set      bool
}

func (a WindowsAdapter) String() string {
	if !a.Set {
		return "unbound"
	}
	return fmt.Sprintf("%08x:%08x/vendor=%04x", uint32(a.LUID>>32), uint32(a.LUID), a.VendorID)
}

type Encoder interface {
	Encode(nv12 []byte, forceIDR bool) ([]byte, error)
	Close()
	Name() string
}

// Packet identifies the submitted picture whose encoding has completed.
// Dropped pictures have no Data; callers should request a new IDR.
// Receive transfers ownership to the caller, who must call Release.
type Packet struct {
	Buffer  *framebuf.Buffer
	ID      uint64
	Data    []byte
	Dropped bool
}

// AsyncEncoder bounds outstanding work and never waits for a picture to finish
// during Submit. False means the unencoded input was not accepted. Receive is
// nonblocking and returns nil when no completion is available.
type AsyncEncoder interface {
	Encoder
	Submit(data []byte, width, height int, pixfmt string, id uint64, forceIDR bool) (bool, error)
	Receive() (*Packet, error)
}

// PixelBufferSubmitter borrows a native CVPixelBuffer during SubmitPixelBuffer.
// An accepted submission retains native storage until encoding completes.
type PixelBufferSubmitter interface {
	SubmitPixelBuffer(pixel unsafe.Pointer, id uint64, forceIDR bool) (bool, error)
}

// D3D11TextureEncoder borrows a keyed-mutex shared BGRA texture for synchronous
// GPU conversion and encoding. handled=false permits CPU readback fallback.
// The caller keeps the texture alive until this method returns.
type D3D11TextureEncoder interface {
	EncodeD3D11Texture(handle uintptr, rotation int, forceIDR bool) (data []byte, handled bool, err error)
}

// MediaFoundationSampleEncoder borrows a native NV12 IMFSample until the call
// returns. The transform retains any input storage it needs afterwards.
type MediaFoundationSampleEncoder interface {
	EncodeMediaFoundationSample(unsafe.Pointer, bool) ([]byte, bool, error)
}

// DMABUFEncoder borrows a captured image until synchronous encoding ends.
// Supported RGB/NV12 formats and layouts depend on the native backend.
// handled=false permits explicit pixel readback fallback.
type DMABUFEncoder interface {
	EncodeDMABUF(dmabuf.Frame, bool) ([]byte, bool, error)
}

// CanSubmit lets capture leave its latest native sample queued while hardware
// is busy, rather than consuming and discarding that sample prematurely.
func CanSubmit(e Encoder) (bool, error) {
	if gate, ok := e.(interface{ CanSubmit() (bool, error) }); ok {
		return gate.CanSubmit()
	}
	return true, nil
}

func New(o Options) (Encoder, error) {
	if o.Width < 2 || o.Height < 2 || o.Width > 8192 || o.Height > 8192 || o.Width%2 != 0 || o.Height%2 != 0 {
		return nil, fmt.Errorf("invalid encoder size %dx%d", o.Width, o.Height)
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	o.Rate = o.Rate.Or(o.FPS)
	o.FPS = o.Rate.Round()
	if !o.Rate.Valid() || o.Rate.Num < o.Rate.Den || int64(o.Rate.Num) > 240*int64(o.Rate.Den) {
		return nil, fmt.Errorf("invalid encoder frame rate")
	}
	if o.Bitrate < 0 {
		return nil, fmt.Errorf("invalid encoder bitrate")
	}
	if o.Bitrate == 0 && !o.OmitBitrate {
		o.Bitrate = 12000
	}
	if o.Bitrate > 1000000 {
		return nil, fmt.Errorf("invalid encoder bitrate")
	}
	if o.Codec == "" {
		o.Codec = "h264"
	}
	if o.Codec != "h264" && o.Codec != "hevc" && o.Codec != "av1" {
		return nil, fmt.Errorf("unsupported codec %q", o.Codec)
	}
	return newPlatformEncoder(o)
}

// Release frees output after its final reader; a copied Packet borrows the reference.
func (p *Packet) Release() {
	if p != nil {
		p.Buffer.Release()
		p.Buffer = nil
		p.Data = nil
	}
}
