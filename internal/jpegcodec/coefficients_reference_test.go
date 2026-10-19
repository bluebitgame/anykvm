package jpegcodec

import (
	"fmt"
	"math/bits"
)

// Scalar entropy encoder used only as an independent oracle for GPU tests.
type jpegBits struct {
	data  []byte
	value uint32
	count uint32
}

func (w *jpegBits) put(value, count uint32) {
	if count == 0 {
		return
	}
	w.value = w.value<<count | value&((1<<count)-1)
	w.count += count
	for w.count >= 8 {
		w.count -= 8
		b := byte(w.value >> w.count)
		w.data = append(w.data, b)
		if b == 255 {
			w.data = append(w.data, 0)
		}
	}
}

func (w *jpegBits) symbol(table int, symbol byte) {
	c := jpegCodes[table][symbol]
	w.put(c.value, c.size)
}

func jpegMagnitude(value int32) (uint32, uint32) {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	size := uint32(bits.Len32(uint32(abs)))
	if value < 0 {
		value--
	}
	return uint32(value), size
}

// encodeQuantized420 builds a reference JPEG from quantized DCT blocks:
// four Y blocks, Cb, Cr per 16x16 MCU, in raster MCU order.
func encodeQuantized420(coefficients []int32, width, height, quality int) ([]byte, error) {
	return encodeQuantized420Restart(coefficients, width, height, quality, false)
}

// Scalar oracle for checking the GPU scan encoder with standard restart markers.
func encodeQuantized420Restart(coefficients []int32, width, height, quality int, restart bool) ([]byte, error) {
	if width < 1 || height < 1 || width > 8192 || height > 8192 ||
		len(coefficients) != ((width+15)/16)*((height+15)/16)*384 {
		return nil, fmt.Errorf("JPEG: invalid coefficient frame %dx%d (%d)", width, height, len(coefficients))
	}
	w := jpegBits{data: jpegHeader420(width, height, quality, restart)}
	var previous [3]int32
	for offset := 0; offset < len(coefficients); offset += 64 {
		if restart && offset > 0 && offset%384 == 0 {
			if w.count != 0 {
				w.put(0x7f, 8-w.count)
			}
			w.data = append(w.data, 255, byte(0xd0+(offset/384-1)%8))
			previous = [3]int32{}
		}
		block := coefficients[offset : offset+64]
		component, table := 0, 0
		if which := offset / 64 % 6; which >= 4 {
			component, table = which-3, 2
		}
		if block[0] < -1024 || block[0] > 1023 {
			return nil, fmt.Errorf("JPEG: DC coefficient out of range")
		}
		value, size := jpegMagnitude(block[0] - previous[component])
		w.symbol(table, byte(size))
		w.put(value, size)
		previous[component] = block[0]
		run := 0
		for _, natural := range jpegZigzag[1:] {
			v := block[natural]
			if v == 0 {
				run++
				continue
			}
			if v < -1023 || v > 1023 {
				return nil, fmt.Errorf("JPEG: AC coefficient out of range")
			}
			for run >= 16 {
				w.symbol(table+1, 0xf0)
				run -= 16
			}
			value, size = jpegMagnitude(v)
			w.symbol(table+1, byte(run<<4)|byte(size))
			w.put(value, size)
			run = 0
		}
		if run != 0 {
			w.symbol(table+1, 0)
		}
	}
	if w.count != 0 {
		w.put(0x7f, 8-w.count)
	}
	w.data = append(w.data, 255, 217)
	return w.data, nil
}
