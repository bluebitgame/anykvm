//go:build windows

package hid

import (
	"fmt"
	"time"
)

// Open finds a CH9329 HID device.
// absMouse selects absolute (true, default) or relative mouse mode for CH9329.
func Open(absMouse bool) (*Device, error) {
	if d, err := openSerialHID(absMouse); err == nil {
		return &Device{impl: d}, nil
	}
	if absMouse {
		if d, err := openCH9329Abs(); err == nil {
			return &Device{impl: d}, nil
		}
	} else {
		if d, err := openCH9329Rel(); err == nil {
			return &Device{impl: d}, nil
		}
	}
	return nil, fmt.Errorf("hid: no device found on COM ports")
}

func openCH9329Abs() (hidi, error) {
	port, err := findCOMPort()
	if err != nil {
		return nil, err
	}
	f, err := openWindowsSerial(port, baud9600)
	if err != nil {
		return nil, fmt.Errorf("ch9329: open %s: %w", port, err)
	}
	time.Sleep(100 * time.Millisecond)

	base := newCH9329Base(f, true)
	a := &ch9329AbsDevice{ch9329Base: base}

	return a, nil
}

func openCH9329Rel() (hidi, error) {
	port, err := findCOMPort()
	if err != nil {
		return nil, err
	}
	f, err := openWindowsSerial(port, baud9600)
	if err != nil {
		return nil, fmt.Errorf("ch9329: open %s: %w", port, err)
	}
	time.Sleep(100 * time.Millisecond)

	base := newCH9329Base(f, false)
	r := &ch9329RelDevice{ch9329Base: base}

	return r, nil
}

func findCOMPort() (string, error) {
	ports := ListSerialPorts()
	if len(ports) == 0 {
		return "", fmt.Errorf("no COM port found")
	}
	return ports[0], nil
}

// openCH9329 is kept for backward compatibility in the Windows build.
func openCH9329() (hidi, error) {
	return openCH9329Abs()
}
