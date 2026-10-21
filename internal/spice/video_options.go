package spice

import (
	"anykvm/internal/capture"
	"anykvm/internal/framerate"
)

func (d *Device) SetDecodeImage(bool) {} // Compressed source; never materialize pixels.
func (d *Device) SetJPEGQuality(int)  {}

func (d *Device) SetVideoCodec(codec string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.codec = codec
}

func (*Device) SupportsVideoCodec(codec string) bool { return codec == "" || codec == "h264" }

func (d *Device) SetVideoBitrate(bitrate int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bitrate = bitrate
}

func (d *Device) SetVideoStreamOptions(v *capture.VideoStreamOptions) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stream = nil
	if v != nil {
		copy := *v
		d.stream = &copy
	}
}

func (d *Device) SetOutputSize(w, h int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targetW, d.targetH = w, h
}

func (d *Device) CaptureFrameRate() framerate.Rate {
	d.mu.Lock()
	defer d.mu.Unlock()
	return framerate.New(max(1, d.fps), 1)
}

func (d *Device) RequestKeyframe() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.force = true
	return nil
}

func (*Device) CaptureIdleStatus() (bool, string) { return true, "SPICE waits for GL desktop updates" }
