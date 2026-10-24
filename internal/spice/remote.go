package spice

import (
	"anykvm/internal/capture"
	"anykvm/internal/framerate"
)

// Project only raw-capture capabilities. In particular, remote capture must not
// advertise VideoStreamConfigurer: GameStream needs its downstream encoder.
type remoteDevice struct {
	capture.Device
	sourceInput
	source *Device
}

type sourceInput interface {
	KeyDown(uint8) error
	KeyRelease(uint8) error
	KeyUp() error
	MouseMove(int, int) error
	MouseMoveRel(int, int) error
	MouseButton(int, bool) error
	MouseWheel(int) error
}

func wrapRemote(d *Device) *remoteDevice {
	return &remoteDevice{Device: d, sourceInput: d, source: d}
}

func (*remoteDevice) SupportsVideoCodec(codec string) bool { return codec == "" || codec == "rawvideo" }

func (d *remoteDevice) SetVideoCodec(codec string) { d.source.SetVideoCodec(codec) }

func (d *remoteDevice) AudioDevice() capture.AudioDevice { return d.source.AudioDevice() }

func (*remoteDevice) UsesLocalCursor() bool { return false }

func (d *remoteDevice) CaptureCursor() (*capture.CursorState, error) { return d.source.CaptureCursor() }

func (d *remoteDevice) RequestKeyframe() error { return d.source.RequestKeyframe() }

func (d *remoteDevice) CaptureFrameRate() framerate.Rate { return d.source.CaptureFrameRate() }

func (*remoteDevice) CaptureIdleStatus() (bool, string) {
	return true, "SPICE waits for desktop updates"
}

func (d *remoteDevice) Stats() Stats { return d.source.Stats() }
