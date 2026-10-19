package capture

import (
	"anykvm/internal/framerate"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"sync"
)

// pipeEncoderDevice encodes frames from a capture source in-process.
type pipeEncoderDevice struct {
	src          Device
	codec        string
	fps, quality int
	decode       bool
	mu           sync.Mutex
	enc          ffmpegDevice
}

func newPipeEncoderDevice(src Device, codec string, fps int) *pipeEncoderDevice {
	return &pipeEncoderDevice{src: src, codec: codec, fps: max(1, fps)}
}
func (d *pipeEncoderDevice) SetDecodeImage(v bool) { d.mu.Lock(); defer d.mu.Unlock(); d.decode = v }
func (d *pipeEncoderDevice) SetJPEGQuality(q int)  { d.mu.Lock(); defer d.mu.Unlock(); d.quality = q }
func (d *pipeEncoderDevice) CaptureFPS() int       { return d.CaptureFrameRate().Round() }
func (d *pipeEncoderDevice) CaptureFrameRate() framerate.Rate {
	if source, ok := d.src.(interface{ CaptureFrameRate() framerate.Rate }); ok {
		if rate := source.CaptureFrameRate(); rate.Valid() {
			return rate
		}
	}
	return framerate.New(d.fps, 1)
}
func (d *pipeEncoderDevice) Open() error { return d.OpenWithFormat(0, 0, d.fps) }
func (d *pipeEncoderDevice) OpenWithFormat(w, h, fps int) error {
	if fps > 0 {
		d.fps = fps
	}
	d.enc.videoCodec = d.codec
	d.enc.reqFPS = d.fps
	if err := d.src.OpenWithFormat(w, h, fps); err != nil {
		return err
	}
	d.enc.inputFPS = d.CaptureFrameRate()
	d.enc.closed.Store(false)
	return nil
}
func (d *pipeEncoderDevice) Capture() (*Frame, error) {
	d.mu.Lock()
	if d.enc.closed.Load() {
		d.mu.Unlock()
		return nil, ErrStreamStopped
	}
	ready, err := d.enc.receiveEncoded()
	if ready != nil || err != nil {
		d.mu.Unlock()
		return ready, err
	}
	admit, err := videoenc.CanSubmit(d.enc.encoder)
	d.mu.Unlock()
	if err != nil || !admit {
		return nil, err
	}
	var f *Frame
	if src, ok := d.src.(interface{ captureNative() (*Frame, error) }); ok && (d.codec == "h264" || d.codec == "hevc") {
		f, err = src.captureNative()
	} else {
		f, err = d.src.Capture()
	}
	if f != nil && f.nativeBuffer != nil {
		defer f.nativeBuffer.Release()
	}
	if err != nil || f == nil {
		if err != nil {
			f.Release()
			return nil, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.enc.closed.Load() {
			return nil, ErrStreamStopped
		}
		return d.enc.receiveEncoded()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.enc.closed.Load() {
		f.Release()
		return nil, ErrStreamStopped
	}
	d.enc.quality = d.quality
	d.enc.decodeImage = d.decode
	out, err := d.enc.encodeMediaFrame(f)
	if out != f {
		f.Release()
	}
	return out, err
}
func (d *pipeEncoderDevice) RequestKeyframe() error { return d.enc.RequestKeyframe() }
func (d *pipeEncoderDevice) Close() error {
	d.enc.closed.Store(true)
	err := d.src.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enc.Close()
	return err
}

func validPipeFrame(f *Frame) bool {
	if f == nil || f.Width <= 0 || f.Height <= 0 {
		return false
	}
	switch f.Format {
	case FormatRaw:
		return len(f.Data) == f.Width*f.Height*4
	case FormatYUV:
		size := yuvFrameSize(f.PixFmt, f.Width, f.Height)
		return size > 0 && len(f.Data) == size
	}
	return false
}

// yuvFrameSize returns the byte count of one frame in the given ffmpeg pixel
// format (0 when unknown).
func yuvFrameSize(pixfmt string, w, h int) int {
	switch pixfmt {
	case "uyvy422", "yuyv422":
		// Packed 4:2:2 stores pairs of pixels, including padding for odd widths.
		return ((w + 1) / 2) * 4 * h
	case "nv12":
		// The interleaved chroma plane rounds both dimensions up to pairs.
		return w*h + 2*((w+1)/2)*((h+1)/2)
	}
	return 0
}

// pipePixFmt returns the ffmpeg -pixel_format name for a pipe frame.
func pipePixFmt(f *Frame) string {
	if f.Format == FormatYUV {
		return f.PixFmt
	}
	return "bgra"
}

func codecName(codec string) string {
	if codec == "" {
		return "mjpeg"
	}
	return codec
}

// jpegEncodeDevice compresses raw frames with the platform JPEG encoder when
// the consumer wants compressed frames (SetDecodeImage(false)). Device JPEGs
// pass through, so native MJPEG sources incur no extra encoding.
type jpegEncodeDevice struct {
	Device
	quality int

	mu     sync.Mutex
	decode bool
}

func (j *jpegEncodeDevice) CaptureFrameRate() framerate.Rate {
	if source, ok := j.Device.(interface{ CaptureFrameRate() framerate.Rate }); ok {
		return source.CaptureFrameRate()
	}
	if source, ok := j.Device.(fpsReporter); ok {
		return framerate.New(source.CaptureFPS(), 1)
	}
	return framerate.Rate{}
}
func (j *jpegEncodeDevice) CaptureFPS() int { return j.CaptureFrameRate().Round() }

func (j *jpegEncodeDevice) SetDecodeImage(v bool) {
	j.mu.Lock()
	j.decode = v
	j.mu.Unlock()
	j.Device.SetDecodeImage(v)
}

func (j *jpegEncodeDevice) SetJPEGQuality(q int) {
	j.mu.Lock()
	j.quality = q
	j.mu.Unlock()
	j.Device.SetJPEGQuality(q)
}

func (j *jpegEncodeDevice) Capture() (*Frame, error) {
	f, err := j.Device.Capture()
	if err != nil || f == nil {
		return f, err
	}
	j.mu.Lock()
	decode, quality := j.decode, j.quality
	j.mu.Unlock()

	if decode || f.Format != FormatRaw || len(f.Data) != f.Width*f.Height*4 {
		return f, nil
	}
	if quality <= 0 {
		quality = 80
	}
	defer f.Release()
	data, err := jpegcodec.EncodeBGRAOwned(f.Data, f.Width, f.Height, quality)
	if err != nil {
		return nil, err
	}
	out := *f
	out.Data, out.Format, out.Buffer = data.Bytes(), FormatJPEG, data
	return &out, nil
}

// bgraToJPEGBytes encodes BGRA pixels as JPEG.
func bgraToJPEGBytes(pixels []byte, w, h, quality int) ([]byte, error) {
	return jpegcodec.EncodeBGRA(pixels, w, h, quality)
}
