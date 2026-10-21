package jpegcodec

import (
	"image/color"
	"math/bits"
)

// gpuConstantColor converts the retained quantized color descriptor once, while
// accepting a GPU update. The locally generated header has Y/C DC quantizers at
// bytes 7/72. No Huffman decoding or pixel buffer is involved.
func gpuConstantColor(dc [3]int16, header []byte) (uint32, bool) {
	if len(header) <= 72 {
		return 0, false
	}
	q := [3]int{int(header[7]), int(header[72]), int(header[72])}
	var sample [3]uint8
	for c := range sample {
		d := int(dc[c]) * q[c]
		if q[c] == 0 || d < -2048 || d > 2047 {
			return 0, false
		}
		sample[c] = uint8(min(255, max(0, 128+((d+4)>>3))))
	}
	r, g, b := color.YCbCrToRGB(sample[0], sample[1], sample[2])
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b), true
}

// constantMCUEntropy is only the compatibility path for consumers needing a
// JPEG, not the Tight Fill path. All six blocks have zero AC coefficients.
func constantMCUEntropy(dc [3]int16) []byte {
	out := make([]byte, 0, 24)
	var pending uint32
	var count uint
	put := func(value uint32, n uint) {
		pending = pending<<n | value&((1<<n)-1)
		count += n
		for count >= 8 {
			count -= 8
			v := byte(pending >> count)
			out = append(out, v)
			if v == 255 {
				out = append(out, 0)
			}
		}
	}
	for block := 0; block < 6; block++ {
		component, table := 0, 0
		if block >= 4 {
			component, table = block-3, 2
		}
		v := int32(dc[component])
		if block > 0 && block < 4 {
			v = 0 // Y's DC predictor already equals the constant after block 0.
		}
		magnitude := v
		if magnitude < 0 {
			magnitude = -magnitude
		}
		n := uint(bits.Len32(uint32(magnitude)))
		code := jpegCodes[table][n]
		put(code.value, uint(code.size))
		if v < 0 {
			v--
		}
		put(uint32(v), n)
		eob := jpegCodes[table+1][0]
		put(eob.value, uint(eob.size))
	}
	if count > 0 {
		put((1<<(8-count))-1, 8-count)
	}
	return out
}
