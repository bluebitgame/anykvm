package videoenc

import "sync"

var h264Availability struct {
	sync.Once
	supported bool
}

// SupportsH264 checks the platform encoder before capture advertises transcoding.
// Opening Raspberry Pi's native encoder includes its existing startup probe.
func SupportsH264() bool {
	h264Availability.Do(func() {
		encoder, err := New(Options{Width: 1280, Height: 720, FPS: 30, Bitrate: 4000, Codec: "h264"})
		if err == nil {
			h264Availability.supported = true
			encoder.Close()
		}
	})
	return h264Availability.supported
}
