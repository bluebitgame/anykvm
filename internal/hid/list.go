package hid

// ListBluetooth returns discovered Bluetooth HID device names.
// On platforms without Bluetooth scanning, returns nil.
func ListBluetooth() []string { return nil }

// ListGadget returns the USB gadget HID device path if available.
// On platforms without USB gadget support, returns ("", nil).
func ListGadget() (string, error) { return "", nil }
