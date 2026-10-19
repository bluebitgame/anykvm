package hid

import "sync"

// NewSessionDevice defers opening an external device until a target is controlled.
func NewSessionDevice(typ, path, mode string, slot int) *Device {
	return &Device{impl: &sessionDevice{open: func() (*Device, error) {
		dev, err := OpenConfigured(typ, path, mode, slot)
		if err != nil {
			return nil, err
		}
		if ble, ok := dev.BLE(); ok {
			if status, err := ble.ProfileStatus(); err == nil && !status.Bonded {
				_ = ble.ProfileAdvertise()
			}
		}
		return dev, nil
	}}}
}

type sessionDevice struct {
	mu   sync.Mutex
	open func() (*Device, error)
	dev  *Device
}

func (d *Device) StartSession() error {
	if session, ok := d.impl.(interface{ StartSession() error }); ok {
		return session.StartSession()
	}
	return nil
}

func (d *Device) StopSession() error {
	if session, ok := d.impl.(*sessionDevice); ok {
		return session.Close()
	}
	return nil
}

func (s *sessionDevice) StartSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev != nil {
		return nil
	}
	d, err := s.open()
	if err == nil {
		s.dev = d
	}
	return err
}

func (s *sessionDevice) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev == nil {
		return nil
	}
	_ = s.dev.KeyUp()
	for button := 1; button <= 3; button++ {
		_ = s.dev.MouseButton(button, false)
	}
	err := s.dev.Close()
	s.dev = nil
	return err
}

func (s *sessionDevice) send(f func(*Device) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev == nil {
		return nil
	}
	return f(s.dev)
}

func (s *sessionDevice) KeyDown(code uint8) error {
	return s.send(func(d *Device) error { return d.KeyDown(code) })
}
func (s *sessionDevice) KeyRelease(code uint8) error {
	return s.send(func(d *Device) error { return d.KeyRelease(code) })
}
func (s *sessionDevice) KeyUp() error { return s.send(func(d *Device) error { return d.KeyUp() }) }
func (s *sessionDevice) MouseMove(x, y int) error {
	return s.send(func(d *Device) error { return d.MouseMove(x, y) })
}
func (s *sessionDevice) MouseMoveRel(x, y int) error {
	return s.send(func(d *Device) error { return d.MouseMoveRel(x, y) })
}
func (s *sessionDevice) MouseButton(button int, down bool) error {
	return s.send(func(d *Device) error { return d.MouseButton(button, down) })
}
func (s *sessionDevice) MouseWheel(delta int) error {
	return s.send(func(d *Device) error { return d.MouseWheel(delta) })
}
