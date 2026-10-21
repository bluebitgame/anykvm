package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// updateGPURegions applies a compact GPU readback to an immutable compressed
// snapshot. Records are [MCU index, tagged length, color descriptor, entropy].
// A tagged constant contains only three packed DCs, with no entropy payload. In GPU
// append order; duplicate indices are rejected. The
// encoder retains only this CPU snapshot; consumers may safely retain older ones
// through dropped frames, coalescing and framebuffer refresh requests.
func updateGPURegions(previous *RestartJPEG, header, packet []byte, width, height int) (*RestartJPEG, error) {
	invalid := func() (*RestartJPEG, error) { return nil, fmt.Errorf("invalid GPU JPEG region packet") }
	if width < 1 || height < 1 || width > 8192 || height > 8192 || len(packet) < 4 {
		return invalid()
	}
	count := ((width + 15) / 16) * ((height + 15) / 16)
	if int(binary.LittleEndian.Uint32(packet)) != count {
		return invalid()
	}
	if len(packet) == 4 && previous.HasRecordedColors() && previous.Width == width && previous.Height == height && bytes.Equal(previous.header, header) {
		return previous, nil
	}
	next := &RestartJPEG{Width: width, Height: height, header: bytes.Clone(header), segments: make([][]byte, count), solidColors: make([]uint32, count), solidDC: make([][3]int16, count)}
	// The header is generated locally from jpegFrameParameters, never from the
	// device. Walk marker lengths rather than searching inside quantizer tables.
	for pos := 2; pos+4 <= len(header); {
		if header[pos] != 0xff {
			return invalid()
		}
		length := int(binary.BigEndian.Uint16(header[pos+2:]))
		if length < 2 || pos+2+length > len(header) {
			return invalid()
		}
		if header[pos+1] == 0xc0 {
			next.sof = pos + 4
		}
		pos += 2 + length
	}
	if next.sof == 0 || next.sof+5 > len(header) || int(binary.BigEndian.Uint16(header[next.sof+1:])) != height || int(binary.BigEndian.Uint16(header[next.sof+3:])) != width {
		return invalid()
	}
	if next.SameParameters(previous) && previous.HasRecordedColors() {
		copy(next.segments, previous.segments)
		copy(next.solidColors, previous.solidColors)
		copy(next.solidDC, previous.solidDC)
	}
	seen := make([]bool, count)
	for pos := 4; pos < len(packet); {
		if len(packet)-pos < 12 {
			return invalid()
		}
		index := int(binary.LittleEndian.Uint32(packet[pos:]))
		tagged := binary.LittleEndian.Uint32(packet[pos+4:])
		descriptor := binary.LittleEndian.Uint32(packet[pos+8:])
		constant := tagged&gpuSolidMCUTag != 0
		length := int(tagged)
		if constant {
			length = 0
		}
		padded := (length + 3) &^ 3
		pos += 12
		if index < 0 || index >= count || (!constant && (length < 1 || length > 3072)) || padded > len(packet)-pos {
			return invalid()
		}
		if (constant && tagged & ^(gpuSolidMCUTag|gpuSolidDCHigh) != 0) || (!constant && descriptor != 0) {
			return invalid()
		}
		if seen[index] {
			return invalid()
		}
		seen[index] = true
		if constant {
			cr := descriptor>>22 | ((tagged>>30)&1)<<10
			dc := [3]int16{int16(int32(descriptor<<21) >> 21), int16(int32((descriptor>>11)<<21) >> 21), int16(int32(cr<<21) >> 21)}
			color, valid := gpuConstantColor(dc, header)
			if !valid {
				return invalid()
			}
			next.segments[index] = nil // Clear any older textured representation.
			next.solidDC[index] = dc
			next.solidColors[index] = gpuSolidFlag | color
			continue
		}
		data := packet[pos : pos+length]
		// The resident entropy pipeline terminates each non-final MCU with RST.
		// Rectangles insert their own correctly numbered restart markers on the CPU.
		if index+1 < count {
			if len(data) < 3 || data[len(data)-2] != 0xff || data[len(data)-1] != byte(0xd0+index%8) {
				return invalid()
			}
			data = data[:len(data)-2]
		}
		next.segments[index] = bytes.Clone(data)
		next.solidColors[index] = 0
		next.solidDC[index] = [3]int16{}
		pos += padded
	}
	for i, segment := range next.segments {
		if len(segment) == 0 && next.solidColors[i]&gpuSolidFlag == 0 {
			return invalid()
		}
	}
	return next, nil
}
