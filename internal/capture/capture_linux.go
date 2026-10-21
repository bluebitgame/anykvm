//go:build linux && cgo

package capture

// #cgo LDFLAGS: -lv4lconvert -lm
// #include <stdlib.h>
// #include "native_v4l2_linux.h"
import "C"

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"errors"
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The native device owns capture and encoding selection, as on Windows.
const nativeIsFFmpeg = true

type v4l2Device struct {
	mu                           sync.Mutex
	path                         string
	ptr                          *C.bk_v4l_capture
	mode                         Mode
	configured                   bool
	width, height                int
	inputFormat                  string
	rate                         framerate.Rate
	enc                          ffmpegDevice // In-process encoding; does not open an FFmpeg capture session.
	decoder                      *avmedia.Session
	dmabufDisabled               bool
	nativeLogged, fallbackLogged bool
}

func newNativeDevice(id string) Device              { return &v4l2Device{path: id} }
func (*v4l2Device) UsesLocalCursor() bool           { return false }
func (*v4l2Device) PreserveCaptureMode() bool       { return true }
func (d *v4l2Device) SetOutputSize(w, h int)        { d.enc.targetW, d.enc.targetH = w, h }
func (d *v4l2Device) SetOutputSizeLimit(w, h int)   { d.enc.SetOutputSizeLimit(w, h) }
func (d *v4l2Device) SetOutputDownscaleOnly(v bool) { d.enc.outputDownscaleOnly = v }
func (*v4l2Device) CanRecoverCapture() bool         { return true }
func (d *v4l2Device) SetCaptureMode(value string) error {
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("cannot change V4L2 capture mode while open")
	}
	d.mode = mode
	d.configured = true
	return nil
}
func (d *v4l2Device) SetDecodeImage(v bool)                       { d.enc.SetDecodeImage(v) }
func (d *v4l2Device) SetJPEGQuality(q int)                        { d.enc.quality = q }
func (d *v4l2Device) SetVideoCodec(c string)                      { d.enc.SetVideoCodec(c) }
func (d *v4l2Device) SetVideoBitrate(kbps int)                    { d.enc.SetVideoBitrate(kbps) }
func (d *v4l2Device) SetVideoChroma(c string)                     { d.enc.SetVideoChroma(c) }
func (d *v4l2Device) SetVideoStreamOptions(o *VideoStreamOptions) { d.enc.SetVideoStreamOptions(o) }
func (d *v4l2Device) CaptureFrameRate() framerate.Rate            { return d.rate }
func (d *v4l2Device) CaptureFPS() int                             { return d.rate.Round() }
func (d *v4l2Device) SupportsGameStreamAV1() bool                 { return videoenc.SupportsAV1() }
func (d *v4l2Device) SupportsGameStreamHEVC() bool                { return videoenc.SupportsHEVC() }
func (d *v4l2Device) SupportsVideoCodec(c string) bool {
	switch c {
	case "", "mjpeg", "rawvideo":
		return true
	case "h264":
		return d.mode.PixelFormat == "h264" || videoenc.SupportsH264()
	case "hevc":
		return videoenc.SupportsHEVC()
	case "av1":
		return videoenc.SupportsAV1()
	}
	return false
}
func (d *v4l2Device) codec() string {
	if d.enc.streamOptions != nil {
		if d.enc.streamOptions.Codec != "" {
			return d.enc.streamOptions.Codec
		}
		return "h264"
	}
	return d.enc.videoCodec
}
func (d *v4l2Device) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *v4l2Device) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("V4L2 capture already open")
	}
	rate := float64(max(0, fps))
	if d.configured {
		rate = d.mode.FPS
	}
	if d.mode.Width > 0 {
		w, h = d.mode.Width, d.mode.Height
	}
	path := C.CString(d.path)
	defer C.free(unsafe.Pointer(path))
	inputFormat := d.mode.PixelFormat
	if inputFormat == "" {
		if modes, err := ListVideoModes(d.path); err == nil {
			inputFormat = preferredCaptureFormat(modes, w, h, rate, d.codec(), d.enc.shouldDecode())
		}
	}
	format := C.CString(inputFormat)
	defer C.free(unsafe.Pointer(format))
	// Some drivers cannot enumerate while the streaming handle is active.
	// A missing optional metadata ioctl must not prevent manually configured input.
	if _, err := ListVideoModes(d.path); err != nil {
		log.Printf("capture: V4L2 mode discovery %q: %v", d.path, err)
	}
	linuxModes.use(d.path, 1)
	var msg [512]C.char
	d.ptr = C.bk_v4l_open(path, C.int(w), C.int(h), C.double(rate), format, &msg[0], C.size_t(len(msg)))
	if d.ptr == nil {
		linuxModes.use(d.path, -1)
		return fmt.Errorf("V4L2 %q mode %dx%d@%g:%s: %s", d.path, w, h, rate, d.mode.PixelFormat, C.GoString(&msg[0]))
	}
	d.width, d.height = int(C.bk_v4l_width(d.ptr)), int(C.bk_v4l_height(d.ptr))
	d.inputFormat = C.GoString(C.bk_v4l_format(d.ptr))
	d.rate = framerate.New(int(C.bk_v4l_rate_num(d.ptr)), int(C.bk_v4l_rate_den(d.ptr)))
	if d.inputFormat == "h264" && (d.codec() != "h264" || d.enc.shouldDecode()) {
		var err error
		d.decoder, err = avmedia.NewDecoder("h264")
		if err != nil {
			C.bk_v4l_close(d.ptr)
			d.ptr = nil
			linuxModes.use(d.path, -1)
			return fmt.Errorf("V4L2 H.264 input needs a decoder for this viewer; select MJPEG/raw or enable FFmpeg libraries: %w", err)
		}
	}
	d.dmabufDisabled, d.nativeLogged, d.fallbackLogged = false, false, false
	d.enc.inputFPS = d.rate
	d.enc.requestedRate = d.rate
	d.enc.reqFPS = d.rate.Round()
	d.enc.scalableInput = false
	d.enc.closed.Store(false)
	log.Printf("capture: V4L2 %q %dx%d input_format=%s rate=%d/%d", d.path, d.width, d.height, d.inputFormat, d.rate.Num, d.rate.Den)
	return nil
}
func (d *v4l2Device) RequestKeyframe() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil && d.inputFormat == "h264" {
		C.bk_v4l_keyframe(d.ptr)
		return nil
	}
	return d.enc.RequestKeyframe()
}
func (d *v4l2Device) Capture() (out *Frame, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, ErrStreamStopped
	}
	if ready, err := d.enc.receiveEncoded(); ready != nil || err != nil {
		return ready, err
	}
	if admit, err := videoenc.CanSubmit(d.enc.encoder); err != nil || !admit {
		return nil, err
	}
	var msg [512]C.char
	var data *C.uint8_t
	var size C.size_t
	var index C.int
	latest := C.int(1)
	if d.inputFormat == "h264" {
		latest = 0
	}
	result := C.bk_v4l_read(d.ptr, latest, &data, &size, &index, &msg[0], C.size_t(len(msg)))
	if result < 0 {
		return nil, fmt.Errorf("%w: %s", ErrStreamStopped, C.GoString(&msg[0]))
	}
	if result == 0 {
		return nil, nil
	}
	when := time.Now()
	if size == 0 {
		C.bk_v4l_done(d.ptr, index, &msg[0], C.size_t(len(msg)))
		return nil, nil
	}
	f := &Frame{Width: d.width, Height: d.height, SourceWidth: d.width, SourceHeight: d.height, CapturedAt: when, Format: FormatRaw}

	// Borrow NV12 until synchronous encoding completes. The defer runs while mu
	// is held, so Close cannot unmap the buffer during encoder access.
	if d.inputFormat == "nv12" && !d.enc.shouldDecode() && (d.codec() == "h264" || d.codec() == "hevc" || d.codec() == "av1") {
		var fd C.int
		var pitch C.uint
		var allocation C.size_t
		if C.bk_v4l_nv12(d.ptr, index, size, &fd, &pitch, &allocation) != 0 {
			lease := &v4l2Frame{device: d, data: unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size)), pitch: int(pitch), fd: int(fd), allocation: uint64(allocation)}
			defer lease.Release()
			defer func() {
				if C.bk_v4l_done(d.ptr, index, &msg[0], C.size_t(len(msg))) < 0 {
					out.Release()
					out = nil
					err = fmt.Errorf("%w: V4L2 requeue: %s", ErrStreamStopped, C.GoString(&msg[0]))
				}
			}()
			f.Format, f.PixFmt, f.nativeBuffer = FormatYUV, "nv12", lease
			return d.enc.encodeMediaFrame(f)
		}
	}
	var convertErr error
	if d.inputFormat == "mjpeg" || d.inputFormat == "jpeg" || d.inputFormat == "h264" {
		f.Data = C.GoBytes(unsafe.Pointer(data), C.int(size))
	} else {
		f.Data = make([]byte, d.width*d.height*4)
		if C.bk_v4l_convert(d.ptr, data, size, (*C.uint8_t)(unsafe.Pointer(&f.Data[0])), C.size_t(len(f.Data)), &msg[0], C.size_t(len(msg))) < 0 {
			convertErr = fmt.Errorf("%s", C.GoString(&msg[0]))
		}
	}
	if C.bk_v4l_done(d.ptr, index, &msg[0], C.size_t(len(msg))) < 0 {
		return nil, fmt.Errorf("%w: %s", ErrStreamStopped, C.GoString(&msg[0]))
	}
	if convertErr != nil {
		return nil, convertErr
	}
	if d.inputFormat == "mjpeg" || d.inputFormat == "jpeg" {
		if !d.enc.shouldDecode() && (d.codec() == "" || d.codec() == "mjpeg") {
			f.Format = FormatJPEG
			f.Buffer = framebuf.New(f.Data, nil)
			d.enc.outputLog.start("jpeg", "passthrough")
			return f, nil
		}
		if codec := d.codec(); codec == "h264" || codec == "hevc" || codec == "av1" {
			f.Format = FormatJPEG
			return d.enc.encodeMediaFrame(f)
		}
		pixels, w, h, err := jpegcodec.DecodeBGRA(f.Data)
		if err != nil {
			return nil, err
		}
		if w != d.width || h != d.height {
			return nil, fmt.Errorf("V4L2 JPEG dimensions changed from %dx%d to %dx%d", d.width, d.height, w, h)
		}
		f.Data = pixels
	} else if d.inputFormat == "h264" {
		if d.decoder == nil {
			f.Format = FormatH264
			f.Buffer = framebuf.New(f.Data, nil)
			d.enc.outputLog.start("h264", "passthrough")
			return f, nil
		}
		decoded, err := d.decoder.Decode(f.Data)
		if err != nil || decoded == nil {
			return nil, err
		}
		f = mediaCaptureFrame(decoded)
		f.CapturedAt = when
	}
	out, err = d.enc.encodeMediaFrame(f)
	if out != f {
		f.Release()
	}
	return out, err
}
func (d *v4l2Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		C.bk_v4l_close(d.ptr)
		d.ptr = nil
		linuxModes.use(d.path, -1)
	}
	if d.decoder != nil {
		d.decoder.Close()
		d.decoder = nil
	}
	return d.enc.Close()
}

// A V4L2 frame never escapes Capture: the encoded packet has independent storage.
type v4l2Frame struct {
	device     *v4l2Device
	data       []byte
	pitch, fd  int
	allocation uint64
}

func (f *v4l2Frame) Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error) {
	return false, fmt.Errorf("V4L2 uses DMA-BUF")
}
func (f *v4l2Frame) Release() {
	if f.fd >= 0 {
		syscall.Close(f.fd)
		f.fd = -1
	}
}
func (f *v4l2Frame) EncodeTexture(e videoenc.Encoder, key bool) ([]byte, bool, error) {
	d := f.device
	if gpu, ok := e.(videoenc.DMABUFEncoder); ok && f.fd >= 0 && !d.dmabufDisabled {
		source := dmabuf.Frame{FD: f.fd, Width: uint32(d.width), Height: uint32(d.height), FourCC: dmabuf.NV12, Pitch: uint32(f.pitch), Size: f.allocation,
			PlaneCount: 2, PlanePitches: [4]uint32{uint32(f.pitch), uint32(f.pitch)}, PlaneOffsets: [4]uint32{0, uint32(f.pitch * d.height)}, CropWidth: uint32(d.width), CropHeight: uint32(d.height)}
		data, handled, err := gpu.EncodeDMABUF(source, key)
		if handled {
			if err == nil && !d.nativeLogged {
				log.Printf("capture: V4L2 NV12 DMA-BUF -> %s (no pixel copy/conversion)", e.Name())
				d.nativeLogged = true
			}
			return data, handled, err
		}
		if err != nil && !errors.Is(err, videoenc.ErrUnavailable) {
			return nil, true, err
		}
		d.dmabufDisabled = true
	}
	if f.pitch != d.width {
		return nil, false, nil
	}
	if !d.fallbackLogged {
		log.Printf("capture: V4L2 borrowed NV12 -> %s (no BGRA conversion; encoder may upload pixels)", e.Name())
		d.fallbackLogged = true
	}
	data, err := e.Encode(f.data[:d.width*d.height*3/2], key)
	return data, true, err
}
func (f *v4l2Frame) Materialize(out *Frame) error {
	d := f.device
	if !d.fallbackLogged {
		log.Printf("capture: V4L2 padded NV12 rows packed for encoder (no BGRA conversion)")
		d.fallbackLogged = true
	}
	out.Data = make([]byte, d.width*d.height*3/2)
	for y := 0; y < d.height+d.height/2; y++ {
		copy(out.Data[y*d.width:(y+1)*d.width], f.data[y*f.pitch:y*f.pitch+d.width])
	}
	out.Format, out.PixFmt, out.nativeBuffer = FormatYUV, "nv12", nil
	return nil
}
