//go:build windows && cgo

package capture

/*
#cgo windows LDFLAGS: -ld3d11 -ldxgi -ldxguid -lmfplat -lmf -lmfreadwrite -lmfuuid -lole32 -luuid -luser32
#include "native_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/videoenc"
	"anykvm/internal/videotiming"
	"fmt"
	"image"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

type windowsDevice struct {
	mu                            sync.Mutex
	id                            string
	ptr                           *C.bk_win_capture
	enc                           ffmpegDevice // Shared in-process JPEG/native GPU encoding, without libavmedia capture.
	modeConfigured                bool
	mode                          Mode
	hideCursor                    bool
	capturePacer                  framePacer
	fps                           int
	sourceRate                    framerate.Rate
	captureRate                   framerate.Rate
	readbacks, textureFrames      uint64
	lastNativePath                string
	jpegLeases                    atomic.Int32
	adapter                       videoenc.WindowsAdapter
	gpuJPEG                       windowsGPUJPEG
	desktopDamage                 jpegcodec.DesktopDamageAccumulator
	collectDesktopDamage          bool
	timingStatsAt                 time.Time
	timingCPU, accumulatedUpdates uint64
}

func newWindowsDevice(id string) Device { return &windowsDevice{id: id} }

func (d *windowsDevice) SetCaptureMode(value string) error {
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	if isScreenDevice(d.id) && mode.PixelFormat != "" {
		return fmt.Errorf("screen capture does not support an input pixel format")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("cannot change capture mode while open")
	}
	d.mode = mode
	d.modeConfigured = true
	return nil
}

func (d *windowsDevice) UsesLocalCursor() bool { return isScreenDevice(d.id) }

// DXGI hardware cursors are not composited into the desktop in this backend.
func (d *windowsDevice) SetHideCursor(v bool) { d.hideCursor = v }
func (d *windowsDevice) SetDecodeImage(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enc.SetDecodeImage(v)
	d.updateDamageCollection()
}
func (d *windowsDevice) SetJPEGQuality(q int)                        { d.enc.quality = q }
func (d *windowsDevice) SetVideoBitrate(kbps int)                    { d.enc.SetVideoBitrate(kbps) }
func (d *windowsDevice) SetVideoCodec(c string)                      { d.enc.SetVideoCodec(c) }
func (d *windowsDevice) SetVideoChroma(c string)                     { d.enc.SetVideoChroma(c) }
func (d *windowsDevice) SetVideoStreamOptions(o *VideoStreamOptions) { d.enc.SetVideoStreamOptions(o) }
func (d *windowsDevice) SetOutputDownscaleOnly(v bool)               { d.enc.outputDownscaleOnly = v }
func (d *windowsDevice) SetOutputSize(w, h int)                      { d.enc.targetW, d.enc.targetH = w, h }
func (d *windowsDevice) SetOutputSizeLimit(w, h int)                 { d.enc.SetOutputSizeLimit(w, h) }
func (d *windowsDevice) SetOutputScale(scale float64) error {
	if scale != 0 && scale != 1 && !isScreenDevice(d.id) {
		return fmt.Errorf("proportional scaling requires a local screen source")
	}
	d.enc.outputScale = scale
	return nil
}
func (d *windowsDevice) SetGPUSurface(v bool)         { d.enc.gpuSurface = v }
func (d *windowsDevice) PreserveCaptureMode() bool    { return !isScreenDevice(d.id) }
func (d *windowsDevice) SupportsGameStreamHEVC() bool { return videoenc.SupportsHEVC() }
func (d *windowsDevice) SupportsVideoCodec(c string) bool {
	switch c {
	case "", "mjpeg", "rawvideo", "h264":
		return true
	case "hevc":
		return videoenc.SupportsHEVC()
	case "av1":
		return videoenc.SupportsAV1()
	}
	return false
}
func (d *windowsDevice) SupportsGameStreamAV1() bool      { return videoenc.SupportsAV1() }
func (d *windowsDevice) CaptureFrameRate() framerate.Rate { return d.sourceRate.Or(d.fps) }
func (d *windowsDevice) CanRecoverCapture() bool          { return true }
func (d *windowsDevice) CaptureIdleStatus() (bool, string) {
	if isScreenDevice(d.id) {
		return true, "DXGI desktop updates are event driven"
	}
	return false, "waiting for Media Foundation sample"
}
func (d *windowsDevice) CaptureFPS() int { return d.CaptureFrameRate().Round() }
func (d *windowsDevice) RequestKeyframe() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.enc.RequestKeyframe()
}
func (d *windowsDevice) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *windowsDevice) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("Windows capture already open")
	}
	id := C.CString(d.id)
	defer C.free(unsafe.Pointer(id))
	var msg [512]C.char
	hide := 0
	if d.hideCursor {
		hide = 1
	}
	requestedFPS := float64(max(0, fps))
	if d.modeConfigured && !isScreenDevice(d.id) {
		requestedFPS = d.mode.FPS
	}
	if d.mode.FPS > 0 {
		requestedFPS = d.mode.FPS
	}
	if d.mode.Width > 0 {
		w, h = d.mode.Width, d.mode.Height
	}
	inputFormat := d.mode.PixelFormat
	codecPreference := d.enc.videoCodec
	if d.enc.streamOptions != nil {
		codecPreference = d.enc.streamOptions.Codec
		if codecPreference == "" {
			codecPreference = "h264"
		}
	}
	if inputFormat == "" && !isScreenDevice(d.id) {
		if modes, err := ListVideoModes(d.id); err == nil {
			inputFormat = preferredCaptureFormat(modes, w, h, requestedFPS, codecPreference, d.enc.shouldDecode())
		}
	}
	format := C.CString(inputFormat)
	defer C.free(unsafe.Pointer(format))
	if !isScreenDevice(d.id) {
		if _, err := ListVideoModes(d.id); err != nil {
			return err
		}
		windowsModes.use(d.id, 1)
		defer func() {
			if d.ptr == nil {
				windowsModes.use(d.id, -1)
			}
		}()
	}
	jpeg := C.int(1)
	codec := d.enc.videoCodec
	if d.enc.streamOptions != nil {
		codec = d.enc.streamOptions.Codec
	}
	if d.enc.shouldDecode() || codec == "rawvideo" || codec == "uyvy" {
		jpeg = 0
	} else if codec == "" || codec == "mjpeg" {
		jpeg = 2
	}
	d.ptr = C.bk_win_open_video(id, C.int(w), C.int(h), C.double(requestedFPS), format, C.int(hide), jpeg, &msg[0], C.size_t(len(msg)))
	if d.ptr == nil {
		return fmt.Errorf("Windows native capture %q mode %dx%d@%g:%s: %s", d.id, w, h, requestedFPS, d.mode.PixelFormat, C.GoString(&msg[0]))
	}
	d.adapter = videoenc.WindowsAdapter{}
	if isScreenDevice(d.id) {
		d.updateDamageCollection()
		var luid C.uint64_t
		var vendor C.uint32_t
		if C.bk_win_adapter(d.ptr, &luid, &vendor) == 0 {
			C.bk_win_close(d.ptr)
			d.ptr = nil
			return fmt.Errorf("Windows desktop adapter identity unavailable")
		}
		d.adapter = videoenc.WindowsAdapter{LUID: uint64(luid), VendorID: uint32(vendor), Set: true}
		logging.Infof("capture: Windows desktop %q adapter=%s", d.id, d.adapter)
	}
	d.timingStatsAt = time.Now()
	if videotiming.Enabled {
		d.timingCPU = uint64(C.bk_win_process_cpu_100ns())
	}
	deviceRate := framerate.New(int(C.bk_win_rate_num(d.ptr)), int(C.bk_win_rate_den(d.ptr)))
	rate, err := nativeCaptureRate(deviceRate, fps)
	if !isScreenDevice(d.id) && deviceRate.Valid() {
		rate, err = deviceRate, nil
	}
	if err != nil {
		C.bk_win_close(d.ptr)
		d.ptr = nil
		return err
	}
	d.sourceRate = deviceRate
	d.captureRate = rate
	d.fps = rate.Round()
	if isScreenDevice(d.id) {
		d.sourceRate = rate
	}
	d.enc.reqFPS = rate.Round()
	d.enc.requestedRate = rate
	d.enc.inputFPS = d.sourceRate
	d.enc.scalableInput = isScreenDevice(d.id)
	if isScreenDevice(d.id) && !d.enc.outputDownscaleOnly {
		d.enc.targetW, d.enc.targetH = w, h
	}
	d.enc.closed.Store(false)
	d.capturePacer = framePacer{}
	d.desktopDamage.Consumed()
	backend := "Media Foundation"
	if isScreenDevice(d.id) {
		backend = "DXGI Desktop Duplication"
	}
	logging.Infof("capture: Windows %s %q device_rate=%d/%d capture_rate=%d/%d input_format=%s", backend, d.id, deviceRate.Num, deviceRate.Den, rate.Num, rate.Den, C.GoString(C.bk_win_input_format(d.ptr)))
	return nil
}
func (d *windowsDevice) Capture() (*Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, ErrStreamStopped
	}
	// Desktop duplication is driven by updates; avoid queuing old pictures.
	if isScreenDevice(d.id) && !d.enc.forceIDR.Load() && !d.capturePacer.due(time.Now(), d.captureRate.Interval()) {
		return nil, nil
	}
	var msg [512]C.char
	native := C.bk_win_next(d.ptr, 10, &msg[0], C.size_t(len(msg)))
	if native == nil && msg[0] == 0 && d.enc.forceIDR.Load() && isScreenDevice(d.id) {
		native = C.bk_win_repeat(d.ptr)
	}
	if native == nil {
		if msg[0] != 0 {
			return nil, fmt.Errorf("%w: Windows native capture: %s", ErrStreamStopped, C.GoString(&msg[0]))
		}
		return nil, nil
	}
	outputCodec := d.enc.videoCodec
	if d.enc.streamOptions != nil {
		outputCodec = d.enc.streamOptions.Codec
		if outputCodec == "" {
			outputCodec = "h264"
		}
	}
	encodedOutput := outputCodec == "h264" || outputCodec == "hevc" || outputCodec == "av1"
	lease := &windowsFrame{ptr: native, adapter: d.adapter, gpuJPEG: &d.gpuJPEG, surfaceLeases: &d.enc.gpuSurfaceLeases}
	if isScreenDevice(d.id) && (d.enc.jpegRegions || d.collectDesktopDamage) && !encodedOutput {
		lease.damage = d.desktopDamage.Add(lease.desktopDamage(), int(C.bk_win_frame_rotation(native)))
	} else {
		d.desktopDamage.Consumed()
	}
	if C.bk_win_frame_kind(native) == 2 && !encodedOutput && !d.enc.shouldDecode() && d.lastNativePath != "mjpeg" {
		d.enc.outputLog.start("jpeg", "passthrough")
		d.lastNativePath = "mjpeg"
	}
	defer func() { lease.Release() }()
	f := &Frame{Format: FormatRaw, Width: int(C.bk_win_frame_width(native)), Height: int(C.bk_win_frame_height(native)), CapturedAt: time.Now(), nativeBuffer: lease}
	if isScreenDevice(d.id) {
		var timing C.bk_win_timing
		C.bk_win_frame_timing(native, &timing)
		if timing.accumulated_frames > 1 {
			d.accumulatedUpdates += uint64(timing.accumulated_frames - 1)
		}
		anchor := time.Now()
		mapTime := func(t C.int64_t) time.Time {
			return videotiming.QPCTime(int64(t), int64(timing.frequency), int64(timing.now), anchor)
		}
		if at := videotiming.PictureTime(mapTime(timing.present), mapTime(timing.acquired), timing.repeated != 0); !at.IsZero() {
			f.CapturedAt = at
		}
		if videotiming.Sample() {
			f.Timing = &videotiming.Trace{Present: mapTime(timing.present), AcquireStart: mapTime(timing.acquire_start), Acquired: mapTime(timing.acquired), CopySubmitted: mapTime(timing.copy_submitted), Anchor: anchor, AnchorQPC: int64(timing.now), Frequency: int64(timing.frequency), Repeated: timing.repeated != 0}
		}
	}
	if C.bk_win_frame_kind(native) == 2 && !d.enc.shouldDecode() && !encodedOutput {
		var data *C.uint8_t
		var size C.size_t
		var pitch C.int
		if C.bk_win_frame_borrow(native, &data, &size, &pitch, &msg[0], 512) > 0 {
			f.Data = unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size))
			f.Format = FormatJPEG
			f.nativeBuffer = nil
			// Bound retained device samples. Slow VNC clients must not exhaust
			// the capture driver's pool; an extra compressed-packet copy is
			// cheaper than holding every capture buffer behind network I/O.
			if d.jpegLeases.Load() == 0 {
				d.jpegLeases.Add(1)
				retained := lease
				f.Buffer = framebuf.New(f.Data, func() { retained.Release(); d.jpegLeases.Add(-1) })
				lease = nil
			} else {
				f.Data = append([]byte(nil), f.Data...)
				f.Buffer = framebuf.New(f.Data, nil)
			}
			return f, nil
		} else {
			if msg[0] != 0 {
				return nil, fmt.Errorf("borrow MJPEG: %s", C.GoString(&msg[0]))
			}
			if C.bk_win_frame_data(native, &data, &size, &msg[0], 512) < 0 {
				return nil, fmt.Errorf("read MJPEG: %s", C.GoString(&msg[0]))
			}
			f.Data = unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size))
			f.Format = FormatJPEG
			f.nativeBuffer = nil
			f.Buffer = framebuf.New(f.Data, func() { C.free(unsafe.Pointer(data)) })
			return f, nil
		}
	}
	if isScreenDevice(d.id) {
		d.capturePacer.accepted(time.Now(), d.captureRate.Interval())
	}
	out, err := d.enc.encodeMediaFrame(f)
	if err == nil && out != nil {
		if (out.Format == FormatRaw || out.Format == FormatSurface || out.JPEGRegions != nil) && lease.damage != nil {
			out.Damage = jpegcodec.MapDesktopDamage(lease.damage, out.Width, out.Height, int(C.bk_win_frame_rotation(native)))
		}
		d.desktopDamage.Consumed()
	}
	if out != nil && f.Timing != nil {
		t := f.Timing
		t.Ready = time.Now()
		name := "pixels"
		if out.Format == FormatJPEG {
			name = "jpeg"
		}
		if out.Format == FormatSurface {
			name = "surface"
		}
		if d.enc.encoder != nil {
			name = d.enc.encoder.Name()
		}
		t.Path = "cpu-readback/" + name
		if lease.textureUsed {
			t.Path = "d3d11/" + name
		}
		if timed, ok := d.enc.encoder.(interface{ LastNativeTiming() videotiming.Native }); ok {
			t.Native(timed.LastNativeTiming())
		}
		out.Timing = t
		t.Capture()
	}
	if lease.path != "" && lease.path != d.lastNativePath {
		logging.Infof("capture: Windows path=%s", lease.path)
		d.lastNativePath = lease.path
	}
	if lease.materialized {
		d.readbacks++
	}
	if lease.textureUsed {
		d.textureFrames++
		if out != nil && isScreenDevice(d.id) {
			out.ContentRect = screenContentRect(out.Width, out.Height, f.Width, f.Height, true)
		}
	}
	if videotiming.Enabled && d.adapter.Set && time.Since(d.timingStatsAt) >= 5*time.Second {
		now := time.Now()
		cpu := uint64(C.bk_win_process_cpu_100ns())
		cpuText := "unavailable"
		if d.timingCPU > 0 && cpu >= d.timingCPU {
			cpuText = fmt.Sprintf("%.1f", float64(cpu-d.timingCPU)*float64(100*time.Nanosecond)/float64(now.Sub(d.timingStatsAt))*100)
		}
		model := "compatibility_inflight_unknown"
		if _, ok := d.enc.encoder.(interface{ LastNativeTiming() videotiming.Native }); ok {
			model = "native_one_inflight"
		}
		logging.Debugf("video-timing: Windows desktop=%q adapter=%s cumulative_texture_frames=%d cumulative_cpu_readbacks=%d cumulative_unencoded_drops=%d cumulative_accumulated_updates=%d encoder_model=%s process_cpu_core_percent=%s", d.id, d.adapter, d.textureFrames, d.readbacks, d.enc.skippedEncodeInputs, d.accumulatedUpdates, model, cpuText)
		d.timingStatsAt, d.timingCPU = now, cpu
	}
	if out != nil && out.nativeBuffer != nil {
		out.nativeBuffer = nil
	}
	if err != nil {
		return out, fmt.Errorf("%w: Windows native encoding: %w", ErrStreamStopped, err)
	}
	return out, nil
}
func (d *windowsDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gpuJPEG.close()
	d.desktopDamage.Consumed()
	if d.ptr != nil {
		C.bk_win_close(d.ptr)
		if !isScreenDevice(d.id) {
			windowsModes.use(d.id, -1)
		}
		d.ptr = nil
	}
	return d.enc.Close()
}

type windowsFrame struct {
	damage                    *jpegcodec.DesktopDamage
	surfaceLeases             *atomic.Int32
	gpuJPEG                   *windowsGPUJPEG
	adapter                   videoenc.WindowsAdapter
	ptr                       *C.bk_win_frame
	materialized, textureUsed bool
	preserveJPEG              bool
	path                      string
}

func (f *windowsFrame) SnapshotSurface() (*gpuconv.Surface, *framebuf.Buffer, bool, error) {
	if f.surfaceLeases == nil || !f.adapter.Set || uintptr(C.bk_win_frame_handle(f.ptr)) == 0 {
		return nil, nil, false, nil
	}
	if f.surfaceLeases.Load() >= 4 {
		return nil, nil, true, nil
	}
	var message [512]C.char
	snapshot := C.bk_win_frame_snapshot(f.ptr, &message[0], 512)
	if snapshot == nil {
		return nil, nil, true, fmt.Errorf("D3D11 immutable capture: %s", C.GoString(&message[0]))
	}
	f.surfaceLeases.Add(1)
	leases := f.surfaceLeases
	owner := framebuf.New(nil, func() { C.bk_win_frame_free(snapshot); leases.Add(-1) })
	f.textureUsed, f.path = true, "immutable GPU surface (no pixel readback)"
	return &gpuconv.Surface{SharedTexture: uintptr(C.bk_win_frame_handle(snapshot)), AdapterLUID: f.adapter.LUID,
		Width: int(C.bk_win_frame_width(snapshot)), Height: int(C.bk_win_frame_height(snapshot)), Rotation: int(C.bk_win_frame_rotation(snapshot))}, owner, true, nil
}

// GPU state belongs to the capture session, not a borrowed frame. Capture and
// Close are serialized by windowsDevice.mu. Failures fall back once per session.
type windowsGPUJPEG struct {
	encoder  *jpegcodec.D3D11Encoder
	disabled bool
}

func (j *windowsGPUJPEG) close() {
	if j.encoder != nil {
		j.encoder.Close()
	}
	*j = windowsGPUJPEG{}
}

func (*windowsFrame) JPEGEncoderName() string { return "D3D11 Compute JPEG" }

func (f *windowsFrame) EncodeJPEG(width, height, quality int) (*framebuf.Buffer, bool, error) {
	buffer, _, handled, err := f.encodeJPEG(width, height, quality, false)
	return buffer, handled, err
}
func (f *windowsFrame) EncodeJPEGRegions(width, height, quality int) (*jpegcodec.RestartJPEG, bool, error) {
	_, snapshot, handled, err := f.encodeJPEG(width, height, quality, true)
	return snapshot, handled, err
}

func (f *windowsFrame) desktopDamage() *jpegcodec.DesktopDamage {
	if f.damage != nil {
		return f.damage
	}
	var damage C.bk_win_damage
	C.bk_win_frame_damage(f.ptr, &damage)
	d := &jpegcodec.DesktopDamage{Stream: uint64(damage.stream), Previous: uint64(damage.previous), Sequence: uint64(damage.sequence), SourceWidth: int(damage.width), SourceHeight: int(damage.height), Valid: damage.valid != 0}
	if damage.count > 1<<20 || (damage.count > 0 && damage.rects == nil) || damage.move_count > 1<<20 || (damage.move_count > 0 && damage.moves == nil) {
		d.Valid = false
		return d
	}
	if d.Valid {
		for _, m := range unsafe.Slice(damage.moves, int(damage.move_count)) {
			d.Moves = append(d.Moves, jpegcodec.DesktopMove{Source: image.Rect(int(m.source.left), int(m.source.top), int(m.source.right), int(m.source.bottom)), Destination: image.Rect(int(m.destination.left), int(m.destination.top), int(m.destination.right), int(m.destination.bottom))})
		}
		for _, r := range unsafe.Slice(damage.rects, int(damage.count)) {
			d.Rects = append(d.Rects, image.Rect(int(r.left), int(r.top), int(r.right), int(r.bottom)))
		}
	}
	return d
}

func (f *windowsFrame) encodeJPEG(width, height, quality int, regions bool) (*framebuf.Buffer, *jpegcodec.RestartJPEG, bool, error) {
	j := f.gpuJPEG
	if j == nil || j.disabled || !f.adapter.Set || os.Getenv("ANYKVM_WINDOWS_GPU_JPEG") == "0" {
		return nil, nil, false, nil
	}
	handle := uintptr(C.bk_win_frame_handle(f.ptr))
	if handle == 0 {
		return nil, nil, false, nil
	}
	var err error
	if j.encoder == nil {
		j.encoder, err = jpegcodec.NewD3D11Encoder(f.adapter.LUID)
	}
	var data []byte
	var snapshot *jpegcodec.RestartJPEG
	if err == nil {
		if regions {
			snapshot, err = j.encoder.EncodeRegionsWithDamage(handle, int(C.bk_win_frame_rotation(f.ptr)), width, height, quality, f.desktopDamage())
		} else {
			data, err = j.encoder.EncodeTexture(handle, int(C.bk_win_frame_rotation(f.ptr)), width, height, quality)
		}
	}
	if err != nil {
		j.close()
		j.disabled = true
		logging.Infof("capture: D3D11 JPEG unavailable; using CPU JPEG: %v", err)
		return nil, nil, false, nil
	}
	f.textureUsed, f.path = true, "D3D11 Compute JPEG (compressed readback)"
	if regions {
		return nil, snapshot, true, nil
	}
	return framebuf.New(data, nil), nil, true, nil
}

func (f *windowsFrame) WindowsAdapter() videoenc.WindowsAdapter { return f.adapter }

func (f *windowsFrame) Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error) {
	return false, fmt.Errorf("Windows frame is a D3D11 texture")
}
func (f *windowsFrame) Release() {
	if f == nil {
		return
	}
	if f.ptr != nil {
		C.bk_win_frame_free(f.ptr)
		f.ptr = nil
	}
}

// MaterializeVideo retains device MJPEG for the compute decoder. Other
// consumers keep Materialize's existing decoded-pixel contract.
func (f *windowsFrame) MaterializeVideo(out *Frame) error {
	f.preserveJPEG = true
	defer func() { f.preserveJPEG = false }()
	return f.Materialize(out)
}

func (f *windowsFrame) Materialize(out *Frame) error {
	var data *C.uint8_t
	var size C.size_t
	var msg [512]C.char
	if C.bk_win_frame_data(f.ptr, &data, &size, &msg[0], C.size_t(len(msg))) < 0 {
		return fmt.Errorf("Windows frame readback: %s", C.GoString(&msg[0]))
	}
	defer C.free(unsafe.Pointer(data))
	f.materialized = true
	out.Data = C.GoBytes(unsafe.Pointer(data), C.int(size))
	out.nativeBuffer = nil
	switch C.bk_win_frame_kind(f.ptr) {
	case 1:
		out.Format, out.PixFmt = FormatYUV, "nv12"
	case 2:
		if f.preserveJPEG {
			out.Format = FormatJPEG
			return nil
		}
		pixels, w, h, err := jpegcodec.DecodeBGRA(out.Data)
		if err != nil {
			return err
		}
		out.Data, out.Width, out.Height, out.Format = pixels, w, h, FormatRaw
	}
	return nil
}
func (f *windowsFrame) EncodeTexture(e videoenc.Encoder, force bool) ([]byte, bool, error) {
	if C.bk_win_frame_kind(f.ptr) == 1 {
		if int(C.bk_win_frame_pitch(f.ptr)) == int(C.bk_win_frame_width(f.ptr)) {
			if mf, ok := e.(videoenc.MediaFoundationSampleEncoder); ok {
				packet, handled, err := mf.EncodeMediaFoundationSample(C.bk_win_frame_sample(f.ptr), force)
				if handled || err != nil {
					f.textureUsed = handled && err == nil
					f.path = "borrowed NV12 sample"
					return packet, handled, err
				}
			}
		}
		// Only tightly packed NV12 is compatible with the encoder's negotiated type.
		var data *C.uint8_t
		var size C.size_t
		var pitch C.int
		var msg [512]C.char
		if C.bk_win_frame_borrow(f.ptr, &data, &size, &pitch, &msg[0], 512) <= 0 {
			return nil, false, nil
		}
		w, h := int(C.bk_win_frame_width(f.ptr)), int(C.bk_win_frame_height(f.ptr))
		if int(pitch) != w {
			return nil, false, nil
		}
		// Native vendor encoders consume the borrowed plane directly. Their GPU
		// upload remains necessary for USB samples backed by system memory.
		packet, err := e.Encode(unsafe.Slice((*byte)(unsafe.Pointer(data)), w*h*3/2), force)
		f.textureUsed = err == nil
		f.path = "borrowed NV12 planes (encoder upload may remain)"
		return packet, true, err
	}
	handle := uintptr(C.bk_win_frame_handle(f.ptr))
	if handle == 0 {
		return nil, false, nil
	}
	if gpu, ok := e.(videoenc.D3D11TextureEncoder); ok {
		data, handled, err := gpu.EncodeD3D11Texture(handle, int(C.bk_win_frame_rotation(f.ptr)), force)
		f.textureUsed = handled && err == nil
		return data, handled, err
	}
	return nil, false, nil
}

func windowsDevices(audio bool) []string {
	var msg [512]C.char
	kind := 0
	if audio {
		kind = 1
	}
	names := C.bk_win_devices(C.int(kind), &msg[0], C.size_t(len(msg)))
	if names == nil {
		logging.Infof("capture: Media Foundation enumeration: %s", C.GoString(&msg[0]))
		return []string{}
	}
	defer C.free(unsafe.Pointer(names))
	value := strings.TrimSuffix(C.GoString(names), "\n")
	if value == "" {
		return []string{}
	}
	return strings.Split(value, "\n")
}
func ListNativeVideoDevices(bool) []string { return windowsDevices(false) }
func ListNativeAudioDevices() []string     { return windowsDevices(true) }

type windowsAudioDevice struct {
	mu  sync.Mutex
	id  string
	ptr *C.bk_win_capture
}

func newWindowsAudioDevice(id string) AudioDevice { return &windowsAudioDevice{id: id} }
func (d *windowsAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("Windows audio already open")
	}
	id := C.CString(d.id)
	defer C.free(unsafe.Pointer(id))
	var msg [512]C.char
	d.ptr = C.bk_win_open(id, 0, 0, 0, 0, 1, &msg[0], C.size_t(len(msg)))
	if d.ptr == nil {
		return fmt.Errorf("Windows native audio: %s", C.GoString(&msg[0]))
	}
	return nil
}
func (d *windowsAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, ErrStreamStopped
	}
	var msg [512]C.char
	f := C.bk_win_next(d.ptr, 0, &msg[0], C.size_t(len(msg)))
	if f == nil {
		if msg[0] != 0 {
			return nil, fmt.Errorf("Windows native audio: %s", C.GoString(&msg[0]))
		}
		return nil, nil
	}
	defer C.bk_win_frame_free(f)
	var data *C.uint8_t
	var size C.size_t
	if C.bk_win_frame_data(f, &data, &size, &msg[0], C.size_t(len(msg))) < 0 {
		return nil, fmt.Errorf("Windows native audio: %s", C.GoString(&msg[0]))
	}
	defer C.free(unsafe.Pointer(data))
	return &AudioFrame{PCM: C.GoBytes(unsafe.Pointer(data), C.int(size)), SampleRate: AudioSampleRate, Channels: AudioChannels}, nil
}
func (d *windowsAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		C.bk_win_close(d.ptr)
		d.ptr = nil
	}
	return nil
}

func (d *windowsDevice) updateDamageCollection() {
	if d.ptr == nil {
		return
	}
	codec := d.enc.videoCodec
	if d.enc.streamOptions != nil {
		codec = d.enc.streamOptions.Codec
	}
	enabled := C.int(0)
	if (d.enc.jpegRegions || d.collectDesktopDamage) && (codec == "" || codec == "mjpeg" || codec == "rawvideo") {
		enabled = 1
	}
	C.bk_win_collect_damage(d.ptr, enabled)
}
func (d *windowsDevice) SetJPEGRegions(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enc.jpegRegions = v
	d.updateDamageCollection()
}

func (d *windowsDevice) SetDesktopDamage(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.collectDesktopDamage = v
	d.updateDamageCollection()
}
