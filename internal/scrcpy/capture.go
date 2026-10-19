package scrcpy

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/jpegcodec"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"

	"anykvm/internal/capture"
)

// RequestKeyframe restarts only the Android video encoder, preserving audio.
func (c *Capture) RequestKeyframe() error {
	return c.sess.sendControl([]byte{17})
}

// Capture adapts a scrcpy Session to the capture.Device interface. The device
// streams H.264 or H.265; depending on the codec selected by the bridge it is
// passed through untouched or decoded by in-process libavcodec
// into MJPEG (FormatJPEG) or raw BGRA pixels (FormatRaw).
type Capture struct {
	sess *Session

	mu          sync.Mutex
	codec       string // requested output: "" / "mjpeg", "h264", "hevc", "rawvideo"
	activeCodec string // codec in effect for the current Open
	decodeImage bool
	qscale      string // ffmpeg mjpeg -q:v value ("" = default)
	fps         int

	closed atomic.Bool

	// Compressed passthrough state.
	pendingCfg []byte // last parameter sets, prepended to the next frame

	decoder *avmedia.Session
}

// NewCapture creates a capture.Device backed by the given scrcpy session.
func NewCapture(sess *Session) *Capture {
	return &Capture{sess: sess}
}

func (c *Capture) Open() error { return c.OpenWithFormat(0, 0, 0) }

// OpenWithFormat starts the scrcpy session. The requested width/height act as
// a size limit when mirroring (scrcpy max_size = the larger dimension); 0 keeps
// native resolution. An explicit video.mode creates a separate display at its
// configured dimensions, independent of this output size request.
func (c *Capture) OpenWithFormat(width, height, fps int) error {
	maxSize := width
	if height > maxSize {
		maxSize = height
	}
	c.mu.Lock()
	codec := c.codec
	if codec == "mjpeg" {
		codec = ""
	}
	// JPEG/raw output needs an ffmpeg decode of the scrcpy stream. Without
	// ffmpeg, fall back to H.264 passthrough instead of failing entirely:
	// H.264-capable clients (browser WebCodecs, VNC Open H.264) still work.
	if codec != "h264" && codec != "hevc" {
		if _, ok := capture.ResolveFFmpeg(); !ok {
			log.Printf("scrcpy: FFmpeg shared libraries not found; falling back to H.264 passthrough (JPEG/raw clients will not get video)")
			codec = "h264"
		}
	}
	c.activeCodec = codec
	c.fps = fps
	c.pendingCfg = nil
	qscale := c.qscale
	c.mu.Unlock()
	c.sess.mu.Lock()
	c.sess.videoCodec = "h264"
	if codec == "hevc" {
		c.sess.videoCodec = "h265"
	}
	c.sess.mu.Unlock()
	if err := c.sess.Start(maxSize, fps); err != nil {
		return err
	}
	c.closed.Store(false)

	if codec == "h264" || codec == "hevc" {
		log.Printf("scrcpy: %s passthrough", codec)
		return nil
	}
	if err := c.startTranscoder(codec, qscale); err != nil {
		c.sess.Stop()
		return err
	}
	return nil
}

func (c *Capture) Capture() (*capture.Frame, error) {
	c.mu.Lock()
	codec := c.activeCodec
	c.mu.Unlock()

	switch codec {
	case "h264", "hevc":
		return c.captureEncoded()
	case "rawvideo":
		return c.captureRaw()
	default:
		return c.captureJPEG()
	}
}

func (c *Capture) Close() error {
	c.closed.Store(true)
	c.stopTranscoder()
	c.sess.Stop()
	c.SetGameStreamCaptureOptions(nil)
	return nil
}

// SetDecodeImage controls whether MJPEG frames are decoded to BGRA (FormatRaw).
func (c *Capture) SetDecodeImage(v bool) {
	c.mu.Lock()
	c.decodeImage = v
	c.mu.Unlock()
}

// SetJPEGQuality sets the MJPEG transcode quality (1-100, 0 = default).
// Takes effect on the next Open.
func (c *Capture) SetJPEGQuality(q int) {
	c.mu.Lock()
	c.qscale = jpegQualityToQV(q)
	c.mu.Unlock()
}

// SetVideoCodec selects output ("" / "mjpeg", "h264", "hevc", "rawvideo").
// Takes effect on the next Open (the bridge reopens the device on switch).
func (c *Capture) SetVideoCodec(codec string) {
	c.mu.Lock()
	c.codec = codec
	c.mu.Unlock()
}

// Compressed codecs are encoded on Android and passed through without FFmpeg.
func (c *Capture) SupportsVideoCodec(codec string) bool {
	return codec == "" || codec == "mjpeg" || codec == "h264" || codec == "hevc" || codec == "rawvideo"
}

func (*Capture) SupportsGameStreamHEVC() bool { return true }

func (c *Capture) SetVideoStreamOptions(profile *capture.VideoStreamOptions) {
	if profile != nil {
		codec := profile.Codec
		if codec == "" {
			codec = "h264"
		}
		c.SetVideoCodec(codec)
	}
}

// CaptureFPS reports the frame-rate cap requested at Open (0 = unknown).
func (c *Capture) CaptureFPS() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fps
}

// captureEncoded returns the next H.264/H.265 access unit from the scrcpy
// video socket. Codec config packets are folded into the following
// frame, and their SPS keeps the video dimensions current across rotations.
func (c *Capture) captureEncoded() (*capture.Frame, error) {
	c.mu.Lock()
	hevc := c.activeCodec == "hevc"
	c.mu.Unlock()
	for {
		data, isConfig, err := c.sess.ReadPacket()
		if err != nil {
			if c.closed.Load() {
				return nil, nil
			}
			return nil, fmt.Errorf("scrcpy: video read: %w", err)
		}
		if isConfig {
			parseSize := parseSPSSize
			if hevc {
				parseSize = parseHEVCSPSSize
			}
			if w, h, ok := parseSize(data); ok {
				c.sess.setSize(w, h)
			}
			if len(c.pendingCfg)+len(data) > maxPacketSize {
				return nil, fmt.Errorf("scrcpy: codec parameter sets exceed %d bytes", maxPacketSize)
			}
			c.pendingCfg = append(c.pendingCfg, data...)
			continue
		}
		if len(c.pendingCfg) > 0 {
			data = append(append(make([]byte, 0, len(c.pendingCfg)+len(data)), c.pendingCfg...), data...)
			c.pendingCfg = c.pendingCfg[:0]
		}
		w, h := c.sess.Size()
		format := capture.FormatH264
		if hevc {
			format = capture.FormatHEVC
		}
		return &capture.Frame{Data: data, Format: format, Width: w, Height: h}, nil
	}
}

// Decode complete scrcpy access units inside the process.
func (c *Capture) captureRaw() (*capture.Frame, error) {
	for {
		au, err := c.captureEncoded()
		if err != nil || au == nil {
			return au, err
		}
		c.mu.Lock()
		decoder := c.decoder
		c.mu.Unlock()
		if decoder == nil {
			return nil, fmt.Errorf("decoder closed")
		}
		f, err := decoder.Decode(au.Data)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		return &capture.Frame{Data: f.Data, Width: f.Width, Height: f.Height, Format: capture.FormatRaw}, nil
	}
}
func (c *Capture) captureJPEG() (*capture.Frame, error) {
	f, err := c.captureRaw()
	if err != nil || f == nil {
		return f, err
	}
	c.mu.Lock()
	decode, qscale := c.decodeImage, c.qscale
	c.mu.Unlock()
	if decode {
		return f, nil
	}
	quality := 80
	if q, err := strconv.Atoi(qscale); err == nil {
		quality = max(1, 100-(q-2)*99/29)
	}
	data, err := jpegcodec.EncodeBGRA(f.Data, f.Width, f.Height, quality)
	if err != nil {
		return nil, err
	}
	f.Data = data
	f.Format = capture.FormatJPEG
	return f, nil
}
func (c *Capture) startTranscoder(codec, qscale string) error {
	decoder, err := avmedia.NewDecoder("h264")
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.decoder = decoder
	c.mu.Unlock()
	return nil
}
func (c *Capture) stopTranscoder() {
	c.mu.Lock()
	decoder := c.decoder
	c.decoder = nil
	c.mu.Unlock()
	if decoder != nil {
		decoder.Close()
	}
}

func codecLabel(c string) string {
	if c == "" {
		return "mjpeg"
	}
	return c
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
	qv := 2 + (100-quality)*(31-2)/99
	return strconv.Itoa(qv)
}
