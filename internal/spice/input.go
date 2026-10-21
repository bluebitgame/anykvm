package spice

import (
	"fmt"

	"anykvm/internal/hid"
)

// Values match the input dispatch in native_linux.c.
const (
	inputKeyPress = iota
	inputKeyRelease
	inputPosition
	inputMotion
	inputButtonPress
	inputButtonRelease
)

func (d *Device) KeyDown(code uint8) error { return d.key(code, true) }

func (d *Device) KeyRelease(code uint8) error { return d.key(code, false) }

func (d *Device) key(code uint8, down bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil
	}
	scan := hid.PCScanCode(code)
	if scan == 0 || scan > 0x1ff {
		return fmt.Errorf("unsupported SPICE HID usage %#x", code)
	}
	kind := inputKeyRelease
	if down {
		kind = inputKeyPress
	}
	if err := d.session.input(kind, int(scan), 0, d.buttons); err != nil {
		return err
	}
	d.keys[code] = down
	return nil
}

func (d *Device) releaseKeys() error {
	var first error
	for code, held := range d.keys {
		if held {
			if err := d.session.input(inputKeyRelease, int(hid.PCScanCode(uint8(code))), 0, d.buttons); err != nil && first == nil {
				first = err
			}
			d.keys[code] = false
		}
	}
	return first
}

func (d *Device) KeyUp() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil
	}
	return d.releaseKeys()
}

func (d *Device) MouseMove(x, y int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil || d.width == 0 {
		return nil
	}
	return d.session.input(inputPosition, max(0, min(32767, x))*(d.width-1)/32767, max(0, min(32767, y))*(d.height-1)/32767, d.buttons)
}

func (d *Device) MouseMoveRel(x, y int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil
	}
	return d.session.input(inputMotion, x, y, d.buttons)
}

func (d *Device) MouseButton(button int, down bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil
	}
	if button < 1 || button > 3 {
		return fmt.Errorf("invalid SPICE mouse button")
	}
	// AnyKVM HID is left/right/middle; SPICE is left/middle/right.
	button = [3]int{1, 3, 2}[button-1]
	buttons := d.buttons
	mask := 1 << uint(button-1)
	kind := inputButtonRelease
	if down {
		buttons |= mask
		kind = inputButtonPress
	} else {
		buttons &^= mask
	}
	if err := d.session.input(kind, button, 0, buttons); err != nil {
		return err
	}
	d.buttons = buttons
	return nil
}

func (d *Device) MouseWheel(delta int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil
	}
	button := 4
	if delta < 0 {
		button = 5
		delta = -max(delta, -120)
	}
	delta = min(delta, 120)
	for i := 0; i < delta; i++ {
		if err := d.session.input(inputButtonPress, button, 0, d.buttons); err != nil {
			return err
		}
		if err := d.session.input(inputButtonRelease, button, 0, d.buttons); err != nil {
			return err
		}
	}
	return nil
}
