package hid

const serialProtocolVersion = 1
const (
	capabilityKeyboard byte = 1 << iota
	capabilityRelative
	capabilityAbsolute
	capabilityBLE
	capabilityProfiles
	capabilityUSB
	capabilityPS2
)

// serialHIDFrame encodes the common AnyKVM v1 envelope used for requests
// and replies by every serial HID adapter.
func serialHIDFrame(command byte, data [4]byte) []byte {
	frame := []byte{0x33, command, data[0], data[1], data[2], data[3], 0, 0}
	crc := crc16modbus(frame[:6])
	frame[6] = byte(crc >> 8)
	frame[7] = byte(crc)
	return frame
}

func serialModelName(model byte) string {
	switch model {
	case 1:
		return "Pico"
	case 2:
		return "nRF52840"
	case 3:
		return "ESP32-S3"
	}
	return "AnyKVM HID"
}
