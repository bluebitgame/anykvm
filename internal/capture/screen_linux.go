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
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type kmsDevice struct {
	mu                   sync.Mutex
	id                   string
	ptr                  *C.bk_kms
	output               C.bk_kms_output
	enc                  *ffmpegDevice // common in-process frame routing; no FFmpeg dependency
	converter            *gpuconv.Converter
	jpegEncoder          *jpegcodec.VulkanEncoder
	jpegDisabled         bool
	mode                 Mode
	rate                 framerate.Rate
	pacer                framePacer
	gpuFrames, readbacks uint64
}

func newScreenDevice(id string) Device {
	return &kmsDevice{id: id, enc: &ffmpegDevice{device: id, decodeImage: true}}
}
func (d *kmsDevice) SetDecodeImage(v bool)                       { d.enc.SetDecodeImage(v) }
func (d *kmsDevice) SetJPEGQuality(q int)                        { d.enc.SetJPEGQuality(q) }
func (d *kmsDevice) SetVideoCodec(v string)                      { d.enc.SetVideoCodec(v) }
func (d *kmsDevice) SetVideoBitrate(v int)                       { d.enc.SetVideoBitrate(v) }
func (d *kmsDevice) SetVideoChroma(v string)                     { d.enc.SetVideoChroma(v) }
func (d *kmsDevice) SetVideoStreamOptions(v *VideoStreamOptions) { d.enc.SetVideoStreamOptions(v) }
func (d *kmsDevice) SetOutputSize(w, h int)                      { d.enc.targetW, d.enc.targetH = w, h }

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
	d.enc.targetW, d.enc.targetH = w, h
	d.enc.closed.Store(false)
	log.Printf("capture: Linux KMS %q %s connector=%d primary-plane=%d %dx%d@%d/%d (DMA-BUF; cursor omitted)", d.id, C.GoString(&d.output.card[0]), d.output.connector, d.output.plane, d.output.width, d.output.height, rate.Num, rate.Den)
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
	f := dmabuf.Frame{FD: int(native.fd), Width: uint32(native.width), Height: uint32(native.height), FourCC: uint32(native.fourcc), Pitch: uint32(native.pitch), Offset: uint32(native.offset),
		Modifier: uint64(native.modifier), Size: uint64(native.size), X: uint32(native.x), Y: uint32(native.y), CropWidth: uint32(native.crop_width), CropHeight: uint32(native.crop_height), Rotation: int(native.rotation),
		DeviceMajor: uint32(d.output.device_major), DeviceMinor: uint32(d.output.device_minor)}
	f.PlaneCount = uint32(native.planes)
	for i := 0; i < 4; i++ {
		f.PlanePitches[i] = uint32(native.pitches[i])
		f.PlaneOffsets[i] = uint32(native.offsets[i])
	}
	lease := &kmsFrame{device: d, source: f}
	defer lease.Release()
	frame := &Frame{Format: FormatRaw, Width: int(d.output.width), Height: int(d.output.height), CapturedAt: now, nativeBuffer: lease}
	out, err := d.enc.encodeMediaFrame(frame)
	if out != nil {
		out.nativeBuffer = nil
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
	if d.jpegEncoder != nil {
		d.jpegEncoder.Close()
		d.jpegEncoder = nil
	}
	d.jpegDisabled = false
	if d.converter != nil {
		d.converter.Close()
		d.converter = nil
	}
	if d.ptr != nil {
		C.bk_kms_close(d.ptr)
		d.ptr = nil
	}
	return err
}

type kmsFrame struct {
	device   *kmsDevice
	source   dmabuf.Frame
	released bool
}

func (f *kmsFrame) DRMDevice() (uint32, uint32) { return f.source.DeviceMajor, f.source.DeviceMinor }
func (f *kmsFrame) Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error) {
	return false, fmt.Errorf("KMS uses DMA-BUF images")
}
func (f *kmsFrame) Release() {
	if !f.released {
		C.close(C.int(f.source.FD))
		f.released = true
	}
}
func (f *kmsFrame) EncodeTexture(e videoenc.Encoder, key bool) ([]byte, bool, error) {
	if encoder, ok := e.(videoenc.DMABUFEncoder); ok {
		data, handled, err := encoder.EncodeDMABUF(f.source, key)
		if handled && err == nil {
			f.device.gpuFrames++
		}
		return data, handled, err
	}
	return nil, false, nil
}

func (f *kmsFrame) EncodeJPEG(width, height, quality int) (*framebuf.Buffer, bool, error) {
	d := f.device
	if d.jpegDisabled || os.Getenv("ANYKVM_LINUX_GPU_JPEG") == "0" {
		return nil, false, nil
	}
	var err error
	if d.jpegEncoder == nil {
		d.jpegEncoder, err = jpegcodec.NewVulkanEncoder(f.source.DeviceMajor, f.source.DeviceMinor)
		if err == nil {
			log.Printf("capture: KMS Vulkan Compute JPEG (GPU DCT/quantization/Huffman)")
		}
	}
	var data []byte
	if err == nil {
		data, err = d.jpegEncoder.EncodeSource(f.source, width, height, quality)
	}
	if err != nil {
		if d.jpegEncoder != nil {
			d.jpegEncoder.Close()
			d.jpegEncoder = nil
		}
		d.jpegDisabled = true
		log.Printf("capture: Vulkan JPEG unavailable; using CPU JPEG: %v", err)
		return nil, false, nil
	}
	d.gpuFrames++
	return framebuf.New(data, nil), true, nil
}
func (f *kmsFrame) Materialize(out *Frame) error {
	d := f.device
	if d.converter == nil {
		converter, err := gpuconv.New(f.source.DeviceMajor, f.source.DeviceMinor)
		if err != nil {
			return err
		}
		d.converter = converter
		log.Printf("capture: KMS Vulkan BGRA readback for CPU/JPEG or non-DMA-BUF encoder consumer")
	}
	w, h := fitCaptureSize(out.Width, out.Height, d.enc.targetW, d.enc.targetH)
	converted, err := d.converter.Convert(f.source, w, h, false)
	if err != nil {
		return err
	}
	defer converted.Close()
	data, err := converted.Readback()
	if err != nil {
		return err
	}
	out.Width, out.Height = w, h
	out.Data = data
	out.Format = FormatRaw
	out.nativeBuffer = nil
	d.readbacks++
	return nil
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
