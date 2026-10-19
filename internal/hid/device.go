package hid

import "errors"

// TextSender is an optional backend capability for committed Unicode text.
type TextSender interface {
	SendText(text string) error
}

var ErrTextUnsupported = errors.New("HID backend does not support Unicode text")

// Device wraps a platform-specific HID implementation.
type Device struct {
	impl hidi
}

type hidi interface {
	KeyDown(keycode uint8) error
	KeyUp() error
	KeyRelease(keycode uint8) error
	MouseMove(dx, dy int) error
	MouseMoveRel(dx, dy int) error
	MouseButton(button int, down bool) error
	MouseWheel(delta int) error
	Close() error
}

func (d *Device) KeyDown(keycode uint8) error          { return d.impl.KeyDown(keycode) }
func (d *Device) KeyUp() error                         { return d.impl.KeyUp() }
func (d *Device) KeyRelease(code uint8) error          { return d.impl.KeyRelease(code) }
func (d *Device) MouseMove(dx, dy int) error           { return d.impl.MouseMove(dx, dy) }
func (d *Device) MouseMoveRel(dx, dy int) error        { return d.impl.MouseMoveRel(dx, dy) }
func (d *Device) MouseButton(btn int, down bool) error { return d.impl.MouseButton(btn, down) }
func (d *Device) MouseWheel(delta int) error           { return d.impl.MouseWheel(delta) }
func (d *Device) Close() error                         { return d.impl.Close() }

// SetDisplayID selects the local display receiving normalized mouse input.
// External HID backends already use device coordinates and need no change.
func (d *Device) SetDisplayID(id uint32) {
	if target, ok := d.impl.(interface{ SetDisplayID(uint32) }); ok {
		target.SetDisplayID(id)
	}
}

func (d *Device) SendText(text string) error {
	if sender, ok := d.impl.(TextSender); ok {
		return sender.SendText(text)
	}
	return ErrTextUnsupported
}

// GamepadReport is a snapshot of a game controller state. Reserved for a future
// gamepad iteration; no backend implements GamepadSender yet.
type GamepadReport struct {
	Buttons uint16 // button bitmask
	LX, LY  int8   // left stick
	RX, RY  int8   // right stick
	LT, RT  uint8  // triggers
	Hat     uint8  // D-pad hat switch
}

// GamepadSender is an optional capability for HID backends that can emit game
// controller reports. Backends implement it as needed; callers detect support
// via Device.Gamepad(). Reserved: not yet implemented by any backend.
type GamepadSender interface {
	GamepadState(g GamepadReport) error
}

// Gamepad returns the underlying gamepad sender if the active backend supports
// it. Currently always (nil, false) — the interface exists so the input path
// and serial/USB protocol can grow gamepad support without breaking callers.
func (d *Device) Gamepad() (GamepadSender, bool) {
	gs, ok := d.impl.(GamepadSender)
	return gs, ok
}

// OpenLocal opens a HID backend that injects input into the LOCAL machine
// (for local-machine-as-target mode). It is implemented per-platform; platforms
// without support return an error.
func OpenLocal() (*Device, error) {
	impl, err := openLocal()
	if err != nil {
		return nil, err
	}
	return &Device{impl: impl}, nil
}
