//go:build !windows

package hid

import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// serialHIDCandidates returns serial ports likely to host a USB CDC dongle.
// On macOS only cu.* call-out ports are used: opening tty.usbmodem* can block
// indefinitely waiting for carrier detect.
func serialHIDCandidates() []string {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil
	}
	var ports []string
	for _, e := range entries {
		name := e.Name()
		var ok bool
		switch runtime.GOOS {
		case "darwin":
			ok = strings.HasPrefix(name, "cu.usbmodem") ||
				strings.HasPrefix(name, "cu.usbserial") ||
				strings.HasPrefix(name, "cu.wchusbserial") ||
				strings.HasPrefix(name, "cu.SLAB_USBtoUART")
		default:
			ok = strings.HasPrefix(name, "ttyACM") ||
				strings.HasPrefix(name, "ttyUSB") ||
				strings.HasPrefix(name, "ttyAMA") ||
				strings.HasPrefix(name, "cu.usbmodem")
		}
		if ok {
			ports = append(ports, "/dev/"+name)
		}
	}
	return ports
}

func openHIDSerialFile(port string) (*os.File, error) {
	// Configure CLOCAL before permitting a blocking read, without waiting for
	// carrier detect or acquiring the port as this process's controlling TTY.
	return os.OpenFile(port, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
}

func openProbeSerialFile(port string) (*os.File, error) {
	return os.OpenFile(port, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
}
