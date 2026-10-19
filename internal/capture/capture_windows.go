//go:build windows

package capture

// Windows sources implement their own native capture and encoding selection.
func newNativeDevice(deviceID string) Device { return newWindowsDevice(deviceID) }

const nativeIsFFmpeg = true

func newScreenDevice(deviceID string) Device { return newWindowsDevice(deviceID) }
