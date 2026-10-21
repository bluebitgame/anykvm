package hid

import (
	"fmt"
	"strings"
	"time"

	"anykvm/internal/logging"
)

// SerialStale reports whether err indicates the USB CDC port was reset or
// unplugged while a handle was still open (common on macOS during BLE work).
func SerialStale(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "device not configured") ||
		strings.Contains(msg, "not open") ||
		strings.Contains(msg, "bad file descriptor") ||
		strings.Contains(msg, "no such file or directory")
}

// ReopenBLELink closes prev and opens a fresh serial link without changing
// profile slot or mouse mode (safe while iPhone is pairing or already bonded).
func ReopenBLELink(prev *Device, port string) (*Device, error) {
	if prev != nil {
		_ = prev.Close()
	}
	time.Sleep(400 * time.Millisecond)
	n, err := serialConnect(port)
	if err != nil {
		return nil, err
	}
	return &Device{impl: n}, nil
}

// WaitBonded polls dongle status until the slot is bonded or timeout expires.
// It reopens the USB link automatically after macOS CDC re-enumeration.
// The returned *Device may differ from d when the serial port was reopened.
func WaitBonded(d *Device, port string, timeout time.Duration) (*Device, ProfileStatus, error) {
	deadline := time.Now().Add(timeout)
	staleNote := false
	var lastErr error

	for time.Now().Before(deadline) {
		time.Sleep(time.Second)

		ble, ok := d.BLE()
		if !ok {
			return d, ProfileStatus{}, fmt.Errorf("ble: device does not support profile management")
		}

		st, err := ble.ProfileStatus()
		if err != nil && SerialStale(err) {
			if !staleNote {
				staleNote = true
				logging.Infof("AnyKVM: USB serial reset — reopening %s", port)
			}
			next, reopenErr := ReopenBLELink(d, port)
			if reopenErr != nil {
				lastErr = fmt.Errorf("%w; reopen: %v", err, reopenErr)
			} else {
				d = next
				ble, ok = d.BLE()
				if !ok {
					return d, ProfileStatus{}, fmt.Errorf("ble: reopened device lost BLE controller")
				}
				st, err = ble.ProfileStatus()
				if err == nil {
					lastErr = nil
				}
			}
		}
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		if st.Bonded {
			return d, st, nil
		}
	}
	if lastErr != nil {
		return d, ProfileStatus{}, fmt.Errorf("pairing timed out (%s): last status error: %w", timeout, lastErr)
	}
	return d, ProfileStatus{}, fmt.Errorf("pairing timed out (%s)", timeout)
}
