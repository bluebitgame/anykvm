//go:build windows

package hid

// openWindowsSerial applies the Win32 \\.\ prefix when opening a COM port.
// Keep the config value as COMx so the same spelling appears in the UI.
func platformDevicePath(path string) string { return path }
