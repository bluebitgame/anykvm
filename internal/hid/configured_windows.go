//go:build windows

package hid

import (
	"fmt"
	"time"
)

func OpenConfigured(typ, device, mouseMode string, slot int) (*Device, error) {
	absMouse := mouseMode != "relative"
	if typ == "local" {
		return OpenLocal()
	}
	if typ == "ble" || typ == "nrf52840" || typ == "hid" {
		if device == "" {
			d, err := openSerialAdapter(absMouse, typ != "hid")
			if err != nil {
				return nil, err
			}
			return &Device{impl: d}, nil
		}
		if path, pathSlot, ok := splitBLEDeviceSlot(device); ok {
			device = path
			if pathSlot >= 0 {
				slot = pathSlot
			}
		}
		d, err := serialConnect(device)
		if err != nil {
			return nil, err
		}
		if typ != "hid" && d.lastStatus().Capabilities&capabilityBLE == 0 {
			d.Close()
			return nil, fmt.Errorf("hid: device has no BLE output")
		}
		if err := configureSerialSession(d, absMouse, slot); err != nil {
			d.Close()
			return nil, err
		}
		return &Device{impl: d}, nil
	}
	if typ == "auto" || typ == "" {
		if device == "" {
			return Open(absMouse)
		}
		return nil, fmt.Errorf("hid: device type required for %q", device)
	}

	if device == "" {
		var err error
		device, err = findCOMPort()
		if err != nil {
			return nil, err
		}
	}

	baud := uint32(baud115200)
	if typ == "ch9329" {
		baud = baud9600
	}
	f, err := openWindowsSerial(device, baud)
	if err != nil {
		return nil, fmt.Errorf("hid: open %s: %w", device, err)
	}

	switch typ {
	case "ch9329":
		time.Sleep(100 * time.Millisecond)
		base := newCH9329Base(f, absMouse)
		if absMouse {
			return &Device{impl: &ch9329AbsDevice{ch9329Base: base}}, nil
		}
		return &Device{impl: &ch9329RelDevice{ch9329Base: base}}, nil
	default:
		f.Close()
		return nil, fmt.Errorf("hid: unsupported type %q on Windows", typ)
	}
}
