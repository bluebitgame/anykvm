package vnc

import (
	"encoding/binary"
	"fmt"
	"io"
)

// secTypeName returns a readable name for a security type byte.
func secTypeName(t byte) string {
	switch t {
	case secTypeNone:
		return "None"
	case secTypeVNC:
		return "VNC"
	case secTypeVeNCrypt:
		return "VeNCrypt"
	default:
		return fmt.Sprintf("%d", t)
	}
}

// secTypesString formats a slice of security type bytes as readable names.
func secTypesString(types []byte) []string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = secTypeName(t)
	}
	return s
}

// RFB protocol constants
const (
	protoVersion = "RFB 003.008\n"

	// Security
	secTypeNone     = 1
	secTypeVNC      = 2
	secTypeVeNCrypt = 19
	secResultOK     = 0
	secResultFailed = 1

	// Client-to-server message types
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
	msgEnableContinuousUpdates  = 150

	// Server-to-client message types
	msgFramebufferUpdate      = 0
	msgEndOfContinuousUpdates = 150

	// Encodings
	encRaw               = 0
	encCopyRect          = 1
	encHextile           = 5
	encZlib              = 6
	encTight             = 7
	encJPEG              = 21
	encZRLE              = 16
	encH264              = 50 // Open H.264 Encoding
	encCursor            = -239
	encXCursor           = -240
	encCursorWithAlpha   = -314
	encDesktopSize       = -223
	encContinuousUpdates = -313
	encQEMUAudio         = -259 // QEMU Audio pseudo-encoding
	encQEMUPointerMotion = -263 // QEMU Pointer Motion Change pseudo-encoding

	// QEMU message types (server-to-client optional message 255)
	msgQEMU          = 255
	submsgQEMUAudio  = 1
	submsgQEMUPtrMot = 0 // Pointer Motion Change: [state u8] (0=absolute, 1=relative)

	// QEMU Audio client->server operations
	qemuAudioEnable  = 0
	qemuAudioDisable = 1
	qemuAudioSetFmt  = 2

	// QEMU Audio server->client operations
	qemuAudioEnd   = 0
	qemuAudioStart = 1
	qemuAudioData  = 2

	// QEMU Audio sample format IDs
	qemuSampleFmtU8    = 0
	qemuSampleFmtS16   = 3
	qemuSampleChannels = 2
	qemuSampleRate     = 48000

	// JPEG Quality Level pseudo-encodings (-23 = high .. -32 = low)
	encJPEGQualityHigh = -23
	encJPEGQualityLow  = -32

	// Tight compression control byte.
	// JpegCompression: bit7=1, bits7-4 = 1001b => 0x90. No filter byte follows.
	tightJPEG = 0x90

	// defaultJPEGQuality is used when the client does not advertise a JPEG
	// Quality Level pseudo-encoding.
	defaultJPEGQuality = 75

	maxClientMessage = 256
)

var pixelFormatBGR = []byte{
	32,     // bits-per-pixel
	24,     // depth
	0,      // big-endian-flag (little endian)
	1,      // true-color-flag
	0, 255, // red-max
	0, 255, // green-max
	0, 255, // blue-max
	16,      // red-shift
	8,       // green-shift
	0,       // blue-shift
	0, 0, 0, // padding
}

// writeU8 writes a uint8 to buffer
func writeU8(buf []byte, off int, v uint8) int {
	buf[off] = v
	return off + 1
}

// writeU16 writes a uint16 big-endian to buffer
func writeU16(buf []byte, off int, v uint16) int {
	binary.BigEndian.PutUint16(buf[off:], v)
	return off + 2
}

// writeU32 writes a uint32 big-endian to buffer
func writeU32(buf []byte, off int, v uint32) int {
	binary.BigEndian.PutUint32(buf[off:], v)
	return off + 4
}

// writeI32 writes an int32 big-endian to buffer
func writeI32(buf []byte, off int, v int32) int {
	return writeU32(buf, off, uint32(v))
}

type rect struct {
	x, y          uint16
	width, height uint16
	encoding      int32
	data          []byte
}

func readU8(buf []byte, off int) (uint8, int) {
	return buf[off], off + 1
}

func readU16(buf []byte, off int) (uint16, int) {
	return binary.BigEndian.Uint16(buf[off:]), off + 2
}

func readU32(buf []byte, off int) (uint32, int) {
	return binary.BigEndian.Uint32(buf[off:]), off + 4
}

// writeCompactLen writes a compact length (1-3 bytes) to an io.Writer.
func writeCompactLen(w io.Writer, length int) error {
	var buf [3]byte
	n := encodeCompactLen(buf[:], length)
	_, err := w.Write(buf[:n])
	return err
}

// encodeCompactLen encodes a length in compact 1-3 byte format, returns bytes used.
// The first byte holds bits 0-6, second byte holds bits 7-13, third holds bits 14-20.
// Bit 7 of each byte (except the last) is set to indicate more bytes follow.
func encodeCompactLen(out []byte, length int) int {
	lo := length & 0x7F         // bits 0-6
	mid := (length >> 7) & 0x7F // bits 7-13
	hi := (length >> 14) & 0x7F // bits 14-20

	if mid == 0 && hi == 0 {
		out[0] = byte(lo)
		return 1
	}
	if hi == 0 {
		out[0] = byte(lo) | 0x80
		out[1] = byte(mid)
		return 2
	}
	out[0] = byte(lo) | 0x80
	out[1] = byte(mid) | 0x80
	out[2] = byte(hi)
	return 3
}
