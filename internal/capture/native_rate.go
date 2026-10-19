package capture

import (
	"anykvm/internal/framerate"
	"fmt"
)

// Omitted FPS preserves the device's rational cadence, including NTSC rates.
func nativeCaptureRate(device framerate.Rate, requested int) (framerate.Rate, error) {
	if requested > 0 {
		return framerate.New(requested, 1), nil
	}
	if device.Valid() {
		return framerate.New(device.Num, device.Den), nil
	}
	return framerate.Rate{}, fmt.Errorf("native capture device did not report a frame rate; specify FPS in video.mode")
}
