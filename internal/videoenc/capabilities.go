package videoenc

import (
	"anykvm/internal/av1"
	"sync"
)

var av1Probe struct {
	sync.Once
	supported bool
}

// SupportsAV1 checks the driver by encoding a small recovery frame once. Merely
// having an av1_* encoder in libavcodec does not mean the GPU supports it.
// New never falls back to a software encoder for AV1.
func SupportsAV1() bool {
	av1Probe.Do(func() { av1Probe.supported = probeAV1(New) })
	return av1Probe.supported
}
func probeAV1(open func(Options) (Encoder, error)) bool {
	o := Options{Width: 1280, Height: 720, FPS: 30, Bitrate: 4000, Codec: "av1"}
	e, err := open(o)
	if err != nil {
		return false
	}
	defer e.Close()
	pixels := make([]byte, o.Width*o.Height*3/2)
	for i := 0; i < o.Width*o.Height; i++ {
		pixels[i] = 16
	}
	for i := o.Width * o.Height; i < len(pixels); i++ {
		pixels[i] = 128
	}
	for i := 0; i < 4; i++ {
		data, err := e.Encode(pixels, true)
		if err != nil {
			return false
		}
		if len(data) == 0 {
			continue
		}
		info, err := av1.Inspect(data)
		return err == nil && info.Recovery
	}
	return false
}
