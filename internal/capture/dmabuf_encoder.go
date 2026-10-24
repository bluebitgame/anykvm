package capture

import (
	"fmt"
	"time"

	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
)

// DMABUFEncoder shares desktop format routing and GPU resources between KMS
// and local SPICE. Callers serialize configuration, encoding and Close, and
// retain the producer's draw/fence until EncodeSource returns.
// Its embedded routing implementation never opens an FFmpeg capture session.
type DMABUFEncoder struct {
	ffmpegDevice

	// Source policies persist across capture sessions.
	requireGPUEncoding bool
	idleRefresh        bool

	// Conversion/encoding resources and diagnostics are private to this pipeline.
	converter            *gpuconv.Converter
	jpegEncoder          dmabufJPEGEncoder
	jpegDisabled         bool
	gpuFrames, readbacks uint64

	// lastOutput contains only owned storage or metadata for encoder-owned video.
	lastOutput *Frame
}

// DMABUFStats reports GPU work and pixel readbacks. Reading compressed JPEG
// output does not count as a CPU pixel readback.
type DMABUFStats struct{ GPUFrames, CPUReadbacks uint64 }

// DMABUFEncoderOptions selects source policies. The zero value matches KMS.
type DMABUFEncoderOptions struct {
	// RequireGPUEncoding prevents video/JPEG from silently using CPU pixels.
	// Explicit raw-pixel requests can read back and are counted separately.
	RequireGPUEncoding bool
	// IdleRefresh retains owned output for event-driven sources such as SPICE.
	IdleRefresh bool
}

func NewDMABUFEncoder(o DMABUFEncoderOptions) *DMABUFEncoder {
	return &DMABUFEncoder{
		ffmpegDevice:       ffmpegDevice{decodeImage: true, scalableInput: true},
		requireGPUEncoding: o.RequireGPUEncoding,
		idleRefresh:        o.IdleRefresh,
	}
}

// Optional video formats must match the chosen input path. Library codec
// declarations alone cannot promise DMA-BUF import or cached GPU input.
func (d *DMABUFEncoder) SupportsVideoCodec(codec string) bool {
	return d.supportsVideoCodec(codec, supportsDMABUFVideoCodec, d.ffmpegDevice.SupportsVideoCodec)
}

func (d *DMABUFEncoder) supportsVideoCodec(codec string, gpu, pixels func(string) bool) bool {
	switch codec {
	case "", "mjpeg", "rawvideo", "h264":
		return true
	case "hevc", "av1":
		if gpu(codec) {
			return true
		}
		return !d.requireGPUEncoding && pixels(codec)
	}
	return false
}

func (d *DMABUFEncoder) SupportsGameStreamHEVC() bool { return d.SupportsVideoCodec("hevc") }
func (d *DMABUFEncoder) SupportsGameStreamAV1() bool  { return d.SupportsVideoCodec("av1") }

// Configure starts a capture session; call Close before reopening an existing
// session. Viewer codec/quality/policy settings survive, while size/rate defaults,
// statistics and IDR state restart.
func (d *DMABUFEncoder) Configure(w, h, fps int) {
	d.reqFPS = max(1, fps)
	d.inputFPS = framerate.New(d.reqFPS, 1)
	d.requestedRate = d.inputFPS
	if !d.outputDownscaleOnly {
		d.targetW, d.targetH = w, h
	}
	d.gpuFrames, d.readbacks = 0, 0
	d.closed.Store(false)
	d.forceIDR.Store(true)
}

func (d *DMABUFEncoder) SetGPUSurface(v bool)  { d.gpuSurface = v }
func (d *DMABUFEncoder) SetJPEGRegions(v bool) { d.jpegRegions = v }
func (d *DMABUFEncoder) Stats() DMABUFStats {
	return DMABUFStats{GPUFrames: d.gpuFrames, CPUReadbacks: d.readbacks}
}

// EncodeSource consumes borrowed pixels synchronously. w/h are logical display
// dimensions; source describes the allocation, crop and rotation. No source FD
// or native buffer escapes into output or the idle-refresh cache.
func (d *DMABUFEncoder) EncodeSource(source dmabuf.Frame, w, h int, at time.Time, pts int64) (*Frame, error) {
	if err := source.Validate(); err != nil {
		return nil, err
	}
	native := &dmabufFrame{encoder: d, source: source, width: w, height: h}
	return d.encodeFrame(&Frame{
		Width: w, Height: h, SourceWidth: w, SourceHeight: h,
		Format: FormatRaw, CapturedAt: at, PresentationPTS: pts,
		nativeBuffer: native,
	})
}

func (d *DMABUFEncoder) encodeFrame(frame *Frame) (*Frame, error) {
	out, err := d.encodeMediaFrame(frame)
	if out == nil {
		return nil, err
	}
	out.nativeBuffer = nil // Borrowed producer storage must not escape this call.
	if d.idleRefresh {
		d.rememberOutput(out)
	}
	return out, err
}

func (d *DMABUFEncoder) rememberOutput(frame *Frame) {
	d.lastOutput.Release()
	saved := *frame
	switch frame.Format {
	case FormatH264, FormatHEVC, FormatAV1:
		// Repeated video is encoded from the encoder's own GPU input image.
		saved.Buffer, saved.Data = nil, nil
	default:
		saved.Buffer = framebuf.Acquire(frame.Data, frame.Buffer)
		if frame.Surface == nil {
			saved.Data = saved.Buffer.Bytes()
		}
	}
	d.lastOutput = &saved
}

// Repeat refreshes an idle source after its producer was acknowledged. Video
// uses encoder-owned GPU input; JPEG/pixel/surface output retains owned storage.
func (d *DMABUFEncoder) Repeat() (*Frame, error) {
	if d.lastOutput == nil {
		return nil, nil
	}
	out := *d.lastOutput
	out.CapturedAt = time.Now()
	out.PresentationPTS = 0
	switch out.Format {
	case FormatH264, FormatHEVC, FormatAV1:
		e, ok := d.encoder.(interface{ EncodeLast(bool) ([]byte, error) })
		if !ok {
			return nil, fmt.Errorf("video encoder has no cached GPU input")
		}
		data, err := e.EncodeLast(true)
		if err != nil {
			return nil, err
		}
		out.Buffer = framebuf.New(data, nil)
		out.Data = out.Buffer.Bytes()
	default:
		out.Buffer = out.Buffer.Retain()
	}
	d.forceIDR.Store(false)
	return &out, nil
}

func (d *DMABUFEncoder) Close() error {
	d.lastOutput.Release()
	d.lastOutput = nil
	err := d.ffmpegDevice.Close()
	if d.jpegEncoder != nil {
		d.jpegEncoder.Close()
		d.jpegEncoder = nil
	}
	d.jpegDisabled = false
	if d.converter != nil {
		d.converter.Close()
		d.converter = nil
	}
	return err
}
