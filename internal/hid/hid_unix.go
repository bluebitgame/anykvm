//go:build !windows

package hid

import (
	"fmt"
)

// Open finds a HID device. Tries in order: Linux gadget, CH9329, BLE, Bluetooth.
// absMouse controls the mouse mode for backends that support both absolute and
// relative positioning. true = absolute (default for VNC/WS).
func Open(absMouse bool) (*Device, error) {
	if d, err := openGadget(); err == nil {
		return &Device{impl: d}, nil
	}
	if d, err := openCH9329(absMouse); err == nil {
		return &Device{impl: d}, nil
	}
	if d, err := openSerialHID(absMouse); err == nil {
		return &Device{impl: d}, nil
	}
	if d, err := openBluetooth(); err == nil {
		return &Device{impl: d}, nil
	}
	return nil, fmt.Errorf("hid: no device found")
}
