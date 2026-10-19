package hid

// USB HID and PS/2 share the serial HID selector. Discovery is read-only: no profile selection or output changes.
func probeHIDBridgeConnection(rw serialReadWriter) bool {
	d := newSerialDeviceRW(rw)
	defer d.closeLink()
	if probeSerialDeviceWithTimeout(d, serialProbeReplyTimeout, serialProbeRetryDelay) != nil {
		return false
	}
	return d.lastStatus().Capabilities&(capabilityUSB|capabilityPS2) != 0
}
