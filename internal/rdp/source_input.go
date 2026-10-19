package rdp

import "fmt"

// Reverse the server scan-code map so upstream and downstream input
// use the same USB HID usages, including extended keys and modifiers.
func sourceScan(code uint8) (uint16, bool) {
	for _, extended := range []bool{false, true} {
		for scan := uint16(1); scan < 128; scan++ {
			if hidKey(scan, extended) == code {
				if extended {
					return scan | 0x100, true
				}
				return scan, true
			}
		}
	}
	return 0, false
}
func (s *sourceSession) input(kind uint16, flags, first, second uint16) error {
	event := join(u32(0), u16(int(kind)), u16(int(flags)), u16(int(first)), u16(int(second)))
	return s.sendMCS(s.channel, s.shareData(28, join(u16(1), u16(0), event)))
}
func (c *SourceClient) KeyDown(code uint8) error    { return c.sourceKey(code, true) }
func (c *SourceClient) KeyRelease(code uint8) error { return c.sourceKey(code, false) }
func (c *SourceClient) sourceKey(code uint8, down bool) error {
	s := c.current()
	if s == nil {
		return nil
	}
	scan, ok := sourceScan(code)
	if !ok {
		return fmt.Errorf("unsupported RDP HID usage %#x", code)
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	flags := scan & 0x100
	if !down {
		flags |= 0x8000
	}
	if e := s.input(4, flags, scan&0xff, 0); e != nil {
		return e
	}
	s.keys[code] = down
	return nil
}
func (c *SourceClient) KeyUp() error {
	s := c.current()
	if s == nil {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	var first error
	for code, held := range s.keys {
		if !held {
			continue
		}
		scan, ok := sourceScan(uint8(code))
		if ok {
			if e := s.input(4, 0x8000|scan&0x100, scan&0xff, 0); e != nil && first == nil {
				first = e
			}
		}
		s.keys[code] = false
	}

	return first
}
func (c *SourceClient) MouseMove(x, y int) error {
	s := c.current()
	if s == nil {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.mouseX = max(0, min(32767, x)) * max(0, s.width-1) / 32767
	s.mouseY = max(0, min(32767, y)) * max(0, s.height-1) / 32767
	return s.input(0x8001, 0x0800, uint16(s.mouseX), uint16(s.mouseY))
}
func (c *SourceClient) MouseMoveRel(dx, dy int) error {
	s := c.current()
	if s == nil {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.mouseX = max(0, min(s.width-1, s.mouseX+dx))
	s.mouseY = max(0, min(s.height-1, s.mouseY+dy))
	return s.input(0x8001, 0x0800, uint16(s.mouseX), uint16(s.mouseY))
}
func sourceButton(button int) uint16 {
	switch button {
	case 0:
		return 0x1000
	case 1:
		return 0x2000
	case 2:
		return 0x4000
	}
	return 0
}
func (c *SourceClient) MouseButton(button int, down bool) error {
	s := c.current()
	if s == nil {
		return nil
	}
	flag := sourceButton(button)
	if flag == 0 {
		return fmt.Errorf("unsupported RDP mouse button %d", button)
	}
	if down {
		flag |= 0x8000
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if e := s.input(0x8001, flag, uint16(s.mouseX), uint16(s.mouseY)); e != nil {
		return e
	}
	s.buttons[button] = down
	return nil
}
func (c *SourceClient) MouseWheel(delta int) error {
	s := c.current()
	if s == nil {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	for delta != 0 {
		step := max(-2, min(2, delta))
		flags := uint16(int16(step*120))&0x1ff | 0x200
		if e := s.input(0x8001, flags, uint16(s.mouseX), uint16(s.mouseY)); e != nil {
			return e
		}
		delta -= step
	}
	return nil
}
