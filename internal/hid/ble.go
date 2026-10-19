package hid

// ProfileStatus is a snapshot of the BLE HID dongle's state.
type ProfileStatus struct {
	Version      int    // AnyKVM serial protocol version
	Capabilities byte   // keyboard, relative, absolute, BLE, profiles, USB, PS/2
	Model        byte   // 1=Pico, 2=nRF52840, 3=ESP32-S3
	Ready        bool   // dongle has sent ready (boot complete)
	Slot         int    // active profile slot (0-3)
	Connected    bool   // a host is connected on the active slot
	Bonded       bool   // the active slot has a stored bond
	Advertising  bool   // the active slot is currently advertising
	MouseMode    string // "absolute" or "relative"
	SlotCount    int    // total profile slots (from ready reply)
}

// BLEEvent is an asynchronous event from the dongle.
type BLEEvent struct {
	Type string // "connected" or "disconnected"
	Slot int    // profile slot the event occurred on
}

// BLEController is an optional capability for HID backends that expose BLE
// profile management (nRF52840 and ESP32-S3 firmware). Implemented by serialHIDDevice.
type BLEController interface {
	ProfileSelect(slot int) error
	ProfileClear(slot int) error
	ProfileAdvertise() error
	ProfileStatus() (ProfileStatus, error)
	ProfileEvents() <-chan BLEEvent
	SetMouseMode(absolute bool) error
	Pair(slot int) error
}

// BLE returns the underlying BLE controller if the active backend supports
// profile management. Returns (nil, false) for HID Bridge /
// CH9329 backends.
func (d *Device) BLE() (BLEController, bool) {
	if serial, ok := d.impl.(*serialHIDDevice); ok && serial.lastStatus().Capabilities&capabilityBLE == 0 {
		return nil, false
	}
	bc, ok := d.impl.(BLEController)
	return bc, ok
}

// BLEDeviceInfo describes a discovered compatible BLE HID dongle.
type BLEDeviceInfo struct {
	Name      string // firmware model name
	Port      string // serial port path (e.g. "/dev/cu.usbmodemXXXX")
	SlotCount int    // total profile slots
	Slots     [4]BLEStatus
}

// BLEStatus holds a single slot's bond state from auto-detection scan.
type BLEStatus struct {
	Bonded bool
}
