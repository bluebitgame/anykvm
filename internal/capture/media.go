package capture

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/videoenc"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Platform input builders remain a compact description of device options; only
// input arguments are consumed. They are never passed to a subprocess.
func mediaInputArgs(args []string) (format, url string, options map[string]string, err error) {
	options = make(map[string]string)
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-f":
			format = args[i+1]
			i++
		case "-i":
			url = args[i+1]
			if format == "" {
				err = fmt.Errorf("missing capture format")
			}
			return
		case "-framerate", "-video_size", "-pixel_format", "-capture_cursor", "-draw_mouse", "-audio_buffer_size":
			options[args[i][1:]] = args[i+1]
			i++
		}
	}
	err = fmt.Errorf("missing capture input")
	return
}
func fitCaptureSize(w, h, tw, th int) (int, int) {
	if tw <= 0 || th <= 0 {
		return w, h
	}
	if w*th > h*tw {
		return max(2, tw&^1), max(2, (h*tw/w)&^1)
	}
	return max(2, (w*th/h)&^1), max(2, th&^1)
}

// BoundedOutputSize permits a smaller canvas, but uses the actual source size
// when either requested edge exceeds the device. It never enlarges the image.
func BoundedOutputSize(w, h, tw, th int) (int, int) {
	if tw <= 0 || th <= 0 || tw > w || th > h {
		return w, h
	}
	return tw, th
}

// LimitedOutputSize intersects the source and configured ceiling before
// applying the request. Neither axis may exceed either limit.
func LimitedOutputSize(w, h, tw, th, maxW, maxH int) (int, int) {
	if maxW > 0 {
		w = min(w, maxW)
	}
	if maxH > 0 {
		h = min(h, maxH)
	}
	return BoundedOutputSize(w, h, tw, th)
}

func (d *ffmpegDevice) fitOutputSize(w, h int) (int, int) {
	tw, th := d.targetW, d.targetH
	if d.outputDownscaleOnly {
		tw, th = LimitedOutputSize(w, h, tw, th, d.outputMaxW, d.outputMaxH)
	}
	tw, th = d.scaledOutputRequest(tw, th)
	if d.outputScale > 0 && d.outputScale != 1 && !d.outputDownscaleOnly {
		return tw, th
	}
	return fitCaptureSize(w, h, tw, th)
}

func (d *ffmpegDevice) scaledOutputRequest(w, h int) (int, int) {
	if d.outputScale == 0 || d.outputScale == 1 {
		return w, h
	}
	if d.outputDownscaleOnly {
		return BoundedOutputSize(d.scaledW, d.scaledH, w, h)
	}
	return d.scaledW, d.scaledH
}

func (d *ffmpegDevice) SetOutputScale(scale float64) error {
	if scale != 0 && scale != 1 && !isScreenDevice(d.device) {
		return fmt.Errorf("proportional scaling requires a local screen source")
	}
	d.outputScale = scale
	return nil
}

func (d *ffmpegDevice) SetOutputSize(w, h int)        { d.targetW, d.targetH = w, h }
func (d *ffmpegDevice) SetOutputDownscaleOnly(v bool) { d.outputDownscaleOnly = v }
func (d *ffmpegDevice) SetOutputSizeLimit(w, h int)   { d.outputMaxW, d.outputMaxH = w, h }
func (d *ffmpegDevice) RequestKeyframe() error {
	if d.passthrough.Load() {
		logging.Infof("capture: passthrough waiting for source IDR (device keyframe control unavailable)")
		return nil
	}
	d.forceIDR.Store(true)
	return nil
}
func mediaCaptureFrame(f *avmedia.Frame) *Frame {
	format := FormatRaw
	if f.Codec == "h264" {
		format = FormatH264
	} else if f.Codec == "hevc" {
		format = FormatHEVC
	} else if f.Codec == "av1" {
		format = FormatAV1
	}
	if f.Buffer == nil {
		f.Buffer = framebuf.New(f.Data, nil)
	}
	owned := f.Buffer
	f.Buffer = nil // transfer ownership from the libav frame
	return &Frame{CapturedAt: time.Now(), Buffer: owned, Data: f.Data, Width: f.Width, Height: f.Height, SourceWidth: f.Width, SourceHeight: f.Height, Format: format}
}
func (d *ffmpegDevice) readMediaFrame() (*Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed.Load() {
		return nil, ErrStreamStopped
	}
	if ready, err := d.receiveEncoded(); ready != nil || err != nil {
		return ready, err
	}
	if admit, err := videoenc.CanSubmit(d.encoder); err != nil || !admit {
		return nil, err
	}
	f := d.pendingFrame
	d.pendingFrame = nil
	if f == nil {
		raw, err := d.media.ReadOwned()
		if err == io.EOF {
			return nil, ErrStreamStopped
		}
		if err != nil || raw == nil {
			return nil, err
		}
		f = mediaCaptureFrame(raw)
	}
	out, err := d.encodeMediaFrame(f)
	if out != f {
		f.Release()
	}
	return out, err
}
func (d *ffmpegDevice) encodeMediaFrame(f *Frame) (*Frame, error) {
	if d.outputScale > 0 && d.outputScale != 1 {
		var err error
		d.scaledW, d.scaledH, err = ScaledResolution(f.Width, f.Height, d.outputScale)
		if err != nil {
			return nil, err
		}
	}
	// Never rate-limit, resize or re-encode an already compressed source here.
	// Dropping reference pictures would corrupt subsequent predicted pictures.
	if f.Format == FormatH264 || f.Format == FormatHEVC || f.Format == FormatAV1 {
		d.outputLog.start(f.Format.String(), "passthrough")
		return f, nil
	}
	codec := d.videoCodec
	if d.streamOptions != nil {
		codec = d.streamOptions.Codec
		if codec == "" {
			codec = "h264"
		}
	}
	if codec == "h264" || codec == "hevc" || codec == "av1" {
		if d.outputDownscaleOnly && (d.outputMaxW > 0 && d.outputMaxW < 2 || d.outputMaxH > 0 && d.outputMaxH < 2) {
			return nil, fmt.Errorf("Target resolution limit is too small for video encoding")
		}
		w, h := d.fitOutputSize(f.Width, f.Height)
		w &= ^1
		h &= ^1
		o := videoenc.Options{Width: w, Height: h, FPS: max(1, d.reqFPS), Rate: d.requestedRate, Bitrate: defaultH264BitrateKbps, Codec: codec}
		if d.videoBitrate > 0 {
			o.Bitrate = d.videoBitrate
		}
		if d.streamOptions != nil {
			o.Rate = framerate.Rate{}
			o.Width = d.streamOptions.Width
			o.Height = d.streamOptions.Height
			o.FPS = d.streamOptions.FPS
			o.Bitrate = d.streamOptions.Bitrate
			o.DisableLowLatencyRateControl = d.streamOptions.DisableLowLatencyRateControl
			o.H264Profile = d.streamOptions.H264Profile
		}
		if source, ok := f.nativeBuffer.(interface{ DRMDevice() (uint32, uint32) }); ok {
			o.DRMDeviceMajor, o.DRMDeviceMinor = source.DRMDevice()
		}
		if source, ok := f.nativeBuffer.(interface {
			WindowsAdapter() videoenc.WindowsAdapter
		}); ok {
			o.WindowsAdapter = source.WindowsAdapter()
		}
		preserve := d.PreserveCaptureMode()
		divisor := 1
		if preserve {
			o.Width, o.Height = f.Width, f.Height
		}
		// Keep the device's capture mode and frame cadence, while allowing RDP
		// to encode fewer pixels. Already-compressed input returned above.
		if d.outputDownscaleOnly {
			// Native texture conversion fits/letterboxes into this canvas, as
			// SCK does. Keep the requested canvas so RDP's capture-resize gate
			// also works when source and client have different aspect ratios.
			o.Width, o.Height = LimitedOutputSize(f.Width, f.Height, d.targetW, d.targetH,
				d.outputMaxW&^1, d.outputMaxH&^1)
			o.Width, o.Height = (o.Width+1)&^1, (o.Height+1)&^1
		}
		o.Width, o.Height = d.scaledOutputRequest(o.Width, o.Height)
		// USB mirroring can deliver 30 fps while DAL reports a nominal 60 fps.
		// Keep arrival-time pacing for it; counting every second sample would
		// halve the actual rate. Fixed-rate capture cards retain decimation.
		decimate := preserve && !d.variableInputRate && d.streamOptions != nil
		if decimate {
			o.Rate, divisor = d.CaptureFrameRate().Decimate(d.streamOptions.FPS)
			o.FPS = o.Rate.Round()
		}
		if o.FPS <= 0 {
			o.FPS = 30
		}
		o.Rate = o.Rate.Or(o.FPS)
		if d.encoder == nil || d.encodeOptions != o {
			if d.encoder != nil {
				d.encoder.Close()
				d.encoder = nil
			}
			e, err := videoenc.New(o)
			if err != nil {
				return nil, err
			}
			d.encoder = e
			d.encodeOptions = o
			d.sourceFrame = 0
			d.encodedFrames = nil
			d.encodePacer = framePacer{}
			d.forceIDR.Store(true)
			d.outputLog.start(codec, e.Name())
		}
		if decimate {
			skip := d.sourceFrame%uint64(divisor) != 0
			d.sourceFrame++
			if skip {
				d.skippedEncodeInputs++
				return d.receiveEncoded()
			}
		}
		capturedAt := f.CapturedAt
		if capturedAt.IsZero() {
			capturedAt = time.Now()
		}
		if !decimate && !d.forceIDR.Load() && !d.encodePacer.due(capturedAt, o.Rate.Interval()) {
			d.skippedEncodeInputs++
			return d.receiveEncoded()
		}
		if encoder, ok := d.encoder.(videoenc.AsyncEncoder); ok {
			return d.submitEncoded(encoder, o, f, capturedAt)
		}
		if texture, ok := f.nativeBuffer.(interface {
			EncodeTexture(videoenc.Encoder, bool) ([]byte, bool, error)
		}); ok {
			force := d.forceIDR.Swap(false)
			data, handled, err := texture.EncodeTexture(d.encoder, force)
			if err != nil {
				return nil, err
			}
			if handled {
				d.encodePacer.accepted(capturedAt, o.Rate.Interval())
				format := FormatH264
				if codec == "hevc" {
					format = FormatHEVC
				} else if codec == "av1" {
					format = FormatAV1
				}
				buffer := framebuf.New(data, nil)
				return &Frame{Buffer: buffer, Data: buffer.Bytes(), Width: o.Width, Height: o.Height, SourceWidth: f.Width, SourceHeight: f.Height, Format: format, CapturedAt: f.CapturedAt}, nil
			}
			if force {
				d.forceIDR.Store(true)
			}
		}
		if f.nativeBuffer != nil {
			var err error
			if native, ok := f.nativeBuffer.(interface{ MaterializeVideo(*Frame) error }); ok {
				err = native.MaterializeVideo(f)
			} else {
				err = f.nativeBuffer.Materialize(f)
			}
			if err != nil {
				return nil, err
			}
		}
		pixels, pw, ph, pf := f.Data, f.Width, f.Height, pipePixFmt(f)
		if f.Format == FormatJPEG {
			decoded, err := d.jpegDecoder.Decode(f.Data, f.Width, f.Height, o.Width, o.Height)
			if err != nil {
				return nil, err
			}
			pixels, pw, ph, pf = decoded.Data, decoded.Width, decoded.Height, decoded.Format
		}
		buffer, err := videoenc.EncodePixelsOwned(d.encoder, o, pixels, pw, ph, pf, d.forceIDR.Swap(false))
		if err != nil {
			return nil, err
		}
		d.encodePacer.accepted(capturedAt, o.Rate.Interval())
		if buffer == nil {
			return nil, nil
		}
		format := FormatH264
		if codec == "hevc" {
			format = FormatHEVC
		} else if codec == "av1" {
			format = FormatAV1
		}
		return &Frame{Buffer: buffer, Data: buffer.Bytes(), Width: o.Width, Height: o.Height, SourceWidth: f.Width, SourceHeight: f.Height, Format: format, CapturedAt: f.CapturedAt}, nil
	}
	// Native desktop JPEG can consume the texture before Materialize reads the
	// entire frame into CPU memory. Device-compressed MJPEG keeps its passthrough.
	if d.gpuSurface && codec == "rawvideo" && f.nativeBuffer != nil &&
		((d.outputScale == 0 || d.outputScale == 1) || d.scaledW == f.Width && d.scaledH == f.Height) {
		if source, ok := f.nativeBuffer.(interface {
			SnapshotSurface() (*gpuconv.Surface, *framebuf.Buffer, bool, error)
		}); ok {
			surface, owner, handled, err := source.SnapshotSurface()
			if err != nil {
				owner.Release()
				return nil, err
			}
			if handled {
				if surface == nil {
					owner.Release()
					return nil, nil
				} // Bounded snapshot pool is full.
				if owner == nil {
					return nil, fmt.Errorf("GPU snapshot has no lifetime owner")
				}
				if err := surface.Validate(); err != nil {
					owner.Release()
					return nil, err
				}
				f.Buffer.Release()
				f.Surface, f.Buffer, f.Data, f.Format, f.nativeBuffer = surface, owner, nil, FormatSurface, nil
				d.outputLog.start("gpu", "passthrough (GPU surface)")
				return f, nil
			}
			owner.Release()
		}
	}
	if f.nativeBuffer != nil && !d.shouldDecode() && (codec == "" || codec == "mjpeg") {
		if d.jpegRegions {
			if gpu, ok := f.nativeBuffer.(interface {
				EncodeJPEGRegions(int, int, int) (*jpegcodec.RestartJPEG, bool, error)
			}); ok {
				w, h := d.fitOutputSize(f.Width, f.Height)
				if d.PreserveCaptureMode() {
					w, h = f.Width, f.Height
				}
				regions, handled, err := gpu.EncodeJPEGRegions(w, h, d.jpegQuality())
				if err != nil {
					return nil, err
				}
				if handled {
					if regions == nil {
						return nil, fmt.Errorf("GPU JPEG regions missing snapshot")
					}
					d.outputLog.start("jpeg", "GPU JPEG (changed segment readback)")
					return &Frame{JPEGRegions: regions, Width: w, Height: h, SourceWidth: f.Width, SourceHeight: f.Height, Format: FormatJPEG, CapturedAt: f.CapturedAt}, nil
				}
			}
		}

		if gpu, ok := f.nativeBuffer.(interface {
			EncodeJPEG(int, int, int) (*framebuf.Buffer, bool, error)
			JPEGEncoderName() string
		}); ok {
			w, h := d.fitOutputSize(f.Width, f.Height)
			if d.PreserveCaptureMode() {
				w, h = f.Width, f.Height
			}
			buffer, handled, err := gpu.EncodeJPEG(w, h, d.jpegQuality())
			if err != nil {
				return nil, err
			}
			if handled {
				d.outputLog.start("jpeg", gpu.JPEGEncoderName())
				return &Frame{Buffer: buffer, Data: buffer.Bytes(), Width: w, Height: h,
					SourceWidth: f.Width, SourceHeight: f.Height, Format: FormatJPEG, CapturedAt: f.CapturedAt}, nil
			}
		}
	}
	sourceW, sourceH := f.SourceWidth, f.SourceHeight
	if sourceW <= 0 || sourceH <= 0 {
		sourceW, sourceH = f.Width, f.Height
	}
	if f.nativeBuffer != nil {
		if err := f.nativeBuffer.Materialize(f); err != nil {
			return nil, err
		}
	}
	if f.Format != FormatRaw {
		if codec == "uyvy" {
			return f, nil
		}
		if d.jpegMedia == nil || d.jpegW != f.Width || d.jpegH != f.Height {
			if d.jpegMedia != nil {
				d.jpegMedia.Close()
				d.jpegMedia = nil
			}
			encoder, err := avmedia.NewEncoder(avmedia.VideoOptions{Width: f.Width &^ 1, Height: f.Height &^ 1, FPS: max(1, d.reqFPS), Bitrate: 12000, Codec: "mjpeg"})
			if err != nil {
				return nil, err
			}
			d.jpegMedia = encoder
			d.jpegW = f.Width
			d.jpegH = f.Height
			d.outputLog.start("jpeg", "FFmpeg mjpeg")
		}
		buffer, err := d.jpegMedia.EncodeOwned(f.Data, f.Width, f.Height, pipePixFmt(f), false)
		if err != nil || buffer == nil {
			return nil, err
		}
		return &Frame{Buffer: buffer, Data: buffer.Bytes(), Width: f.Width &^ 1, Height: f.Height &^ 1, Format: FormatJPEG, CapturedAt: f.CapturedAt}, nil
	}
	w, h := d.fitOutputSize(f.Width, f.Height)
	if d.PreserveCaptureMode() {
		w, h = f.Width, f.Height
	}
	if w != f.Width || h != f.Height {
		data := make([]byte, w*h*4)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				src := ((y*f.Height/h)*f.Width + x*f.Width/w) * 4
				copy(data[(y*w+x)*4:], f.Data[src:src+4])
			}
		}
		f = &Frame{Data: data, Width: w, Height: h, SourceWidth: sourceW, SourceHeight: sourceH, Format: FormatRaw, CapturedAt: f.CapturedAt}
	}
	if d.outputScale > 0 && d.outputScale != 1 {
		f.SourceWidth, f.SourceHeight = sourceW, sourceH
	}
	if codec == "rawvideo" || codec == "uyvy" || d.shouldDecode() {
		d.outputLog.start(f.Format.String(), "none (pixels)")
		return f, nil
	}
	jpeg, err := jpegcodec.EncodeBGRAOwned(f.Data, f.Width, f.Height, d.jpegQuality())
	if err != nil {
		return nil, err
	}
	d.outputLog.start("jpeg", jpegcodec.Backend())
	return &Frame{Buffer: jpeg, Data: jpeg.Bytes(), Width: f.Width, Height: f.Height, SourceWidth: f.SourceWidth, SourceHeight: f.SourceHeight, Format: FormatJPEG, CapturedAt: f.CapturedAt}, nil
}

func (d *ffmpegDevice) jpegQuality() int {
	quality := d.quality
	if quality <= 0 {
		quality = 80
		if q, err := strconv.Atoi(d.qscale); err == nil {
			quality = max(1, 100-(q-2)*99/29)
		}
	}
	return min(100, max(1, quality))
}

// Outstanding metadata is bounded by the native encoder's admission limit.
// Only accepted input gets an ID, and completion always uses its source time.
func (d *ffmpegDevice) submitEncoded(encoder videoenc.AsyncEncoder, o videoenc.Options, f *Frame, capturedAt time.Time) (*Frame, error) {
	d.nextEncodedID++
	id := d.nextEncodedID
	force := d.forceIDR.Swap(false)
	var accepted bool
	var err error
	if native, ok := d.encoder.(videoenc.PixelBufferSubmitter); ok && f.nativeBuffer != nil {
		accepted, err = f.nativeBuffer.Submit(native, id, force)
	} else {
		if f.nativeBuffer != nil {
			if err = f.nativeBuffer.Materialize(f); err != nil {
				if force {
					d.forceIDR.Store(true)
				}
				return nil, err
			}
		}
		accepted, err = encoder.Submit(f.Data, f.Width, f.Height, pipePixFmt(f), id, force)
	}
	if err != nil || !accepted {
		if force {
			d.forceIDR.Store(true)
		}
		if err != nil {
			return nil, err
		}
		return d.receiveEncoded()
	}
	d.encodePacer.accepted(capturedAt, o.Rate.Interval())
	if d.encodedFrames == nil {
		d.encodedFrames = make(map[uint64]*Frame)
	}
	out := *f
	out.Data, out.nativeBuffer, out.Buffer = nil, nil, nil
	out.Width, out.Height, out.SourceWidth, out.SourceHeight = o.Width, o.Height, f.Width, f.Height
	out.PixFmt = ""
	out.Format = FormatH264
	if o.Codec == "hevc" {
		out.Format = FormatHEVC
	} else if o.Codec == "av1" {
		out.Format = FormatAV1
	}
	d.encodedFrames[id] = &out
	return d.receiveEncoded()
}

func (d *ffmpegDevice) receiveEncoded() (*Frame, error) {
	encoder, ok := d.encoder.(videoenc.AsyncEncoder)
	if !ok {
		return nil, nil
	}
	for {
		packet, err := encoder.Receive()
		if err != nil || packet == nil {
			return nil, err
		}
		f := d.encodedFrames[packet.ID]
		delete(d.encodedFrames, packet.ID)
		if f == nil {
			packet.Release()
			return nil, fmt.Errorf("encoder returned unknown frame %d", packet.ID)
		}
		if packet.Dropped {
			packet.Release()
			d.forceIDR.Store(true)
			continue
		}
		f.Data, f.Buffer = packet.Data, packet.Buffer
		packet.Data, packet.Buffer = nil, nil
		return f, nil
	}
}
