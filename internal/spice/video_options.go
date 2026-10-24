package spice

import (
	"anykvm/internal/capture"
	"anykvm/internal/framerate"
)

// sharedPipeline is called under mu. Settings survive Close/reopen, while GPU
// allocations are released with the current session.
func (d *Device) sharedPipeline() *capture.DMABUFEncoder {
	if d.pipeline == nil {
		d.pipeline = capture.NewDMABUFEncoder(capture.DMABUFEncoderOptions{
			RequireGPUEncoding: true,
			IdleRefresh:        true,
		})
	}
	return d.pipeline
}

func (d *Device) SetDecodeImage(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetDecodeImage(v)
}
func (d *Device) SetJPEGQuality(v int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetJPEGQuality(v)
}
func (d *Device) SetVideoCodec(v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.codec = v
	d.sharedPipeline().SetVideoCodec(v)
}
func (d *Device) SupportsVideoCodec(codec string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sharedPipeline().SupportsVideoCodec(codec)
}
func (d *Device) SupportsGameStreamAV1() bool  { return d.SupportsVideoCodec("av1") }
func (d *Device) SupportsGameStreamHEVC() bool { return d.SupportsVideoCodec("hevc") }
func (d *Device) SetVideoChroma(v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetVideoChroma(v)
}
func (d *Device) SetVideoBitrate(v int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetVideoBitrate(v)
}
func (d *Device) SetVideoStreamOptions(v *capture.VideoStreamOptions) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stream = nil
	if v != nil {
		copy := *v
		d.stream = &copy
	}
	d.sharedPipeline().SetVideoStreamOptions(d.stream)
}
func (d *Device) SetOutputSize(w, h int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetOutputSize(w, h)
}
func (d *Device) SetOutputDownscaleOnly(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetOutputDownscaleOnly(v)
}
func (d *Device) SetOutputSizeLimit(w, h int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetOutputSizeLimit(w, h)
}
func (d *Device) SetGPUSurface(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetGPUSurface(v)
}
func (d *Device) SetJPEGRegions(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sharedPipeline().SetJPEGRegions(v)
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
	if !d.remote {
		return d.sharedPipeline().RequestKeyframe()
	}
	return nil
}
func (*Device) CaptureIdleStatus() (bool, string) { return true, "SPICE waits for GL desktop updates" }
