package airplay

import (
	"fmt"
	"sync"

	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/videodec"
)

// Device adapts the native receiver to AnyKVM capture. H.264 stays compressed
// for capable consumers; pixel/JPEG consumers use the in-process media decoder.
type Device struct {
	receiver           *Receiver
	lifeMu             sync.Mutex
	mu                 sync.Mutex
	codec, activeCodec string
	decode             bool
	quality            int
	decoder            videoDecoder
	open               bool
	receiverPersistent bool
}

func NewDevice(device string, options Options) (*Device, error) {
	r, err := newReceiver(device, options, false)
	if err != nil {
		return nil, err
	}
	// Pixel consumers receive mirror frames only. URL playback is explicitly
	// enabled by the Web listener and never requires a server demuxer.
	r.urlMedia = false
	return &Device{receiver: r}, nil
}

// EnableBrowserMedia must be called before starting discovery.
func (d *Device) EnableBrowserMedia() { d.receiver.browserMedia = true }

func (d *Device) Open() error { return d.OpenWithFormat(0, 0, 30) }

func (d *Device) OpenWithFormat(width, height, fps int) error {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	d.mu.Lock()
	codec, alreadyOpen := d.codec, d.open
	d.mu.Unlock()
	if alreadyOpen {
		return nil
	}
	width, height, fps, err := mirrorDisplay(width, height, fps)
	if err != nil {
		return err
	}
	if !d.SupportsVideoCodec(codec) {
		return fmt.Errorf("airplay: unsupported output codec %q", codec)
	}
	var decoder videoDecoder
	if codec != "h264" {
		var err error
		decoder, err = newMirrorVideoDecoder()
		if err != nil {
			return fmt.Errorf("airplay: H.264 decoder for JPEG/raw output: %w", err)
		}
	}
	d.receiver.mu.Lock()
	// These are sender hints, not a server-side scaler or encoder. The sender
	// chooses the actual picture dimensions and bitrate for its screen shape.
	d.receiver.width, d.receiver.height, d.receiver.fps = width, height, fps
	d.receiver.mu.Unlock()
	if err := d.receiver.Start(); err != nil {
		if decoder != nil {
			decoder.Close()
		}
		return err
	}
	d.mu.Lock()
	d.decoder, d.activeCodec, d.open = decoder, codec, true
	d.mu.Unlock()
	logging.Infof("airplay: native receiver %q ready; select it in iPhone/iPad Screen Mirroring (video and audio, legacy pairing)", d.receiver.name)
	logging.Infof("airplay: requested mirror display %dx%d up to %dfps; actual dimensions and bitrate are sender-controlled", width, height, fps)
	return nil
}

func mirrorDisplay(width, height, fps int) (int, int, int, error) {
	if width == 0 && height == 0 {
		width, height = 1920, 1080
	}
	if width < 1 || height < 1 || width > 8192 || height > 8192 {
		return 0, 0, 0, fmt.Errorf("airplay: display size must be 1..8192 in both dimensions, or 0x0 for the default")
	}
	if fps == 0 {
		fps = 30
	}
	if fps < 1 || fps > 255 {
		return 0, 0, 0, fmt.Errorf("airplay: FPS must be 1..255")
	}
	return width, height, fps, nil
}

// StartReceiver makes discovery and pairing available before a viewer starts.
// Encoded input retains its initial IDR in the bounded receiver queue; TCP
// backpressure preserves dependent frames until capture attaches.
func (d *Device) StartReceiver(width, height, fps int) error {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	width, height, fps, err := mirrorDisplay(width, height, fps)
	if err != nil {
		return err
	}
	d.receiver.mu.Lock()
	d.receiver.width, d.receiver.height, d.receiver.fps = width, height, fps
	d.receiver.mu.Unlock()
	if err := d.receiver.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.receiverPersistent = true
	d.mu.Unlock()
	logging.Infof("airplay: receiver %q advertised before viewer connection; Screen Mirroring is ready", d.receiver.name)
	return nil
}

// ShutdownReceiver is the service lifetime boundary, independent of capture.
func (d *Device) ShutdownReceiver() error {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	d.mu.Lock()
	d.receiverPersistent = false
	d.mu.Unlock()
	return d.receiver.Close()
}

func (d *Device) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	open, codec, decoder := d.open, d.activeCodec, d.decoder
	d.mu.Unlock()
	if !open {
		return nil, capture.ErrStreamStopped
	}
	f, err := d.receiver.ReadFrame()
	if err != nil || f == nil || codec == "h264" {
		return f, err
	}
	if decoder == nil {
		return nil, capture.ErrStreamStopped
	}
	pixels, err := decoder.Decode(f.Data)
	f.Buffer.Release()
	f.Buffer = nil
	if err != nil || pixels == nil {
		return nil, err
	}
	f.Data, f.Width, f.Height, f.Format = pixels.Data, pixels.Width, pixels.Height, capture.FormatRaw
	d.mu.Lock()
	decode, quality := d.decode, d.quality
	d.mu.Unlock()
	if codec == "rawvideo" || decode {
		return f, nil
	}
	if quality == 0 {
		quality = 80
	}
	f.Data, err = jpegcodec.EncodeBGRA(f.Data, f.Width, f.Height, quality)
	f.Format = capture.FormatJPEG
	return f, err
}

func (d *Device) Close() error {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	d.mu.Lock()
	decoder := d.decoder
	d.decoder = nil
	d.open = false
	persistent := d.receiverPersistent
	d.mu.Unlock()
	var err error
	if persistent {
		d.receiver.disconnectSenders()
	} else {
		err = d.receiver.Close()
	}
	if decoder != nil {
		decoder.Close()
	}
	return err
}

func (d *Device) SetDecodeImage(v bool)      { d.mu.Lock(); d.decode = v; d.mu.Unlock() }
func (d *Device) SetJPEGQuality(v int)       { d.mu.Lock(); d.quality = max(0, min(100, v)); d.mu.Unlock() }
func (d *Device) SetVideoCodec(codec string) { d.mu.Lock(); d.codec = codec; d.mu.Unlock() }

// Do not implement VideoStreamConfigurer: Moonlight's macOS H.264 decoder
// rewrites SPS reference limits to one, corrupting AirPlay's multi-reference
// dependency chain. Bridge therefore requests raw pixels for GameStream's local
// encoder. Web can still request h264 and receive the unmodified native stream.
func (*Device) PreserveCaptureMode() bool { return true }

// Native AirPlay H.264 can produce bursty multi-reference decoder output.
// Locally re-encoded output must not inherit this presentation policy.
func (*Device) BufferH264Presentation() bool { return true }
func (*Device) CanRestartForKeyframe() bool  { return false }
func (*Device) SupportsVideoCodec(codec string) bool {
	return codec == "" || codec == "mjpeg" || codec == "h264" || codec == "rawvideo"
}
func (*Device) SupportsGameStreamHEVC() bool { return false }
func (*Device) SupportsGameStreamAV1() bool  { return false }
func (*Device) CaptureFPS() int              { return 0 } // Sender-controlled variable cadence, not the advertised cap.
func (d *Device) CaptureIdleStatus() (bool, string) {
	return true, "AirPlay waits for sender media, screen updates or a Screen Mirroring connection"
}

var _ capture.Device = (*Device)(nil)
var _ capture.KeyframeRestartPolicy = (*Device)(nil)

type videoDecoder interface {
	Decode([]byte) (*avmedia.Frame, error)
	Close()
}

func newMirrorVideoDecoder() (videoDecoder, error) { return videodec.NewH264() }
