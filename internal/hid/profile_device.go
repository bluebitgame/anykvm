package hid

// ProfileDevice wraps a BLE-capable HID device with a specific profile slot.
// It satisfies both bridge.HIDSender and bridge.SlotSelecter.
type ProfileDevice struct {
	dev  *Device
	ble  BLEController
	slot int
}

// NewProfileDevice binds a shared BLE dongle to a target profile slot.
func NewProfileDevice(dev *Device, slot int) *ProfileDevice {
	ble, _ := dev.BLE()
	return &ProfileDevice{dev: dev, ble: ble, slot: slot}
}

func (s *ProfileDevice) Slot() int { return s.slot }

func (s *ProfileDevice) SelectSlot(slot int) error {
	if s.ble == nil {
		return nil
	}
	return s.ble.ProfileSelect(slot)
}

func (s *ProfileDevice) KeyDown(code uint8) error             { return s.dev.KeyDown(code) }
func (s *ProfileDevice) KeyUp() error                         { return s.dev.KeyUp() }
func (s *ProfileDevice) KeyRelease(code uint8) error          { return s.dev.KeyRelease(code) }
func (s *ProfileDevice) MouseMove(dx, dy int) error           { return s.dev.MouseMove(dx, dy) }
func (s *ProfileDevice) MouseMoveRel(dx, dy int) error        { return s.dev.MouseMoveRel(dx, dy) }
func (s *ProfileDevice) MouseButton(btn int, down bool) error { return s.dev.MouseButton(btn, down) }
func (s *ProfileDevice) MouseWheel(delta int) error           { return s.dev.MouseWheel(delta) }
