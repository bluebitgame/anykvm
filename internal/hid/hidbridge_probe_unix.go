//go:build !windows

package hid

// ProbeHIDBridge identifies a USB HID or PS/2 output by its AnyKVM v1 capabilities.
func ProbeHIDBridge(port string) bool {
	return probeSerialPort(port, baud115200, probeHIDBridgeConnection)
}
