package capture

import (
	"anykvm/internal/avmedia"
	"io"
	"sync"
)

// ffmpegAudioDevice uses libavdevice/libavcodec in the server process.
type ffmpegAudioDevice struct {
	device string
	mu     sync.Mutex
	source interface {
		Read() (*avmedia.Frame, error)
		Close()
	}
}

func NewFFmpegAudioDevice(device string) AudioDevice { return &ffmpegAudioDevice{device: device} }
func (d *ffmpegAudioDevice) Open() error {
	if !avmedia.Enabled {
		return avmedia.Available()
	}
	format, url, opts, err := mediaInputArgs(buildFFmpegAudioArgs(d.device))
	if err != nil {
		return err
	}
	source, err := avmedia.OpenSource(format, url, opts, true)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.source = source
	d.mu.Unlock()
	return nil
}
func (d *ffmpegAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	source := d.source
	d.mu.Unlock()
	if source == nil {
		return nil, io.ErrClosedPipe
	}
	f, err := source.Read()
	if err != nil || f == nil {
		return nil, err
	}
	return &AudioFrame{PCM: f.Data, SampleRate: AudioSampleRate, Channels: AudioChannels}, nil
}
func (d *ffmpegAudioDevice) Close() error {
	d.mu.Lock()
	source := d.source
	d.source = nil
	d.mu.Unlock()
	if source != nil {
		source.Close()
	}
	return nil
}
