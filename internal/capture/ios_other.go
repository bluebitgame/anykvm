//go:build !darwin

package capture

import "errors"

// resolveIOSDevice is only implemented on macOS: iOS USB screen mirroring
// (the QuickTime/CoreMediaIO path) is an Apple-only protocol.
func resolveIOSDevice(string) (string, error) {
	return "", errors.New("ios capture (USB screen mirroring) requires macOS")
}
