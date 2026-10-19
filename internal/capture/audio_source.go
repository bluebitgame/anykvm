package capture

import (
	"fmt"
	"sync"
)

// Resolve the provider after Open: USB discovery can replace the native device.
func (s *smartDevice) AudioDevice() AudioDevice {
	if !isIOSDevice(s.deviceID) {
		return nil
	}
	return &sourceAudioDevice{source: s}
}

type sourceAudioDevice struct {
	mu     sync.Mutex
	source *smartDevice
	active AudioDevice
}

func (d *sourceAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		return nil
	}
	p, ok := d.source.native.(AudioProvider)
	if !ok {
		return fmt.Errorf("ios-usb: source audio unavailable on this platform")
	}
	a := p.AudioDevice()
	if a == nil {
		return fmt.Errorf("ios-usb: source has no audio output")
	}
	if err := a.Open(); err != nil {
		return err
	}
	d.active = a
	return nil
}
func (d *sourceAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil, ErrStreamStopped
	}
	return d.active.Capture()
}
func (d *sourceAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	err := d.active.Close()
	d.active = nil
	return err
}
