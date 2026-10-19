package capture

import "anykvm/internal/framerate"

// VideoStreamOptions are negotiated by the streaming client for the next open.
// Changing them requires closing and reopening the capture device.
type VideoStreamOptions struct {
	H264Profile                  string // Native VideoToolbox: empty/constrained_baseline, baseline, main or high.
	DisableLowLatencyRateControl bool   // Native VideoToolbox H.264 only; zero keeps low-latency rate control enabled.
	Codec                        string // empty means H.264; HEVC/AV1 are negotiated explicitly
	Width, Height, FPS, Bitrate  int
}

// VideoStreamConfigurer identifies capture devices that can encode GameStream
// video in their capture process. A nil profile restores normal capture output.
type VideoStreamConfigurer interface {
	SetVideoStreamOptions(*VideoStreamOptions)
}

// EncoderBufferConfigurer sets the native encoder in-flight limit before
// Open. Zero preserves backend defaults; supported values are 1..8.
type EncoderBufferConfigurer interface {
	SetEncoderBufferFrames(int)
}

// VideoBitrateConfigurer adjusts native encoding without overriding capture
// size or frame rate. Called before Open; zero restores the backend default.
// A negotiated VideoStreamOptions profile takes precedence over this value.
type VideoBitrateConfigurer interface {
	SetVideoBitrate(kbps int)
}

// VideoBitrateAutoConfigurer lets a protocol leave rate control to the native
// encoder instead of applying a target bitrate. It is separate from
// VideoBitrateConfigurer so zero can keep its existing "backend default"
// meaning for other consumers.
type VideoBitrateAutoConfigurer interface {
	SetVideoBitrateAuto(auto bool)
}

// GameStreamCaptureConfigurer selects capture settings for the current consumer
// without implying that the device can encode a negotiated stream directly.
// Bridge calls it before each Open; nil restores normal Web/VNC capture.
type GameStreamCaptureConfigurer interface {
	SetGameStreamCaptureOptions(*VideoStreamOptions)
}

func (d *ffmpegDevice) SetVideoStreamOptions(o *VideoStreamOptions) { d.streamOptions = o }

// Client settings never change capture-card or iOS USB source dimensions.
func (s *smartDevice) SetVideoStreamOptions(o *VideoStreamOptions) { s.streamOptions = o }

func (s *smartDevice) RequestKeyframe() error {
	s.devMu.Lock()
	dev := s.dev
	s.devMu.Unlock()
	if encoder, ok := dev.(interface{ RequestKeyframe() error }); ok {
		return encoder.RequestKeyframe()
	}
	// A compressed device without keyframe control must supply periodic IDRs.
	return nil
}

// CaptureCardRate never raises the source rate. Unknown rates are not decimated.
func CaptureCardRate(source, requested int) (fps, divisor int) {
	rate, divisor := framerate.New(source, 1).Decimate(requested)
	return rate.Round(), divisor
}
func (s *smartDevice) CaptureFrameRate() framerate.Rate {
	if source, ok := s.activeBackend().(interface{ CaptureFrameRate() framerate.Rate }); ok {
		if rate := source.CaptureFrameRate(); rate.Valid() {
			return rate
		}
	}
	return framerate.New(s.CaptureFPS(), 1)
}

func (s *smartDevice) PreserveCaptureMode() bool  { return true }
func (d *ffmpegDevice) PreserveCaptureMode() bool { return d.argsFunc == nil && !d.scalableInput }

func (d *ffmpegDevice) SetVideoBitrate(kbps int) { d.videoBitrate = kbps }
func (s *smartDevice) SetVideoBitrate(kbps int) {
	s.videoBitrate = kbps
	if native, ok := s.native.(VideoBitrateConfigurer); ok {
		native.SetVideoBitrate(kbps)
	}
}
