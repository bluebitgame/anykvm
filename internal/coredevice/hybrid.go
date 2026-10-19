package coredevice

import (
	"fmt"

	"anykvm/internal/capture"
	"anykvm/internal/framerate"
)

// HybridInput supplies keyboard/mouse control and the source display geometry.
type HybridInput interface {
	KeyDown(uint8) error
	KeyUp() error
	KeyRelease(uint8) error
	MouseMove(int, int) error
	MouseMoveRel(int, int) error
	MouseButton(int, bool) error
	MouseWheel(int) error
	Open() error
	Close() error
	SetInputDimensions(int, int)
}

// Keep video codec negotiation on AVFoundation and input on Universal HID.
type Hybrid struct {
	capture.Device
	HybridInput
}

// NewHybrid combines an independent video source with CoreDevice input.
func NewHybrid(video capture.Device, input HybridInput) *Hybrid {
	return &Hybrid{Device: video, HybridInput: input}
}

func (d *Hybrid) AudioDevice() capture.AudioDevice {
	if p, ok := d.Device.(capture.AudioProvider); ok {
		return p.AudioDevice()
	}
	return nil
}

func (d *Hybrid) Open() error { return d.OpenWithFormat(0, 0, 30) }
func (d *Hybrid) OpenWithFormat(w, h, fps int) error {
	if err := d.Device.OpenWithFormat(w, h, fps); err != nil {
		d.Device.Close()
		return err
	}
	if err := d.HybridInput.Open(); err != nil {
		d.Device.Close()
		d.HybridInput.Close()
		return err
	}
	return nil
}
func (d *Hybrid) Capture() (*capture.Frame, error) {
	f, err := d.Device.Capture()
	if f != nil {
		w, h := f.SourceWidth, f.SourceHeight
		if w <= 0 || h <= 0 {
			w, h = f.Width, f.Height
		}
		d.HybridInput.SetInputDimensions(w, h)
	}
	return f, err
}
func (d *Hybrid) Close() error {
	inputErr := d.HybridInput.Close()
	videoErr := d.Device.Close()
	if inputErr != nil {
		return inputErr
	}
	return videoErr
}
func (d *Hybrid) SetVideoCodec(codec string) {
	if v, ok := d.Device.(interface{ SetVideoCodec(string) }); ok {
		v.SetVideoCodec(codec)
	}
}
func (d *Hybrid) SetVideoChroma(chroma string) {
	if v, ok := d.Device.(interface{ SetVideoChroma(string) }); ok {
		v.SetVideoChroma(chroma)
	}
}
func (d *Hybrid) SupportsVideoCodec(codec string) bool {
	v, ok := d.Device.(interface{ SupportsVideoCodec(string) bool })
	return ok && v.SupportsVideoCodec(codec)
}
func (d *Hybrid) SetVideoStreamOptions(o *capture.VideoStreamOptions) {
	if v, ok := d.Device.(capture.VideoStreamConfigurer); ok {
		v.SetVideoStreamOptions(o)
	}
}
func (d *Hybrid) SupportsGameStreamHEVC() bool {
	v, ok := d.Device.(interface{ SupportsGameStreamHEVC() bool })
	return ok && v.SupportsGameStreamHEVC()
}
func (d *Hybrid) SupportsGameStreamAV1() bool {
	v, ok := d.Device.(interface{ SupportsGameStreamAV1() bool })
	return ok && v.SupportsGameStreamAV1()
}
func (d *Hybrid) RequestKeyframe() error {
	if v, ok := d.Device.(interface{ RequestKeyframe() error }); ok {
		return v.RequestKeyframe()
	}
	return fmt.Errorf("ios-usb: keyframe control unavailable")
}
func (d *Hybrid) PreserveCaptureMode() bool {
	v, ok := d.Device.(interface{ PreserveCaptureMode() bool })
	return ok && v.PreserveCaptureMode()
}
func (d *Hybrid) CaptureFPS() int {
	if v, ok := d.Device.(interface{ CaptureFPS() int }); ok {
		return v.CaptureFPS()
	}
	return 0
}
func (d *Hybrid) CaptureFrameRate() framerate.Rate {
	if v, ok := d.Device.(interface{ CaptureFrameRate() framerate.Rate }); ok {
		return v.CaptureFrameRate()
	}
	return framerate.New(d.CaptureFPS(), 1)
}
