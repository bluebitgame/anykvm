package ard

import (
	"anykvm/internal/vnc"
)

func (c *Client) input(fn func(*session) error) error {
	c.life.Lock()
	defer c.life.Unlock()
	s := c.current()
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return s.failure()
	default:
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return fn(s)
}
func (s *session) keyEvent(ks uint32, down bool) error {
	p := make([]byte, 8)
	p[0] = 4
	if down {
		p[1] = 1
	}
	put32(p[4:], ks)
	return s.write(p)
}
func (s *session) pointer(buttons byte) error {
	s.mu.Lock()
	w, h := s.w, s.h
	s.mu.Unlock()
	if w <= 0 || h <= 0 {
		return nil
	}
	s.x = min(max(s.x, 0), w-1)
	s.y = min(max(s.y, 0), h-1)
	p := make([]byte, 6)
	p[0], p[1] = 5, buttons
	put16(p[2:], s.x)
	put16(p[4:], s.y)
	return s.write(p)
}
func (c *Client) KeyDown(code uint8) error {
	return c.input(func(s *session) error {
		ks, ok := vnc.KeysymFromHID(code)
		if !ok {
			return nil
		}
		s.pressed[code] = ks
		return s.keyEvent(ks, true)
	})
}
func (c *Client) KeyRelease(code uint8) error {
	return c.input(func(s *session) error {
		ks, ok := vnc.KeysymFromHID(code)
		if !ok {
			return nil
		}
		delete(s.pressed, code)
		return s.keyEvent(ks, false)
	})
}
func (c *Client) KeyUp() error {
	return c.input(func(s *session) error { return s.releaseKeys() })
}
func (s *session) releaseKeys() error {
	// One bounded write releases every key, including on close or takeover.
	var p []byte
	for _, ks := range s.pressed {
		b := make([]byte, 8)
		b[0] = 4
		put32(b[4:], ks)
		p = append(p, b...)
	}
	clear(s.pressed)
	if len(p) == 0 {
		return nil
	}
	return s.write(p)
}
func (s *session) releaseInput() error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	select {
	case <-s.done:
		clear(s.pressed)
		s.buttons = 0
		return nil
	default:
	}
	err := s.releaseKeys()
	s.buttons = 0
	if err != nil {
		return err
	}
	return s.pointer(0)
}
func (c *Client) MouseMove(x, y int) error {
	return c.input(func(s *session) error {
		s.mu.Lock()
		w, h := s.w, s.h
		s.mu.Unlock()
		if w <= 0 || h <= 0 {
			return nil
		}
		s.x = min(max(x, 0), 32767) * (w - 1) / 32767
		s.y = min(max(y, 0), 32767) * (h - 1) / 32767
		return s.pointer(s.buttons)
	})
}
func (c *Client) MouseMoveRel(dx, dy int) error {
	return c.input(func(s *session) error {
		s.mu.Lock()
		w, h := s.w, s.h
		s.mu.Unlock()
		if w <= 0 || h <= 0 {
			return nil
		}
		s.x = min(max(s.x+dx, 0), w-1)
		s.y = min(max(s.y+dy, 0), h-1)
		return s.pointer(s.buttons)
	})
}
func (c *Client) MouseButton(button int, down bool) error {
	return c.input(func(s *session) error {
		if button < 1 || button > 3 {
			return nil
		}
		mask := [3]byte{1, 4, 2}[button-1]
		if down {
			s.buttons |= mask
		} else {
			s.buttons &^= mask
		}
		return s.pointer(s.buttons)
	})
}
func (c *Client) MouseWheel(delta int) error {
	return c.input(func(s *session) error {
		if delta == 0 {
			return nil
		}
		wheel := byte(8)
		if delta < 0 {
			wheel = 16
		}
		count := min(max(delta, -120), 120)
		if count < 0 {
			count = -count
		}
		for i := 0; i < count; i++ {
			if err := s.pointer(s.buttons | wheel); err != nil {
				return err
			}
			if err := s.pointer(s.buttons); err != nil {
				return err
			}
		}
		return nil
	})
}
