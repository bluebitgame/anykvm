package rdp

// RemoteFX Progressive's complete-tile path, independently implemented from
// MS-RDPEGFX 2.2.4.2 / 3.2.8.1 and MS-RDPRFX 3.1.8.1. Each SIMPLE tile is
// original (no coefficient differences), with a configurable uniform quantization
// factor (default 6). FIRST/UPGRADE and reduce-extrapolate DWT are not emitted yet.
// The codec is opt-in until interoperability and capture workload measurements
// justify changing the default fallback.

func progressiveBlock(kind int, body []byte) []byte {
	return join(u16(kind), u32(uint32(6+len(body))), body)
}

func encodeProgressive(pixels []byte, w, h int, r bitmapRect, frame uint32, first bool, quantization int) []byte {
	return encodeProgressiveRects(pixels, w, h, []bitmapRect{r}, frame, first, quantization)
}

// Each WIRETOSURFACE_2 update carries one frame and one region. Multiple
// dirty rectangles share its clipping list and tiles, not separate frames.
func encodeProgressiveRects(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, quantization int) []byte {
	return encodeProgressiveRectsFormat(pixels, w, h, rects, frame, first, quantization, progressiveBGRA)
}

func encodeProgressiveRectsFormat(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, quantization int, format progressivePixelFormat) []byte {
	var stream, tiles []byte
	if first {
		stream = append(stream, progressiveBlock(0xccc0, join(u32(0xcaccacca), u16(0x100)))...)
		stream = append(stream, progressiveBlock(0xccc3, join([]byte{0}, u16(64), []byte{0}))...)
	}
	stream = append(stream, progressiveBlock(0xccc1, join(u32(frame), u16(1)))...)
	count := 0
	var rectangles []byte
	for _, r := range rects {
		rectangles = append(rectangles, join(u16(r.x), u16(r.y), u16(r.w), u16(r.h))...)
		for y := r.y; y < r.y+r.h; y += 64 {
			for x := r.x; x < r.x+r.w; x += 64 {
				var planes [3][4096]int32
				progressiveICT(&planes, pixels, w, h, x, y, format)
				var components [3][]byte
				for component := range planes {
					coeffs := progressiveDWT(planes[component][:])
					quantizeProgressive(coeffs, quantization)
					// The lowest-frequency band is differential coded in scan order.
					for i := 4095; i > 4032; i-- {
						coeffs[i] -= coeffs[i-1]
					}
					components[component] = encodeRLGR1(coeffs)
				}
				body := join([]byte{0, 0, 0}, u16(x/64), u16(y/64), []byte{0},
					u16(len(components[0])), u16(len(components[1])), u16(len(components[2])), u16(0),
					components[0], components[1], components[2])
				tiles = append(tiles, progressiveBlock(0xccc5, body)...)
				count++
			}
		}
	}
	q := byte(quantization | quantization<<4)
	region := join([]byte{64}, u16(len(rects)), []byte{1, 0, 0}, u16(count), u32(uint32(len(tiles))),
		rectangles, []byte{q, q, q, q, q}, tiles)
	stream = append(stream, progressiveBlock(0xccc4, region)...)
	return append(stream, progressiveBlock(0xccc2, nil)...)
}

func quantizeProgressive(coeffs []int32, quantization int) {
	// Factor 6 removes the five fractional bits of the 11.5 fixed-point
	// transform. Each additional factor doubles the quantization step.
	bits := uint(quantization - 1)
	round := int32(1 << (bits - 1))
	for i := range coeffs {
		coeffs[i] = (coeffs[i] + round) >> bits
	}
}

// Transform columns then rows, recursively decomposing LL. Each level is
// linearized as HL, LH, HH, followed by the next level (or the final LL).
func progressiveDWT(input []int32) []int32 {
	out := make([]int32, 0, 4096)
	for size := 64; size >= 16; size /= 2 {
		n := size / 2
		vertical, bands := make([]int32, size*size), make([]int32, size*size)
		var column [64]int32
		var lo, hi [32]int32
		for x := 0; x < size; x++ {
			for y := 0; y < size; y++ {
				column[y] = input[y*size+x]
			}
			low, high := lo[:n], hi[:n]
			progressiveDWTLine(column[:size], low, high)
			for y := 0; y < n; y++ {
				vertical[y*size+x], vertical[(n+y)*size+x] = low[y], high[y]
			}
		}
		for y := 0; y < size; y++ {
			low, high := lo[:n], hi[:n]
			progressiveDWTLine(vertical[y*size:(y+1)*size], low, high)
			lowBand, highBand := 3, 0 // LL, HL
			if y >= n {
				lowBand, highBand = 1, 2 // LH, HH
			}
			copy(bands[lowBand*n*n+(y%n)*n:], low)
			copy(bands[highBand*n*n+(y%n)*n:], high)
		}
		out = append(out, bands[:3*n*n]...)
		input = bands[3*n*n:]
	}
	return append(out, input...)
}

func progressiveDWTLine(input, low, high []int32) {
	for i := range high {
		right := input[min(2*i+2, len(input)-2)]
		high[i] = (2*input[2*i+1] - input[2*i] - right + 2) >> 2
	}
	for i := range low {
		low[i] = input[2*i] + ((high[max(0, i-1)] + high[i] + 1) >> 1)
	}
}

type rfxBitWriter struct {
	data []byte
	used uint
}

func (w *rfxBitWriter) bits(value uint32, count int) {
	for i := count - 1; i >= 0; i-- {
		if w.used == 0 {
			w.data = append(w.data, 0)
		}
		w.data[len(w.data)-1] |= byte((value>>i)&1) << (7 - w.used)
		w.used = (w.used + 1) & 7
	}
}

// RLGR1 switches between a zero-run mode and a signed Golomb-Rice mode.
// Parameters use three fractional bits and are independently adapted after
// each emitted coefficient; the bitstream is MSB first.
func encodeRLGR1(coeffs []int32) []byte {
	w := rfxBitWriter{}
	kp, krp := 8, 8
	gr := func(value uint32) {
		kr := krp >> 3
		quotient := int(value >> kr)
		for i := 0; i < quotient; i++ {
			w.bits(1, 1)
		}
		w.bits(0, 1)
		w.bits(value, kr)
		if quotient == 0 {
			krp = max(0, krp-2)
		} else if quotient > 1 {
			krp = min(80, krp+quotient)
		}
	}
	for i := 0; i < len(coeffs); {
		k := kp >> 3
		if k > 0 {
			run := 0
			for i < len(coeffs) && coeffs[i] == 0 {
				run++
				i++
			}
			for run >= 1<<k {
				w.bits(0, 1)
				run -= 1 << k
				kp = min(80, kp+4)
				k = kp >> 3
			}
			w.bits(1, 1)
			w.bits(uint32(run), k)
			if i == len(coeffs) {
				break
			}
			value := coeffs[i]
			if value < 0 {
				w.bits(1, 1)
				value = -value
			} else {
				w.bits(0, 1)
			}
			gr(uint32(value - 1))
			kp = max(0, kp-6)
		} else {
			value := coeffs[i]
			code := uint32(value * 2)
			if value < 0 {
				code = uint32(-value*2 - 1)
			}
			gr(code)
			if code == 0 {
				kp = min(80, kp+3)
			} else {
				kp = max(0, kp-3)
			}
		}
		i++
	}
	return w.data
}

func graphicsProgressive(data []byte) []byte {
	return gfxPDU(2, join(u16(1), u16(int(codecProgressive)), u32(1), []byte{0x20}, u32(uint32(len(data))), data))
}
