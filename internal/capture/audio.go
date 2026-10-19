package capture

import (
	"errors"
)

var errAudioAllFailed = errors.New("audio: all backends failed")

// Unified audio capture format: 48kHz / 16-bit LE / stereo.
// 48kHz aligns with Opus native sample rate; no resampling needed if switching to Opus later.
const (
	AudioSampleRate    = 48000
	AudioChannels      = 2
	AudioBitsPerSample = 16
)

// AudioFrame carries either interleaved S16LE PCM or one compressed access unit.
// Codec uses the WebCodecs codec string; Config is its decoder description.
type AudioFrame struct {
	PCM             []byte
	SampleRate      int
	Channels        int
	Encoded         []byte
	Codec           string
	Config          []byte
	Timestamp       int64 // presentation time in microseconds, relative to the source
	FramesPerPacket int
	Gain            *float64 // nil means unity; source volume is applied after decoding
	Epoch           uint64   // changes on source restart/FLUSH to reset decoder history
}

// AudioProvider exposes audio from the video device's own session.
type AudioProvider interface{ AudioDevice() AudioDevice }

// AudioDevice is the audio capture device interface.
type AudioDevice interface {
	Open() error
	Capture() (*AudioFrame, error) // PCM or compressed AU; (nil, nil) when no data is available
	Close() error
}

// NewAudioDevice creates a platform audio capture device.
// deviceID: platform-specific audio input identifier (empty=default device).
func NewAudioDevice(deviceID string) AudioDevice {
	return newPlatformAudioDevice(deviceID)
}

// fallbackAudioDevice tries backends in order and uses the first successful one.
type fallbackAudioDevice struct {
	backends []AudioDevice
	active   AudioDevice
}

func (d *fallbackAudioDevice) Open() error {
	for _, be := range d.backends {
		if err := be.Open(); err == nil {
			d.active = be
			return nil
		}
	}
	return errAudioAllFailed
}

func (d *fallbackAudioDevice) Capture() (*AudioFrame, error) {
	return d.active.Capture()
}

func (d *fallbackAudioDevice) Close() error {
	if d.active != nil {
		return d.active.Close()
	}
	return nil
}
