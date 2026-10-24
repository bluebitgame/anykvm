//go:build linux && cgo

package capture

/*
#include <stdlib.h>
#include <unistd.h>
#include "kms_linux.h"
*/
import "C"

import (
	"anykvm/internal/dmabuf"
	"anykvm/internal/framerate"
	"anykvm/internal/logging"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

type kmsDevice struct {
	mu     sync.Mutex
	id     string
	ptr    *C.bk_kms
	output C.bk_kms_output
	enc    *DMABUFEncoder
	mode   Mode
	rate   framerate.Rate
	pacer  framePacer
}

func newScreenDevice(id string) Device {
	encoder := NewDMABUFEncoder(DMABUFEncoderOptions{})
	encoder.device = id
	return &kmsDevice{id: id, enc: encoder}
}
func (d *kmsDevice) SetDecodeImage(v bool)                       { d.enc.SetDecodeImage(v) }
func (d *kmsDevice) SetJPEGQuality(q int)                        { d.enc.SetJPEGQuality(q) }
func (d *kmsDevice) SetVideoCodec(v string)                      { d.enc.SetVideoCodec(v) }
func (d *kmsDevice) SetVideoBitrate(v int)                       { d.enc.SetVideoBitrate(v) }
func (d *kmsDevice) SetVideoChroma(v string)                     { d.enc.SetVideoChroma(v) }
func (d *kmsDevice) SetVideoStreamOptions(v *VideoStreamOptions) { d.enc.SetVideoStreamOptions(v) }
func (d *kmsDevice) SetOutputDownscaleOnly(v bool)               { d.enc.SetOutputDownscaleOnly(v) }
func (d *kmsDevice) SetOutputSize(w, h int)                      { d.enc.SetOutputSize(w, h) }
func (d *kmsDevice) SetOutputSizeLimit(w, h int)                 { d.enc.SetOutputSizeLimit(w, h) }
func (d *kmsDevice) SetOutputScale(scale float64) error          { return d.enc.SetOutputScale(scale) }
func (d *kmsDevice) SetJPEGRegions(v bool)                       { d.enc.SetJPEGRegions(v) }
func (d *kmsDevice) SetGPUSurface(v bool)                        { d.enc.SetGPUSurface(v) }

func (d *kmsDevice) SupportsVideoCodec(codec string) bool { return d.enc.SupportsVideoCodec(codec) }

func (d *kmsDevice) SupportsGameStreamAV1() bool { return d.enc.SupportsGameStreamAV1() }

func (d *kmsDevice) SupportsGameStreamHEVC() bool { return d.enc.SupportsGameStreamHEVC() }

// KMS capture omits the cursor plane.
func (d *kmsDevice) SetHideCursor(bool) {}
func (d *kmsDevice) SetCaptureMode(value string) error {
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	if mode.PixelFormat != "" && mode.PixelFormat != "bgra" {
		return fmt.Errorf("KMS capture accepts only the native SDR RGB input format")
	}
	d.mode = mode
	return nil
}
func (d *kmsDevice) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *kmsDevice) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("KMS capture already open")
	}
	index := 0
	if d.id != "screen" {
		value, ok := strings.CutPrefix(d.id, "screen:")
		if !ok {
			return fmt.Errorf("invalid KMS screen selector %q", d.id)
		}
		var err error
		index, err = strconv.Atoi(value)
		if err != nil || index < 0 {
			return fmt.Errorf("invalid KMS screen selector %q", d.id)
		}
	}
	var msg [512]C.char
	ptr := C.bk_kms_open(C.int(index), &d.output, &msg[0], 512)
	if ptr == nil {
		return fmt.Errorf("Linux native capture: %s", C.GoString(&msg[0]))
	}
	physical := framerate.New(int(d.output.rate_num), int(d.output.rate_den))
	if d.mode.FPS > 0 {
		physical = framerate.New(int(math.Round(d.mode.FPS*1000000)), 1000000)
		fps = 0
	}
	rate, err := nativeCaptureRate(physical, fps)
	if err != nil {
		C.bk_kms_close(ptr)
		return err
	}
	if d.mode.Width > 0 {
		w, h = d.mode.Width, d.mode.Height
	}
	d.ptr = ptr
	d.rate = rate
	d.pacer = framePacer{}
	d.enc.reqFPS = rate.Round()
	d.enc.requestedRate = rate
	d.enc.inputFPS = rate
	d.enc.scalableInput = true
	if !d.enc.outputDownscaleOnly {
		d.enc.SetOutputSize(w, h)
	}
	d.enc.closed.Store(false)
	logging.Infof("capture: Linux KMS %q %s connector=%d primary-plane=%d %dx%d@%d/%d (DMA-BUF; cursor omitted)", d.id, C.GoString(&d.output.card[0]), d.output.connector, d.output.plane, d.output.width, d.output.height, rate.Num, rate.Den)
	return nil
}
func (d *kmsDevice) Capture() (*Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, ErrStreamStopped
	}
	now := time.Now()
	if !d.enc.forceIDR.Load() && !d.pacer.due(now, d.rate.Interval()) {
		return nil, nil
	}
	var native C.bk_kms_frame
	var msg [512]C.char
	if C.bk_kms_next(d.ptr, &native, &msg[0], 512) < 0 {
		return nil, fmt.Errorf("%w: %s", ErrStreamStopped, C.GoString(&msg[0]))
	}
	source := dmabuf.Frame{
		FD: int(native.fd), Width: uint32(native.width), Height: uint32(native.height),
		FourCC: uint32(native.fourcc), Modifier: uint64(native.modifier),
		Pitch: uint32(native.pitch), Offset: uint32(native.offset), Size: uint64(native.size),
		X: uint32(native.x), Y: uint32(native.y),
		CropWidth: uint32(native.crop_width), CropHeight: uint32(native.crop_height),
		Rotation:    int(native.rotation),
		DeviceMajor: uint32(d.output.device_major), DeviceMinor: uint32(d.output.device_minor),
	}
	source.PlaneCount = uint32(native.planes)
	for i := 0; i < 4; i++ {
		source.PlanePitches[i] = uint32(native.pitches[i])
		source.PlaneOffsets[i] = uint32(native.offsets[i])
	}
	defer C.close(C.int(source.FD))
	out, err := d.enc.EncodeSource(source, int(d.output.width), int(d.output.height), now, int64(native.pts_us))
	if err == nil && (out != nil || d.enc.gpuSurface) {
		d.pacer.accepted(now, d.rate.Interval())
	}
	if err != nil {
		return nil, fmt.Errorf("%w: KMS encoding: %w", ErrStreamStopped, err)
	}
	return out, nil
}
func (d *kmsDevice) CaptureFPS() int                  { return d.rate.Round() }
func (d *kmsDevice) CaptureFrameRate() framerate.Rate { return d.rate }
func (d *kmsDevice) RequestKeyframe() error           { return d.enc.RequestKeyframe() }
func (d *kmsDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.enc.Close()
	if d.ptr != nil {
		C.bk_kms_close(d.ptr)
		d.ptr = nil
	}
	return err
}

func ListScreens() []Screen {
	var outputs [64]C.bk_kms_output
	var msg [512]C.char
	count := int(C.bk_kms_list(&outputs[0], 64, &msg[0], 512))
	if count < 0 {
		return nil
	}
	if count > len(outputs) {
		count = len(outputs)
	}
	result := make([]Screen, count)
	for i := range result {
		result[i] = Screen{Index: i, Width: int(outputs[i].width), Height: int(outputs[i].height)}
	}
	return result
}
