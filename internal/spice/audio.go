package spice

import (
	"sync"

	"anykvm/internal/capture"
)

// Playback packets contain PCM already decoded by spice-client-glib (including
// negotiated Opus/CELT). They never pass through a host audio output device.
type audioPacket struct {
	pcm            []byte
	rate, channels int
	epoch, sample  uint64
	volume         [2]uint16
	mute           bool
}

type audioSession interface {
	setAudioEnabled(bool)
	nextAudio() (*audioPacket, error)
	audioAvailable() bool
}

type sourceAudio struct {
	device     *Device
	mu         sync.Mutex
	open       bool
	generation uint64
	converter  pcmConverter
}

var _ capture.AudioProvider = (*Device)(nil)
var _ capture.AudioDevice = (*sourceAudio)(nil)

func (d *Device) AudioDevice() capture.AudioDevice { return &sourceAudio{device: d} }

func (a *sourceAudio) Open() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.device
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if a.open {
		return nil
	}
	a.open = true
	a.generation = d.generation
	a.converter = pcmConverter{}
	d.audioEnabled = true
	if s, ok := d.session.(audioSession); ok {
		s.setAudioEnabled(true)
	}
	return nil
}

func (a *sourceAudio) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open {
		return nil
	}
	a.open = false
	a.converter = pcmConverter{}
	d := a.device
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	d.audioEnabled = false
	if s, ok := d.session.(audioSession); ok {
		s.setAudioEnabled(false)
	}
	return nil
}

func (a *sourceAudio) Capture() (*capture.AudioFrame, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open {
		return nil, capture.ErrStreamStopped
	}
	d := a.device
	d.sessionMu.RLock()
	defer d.sessionMu.RUnlock()
	if d.session == nil {
		return nil, nil
	}
	if a.generation != d.generation {
		a.converter = pcmConverter{}
		a.generation = d.generation
	}
	s, ok := d.session.(audioSession)
	if !ok {
		return nil, nil
	}
	packet, err := s.nextAudio()
	if err != nil {
		return nil, err
	}
	if packet == nil {
		return nil, nil
	}
	frame, err := a.converter.convert(*packet)
	if frame != nil {
		// Reserve the upper bits for session reconnects and lower bits for
		// playback restarts within that session.
		frame.Epoch = d.generation<<32 | packet.epoch&0xffffffff
		d.audioFrames.Add(1)
	}
	return frame, err
}

func (a *sourceAudio) AudioAvailable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open {
		return false
	}
	d := a.device
	d.sessionMu.RLock()
	defer d.sessionMu.RUnlock()
	s, ok := d.session.(audioSession)
	return ok && s.audioAvailable()
}
