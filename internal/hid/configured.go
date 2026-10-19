//go:build !windows

package hid

import (
	"fmt"
	"os"
	"time"
)

// OpenConfigured opens a HID device using an explicit type and optional device
// path instead of auto-detection. typ can be "auto", "ch9329", "hid",
// "local", "gadget", "bluetooth", "ble", or "nrf52840".
// device is the serial port path; for BLE types a trailing :N selects profile
// slot 0-3 (e.g. /dev/cu.usbmodem14101:0), or pass slot explicitly.
// mouseMode is "absolute" (default) or "relative".
// slot is the BLE profile index when typ is ble/nrf52840; use SlotUnset to keep
// the dongle's current active slot.
func OpenConfigured(typ, device, mouseMode string, slot int) (*Device, error) {
	if typ == "ble" || typ == "nrf52840" {
		if path, pathSlot, ok := splitBLEDeviceSlot(device); ok {
			device = path
			if pathSlot >= 0 {
				slot = pathSlot
			}
		}
	}
	return openConfigured(typ, device, mouseMode, slot)
}

func openConfigured(typ, device, mouseMode string, slot int) (*Device, error) {
	abs := mouseMode != "relative"
	if typ == "auto" || typ == "" {
		if device == "" {
			return Open(abs)
		}
		if d, err := openSerialHIDAt(device); err == nil {
			n, ok := d.(*serialHIDDevice)
			if !ok {
				d.Close()
				return nil, fmt.Errorf("ble: unexpected device type")
			}
			if err := configureSerialSession(n, abs, slot); err != nil {
				d.Close()
				return nil, err
			}
			return &Device{impl: d}, nil
		}
		return nil, fmt.Errorf("hid: device type required for %q", device)
	}

	switch typ {
	case "local":
		return OpenLocal()
	case "gadget":
		d, err := openGadget()
		if err == nil {
			return &Device{impl: d}, nil
		}
		return nil, err
	case "bluetooth":
		d, err := openBluetooth()
		if err == nil {
			return &Device{impl: d}, nil
		}
		return nil, err
	}

	if device == "" {
		return nil, fmt.Errorf("hid: device path required for type %q", typ)
	}

	switch typ {
	case "ch9329":
		return openCH9329At(device, abs)
	case "hid":
		return openHIDBridgeAt(device, abs)
	case "ble", "nrf52840":
		d, err := openSerialHIDAt(device)
		if err != nil {
			return nil, err
		}
		n, ok := d.(*serialHIDDevice)
		if ok && n.lastStatus().Capabilities&capabilityBLE == 0 {
			d.Close()
			return nil, fmt.Errorf("hid: device has no BLE output")
		}
		if !ok {
			d.Close()
			return nil, fmt.Errorf("ble: unexpected device type")
		}
		if err := configureSerialSession(n, abs, slot); err != nil {
			d.Close()
			return nil, fmt.Errorf("ble: %w", err)
		}
		return &Device{impl: d}, nil
	default:
		return nil, fmt.Errorf("hid: unknown type %q", typ)
	}
}

func openCH9329At(port string, absMouse bool) (*Device, error) {
	f, err := os.OpenFile(port, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("ch9329: open %s: %w", port, err)
	}
	if err := setRawTermios(f, baud9600); err != nil {
		f.Close()
		return nil, fmt.Errorf("ch9329: configure %s: %w", port, err)
	}
	time.Sleep(100 * time.Millisecond)

	base := newCH9329Base(f, absMouse)
	if absMouse {
		return &Device{impl: &ch9329AbsDevice{ch9329Base: base}}, nil
	}
	return &Device{impl: &ch9329RelDevice{ch9329Base: base}}, nil
}

func openHIDBridgeAt(port string, absMouse bool) (*Device, error) {
	d, err := serialConnect(port)
	if err != nil {
		return nil, err
	}
	if err := configureSerialSession(d, absMouse, SlotUnset); err != nil {
		d.Close()
		return nil, err
	}
	return &Device{impl: d}, nil
}
