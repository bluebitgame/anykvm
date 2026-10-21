package rdp

import (
	"errors"
	"log"
)

// MS-RDPEGFX 3.1.8.1.3 and 3.2.8.1.5: one coarse original FIRST,
// followed by one UPGRADE to the existing base quantization (quality 255).
// No temporal differences are used. Keep un-differenced quantized coefficients
// so refinements need neither source pixels nor another color conversion/DWT.
const progressiveTileCoefficients = 3 * 4096
const progressiveRefineCacheBytes = 128 << 20

type nativeProgressiveLayered interface {
	encodePass([]byte, int, int, []bitmapRect, uint32, bool, progressivePixelFormat, int, bool, []int32) ([]byte, error)
}

func validateProgressiveRefinement(bits int) error {
	if bits < 0 || bits > 4 {
		return errors.New("RDP refine must be 0 through 4 (0 disables FIRST/UPGRADE)")
	}
	return nil
}

func (e *progressiveEncoder) setRefinement(bits int) error {
	if err := validateProgressiveRefinement(bits); err != nil {
		return err
	}
	if e.w != 0 || e.closed {
		return errors.New("configure Progressive refinement before encoding")
	}
	e.refineBits = bits
	return nil
}

func (e *progressiveEncoder) initRefinement(w, h int) {
	e.coefficients, e.pending = nil, nil
	n := ((w + 63) / 64) * ((h + 63) / 64)
	if e.refineBits == 0 {
		return
	}
	if n > progressiveRefineCacheBytes/(4*progressiveTileCoefficients) {
		log.Printf("rdp: Progressive %dx%d exceeds refinement cache limit; using SIMPLE", w, h)
		return
	}
	// GPU backends own the coefficient cache; only scheduling bits live here.
	if _, resident := e.native.(interface{ residentCoefficients() }); !resident {
		e.coefficients = make([]int32, n*progressiveTileCoefficients)
	}
	e.pending = make([]bool, n)
}

func progressiveTileIndex(w, x, y int) int { return (y/64)*((w+63)/64) + x/64 }

func (e *progressiveEncoder) discardRefinements() { clear(e.pending) }

func (e *progressiveEncoder) pendingRects(limit int) []bitmapRect {
	var rects []bitmapRect
	columns := (e.w + 63) / 64
	for i, pending := range e.pending {
		if !pending {
			continue
		}
		x, y := (i%columns)*64, (i/columns)*64
		rects = append(rects, bitmapRect{x, y, min(64, e.w-x), min(64, e.h-y)})
		if len(rects) == limit {
			break
		}
	}
	return rects
}

func (e *progressiveEncoder) encodeLayered(pixels []byte, rects []bitmapRect, frame uint32, format progressivePixelFormat, upgrade bool) ([]byte, error) {
	if native, ok := e.native.(nativeProgressiveLayered); ok {
		out, err := native.encodePass(pixels, e.w, e.h, rects, frame, e.first, format, e.refineBits, upgrade, e.coefficients)
		if err == nil {
			return out, nil
		}
		if e.required {
			return nil, err
		}
		// Cache contains base coefficients in exactly the same order and scale
		// as Go. A failed unsent FIRST can be recomputed; UPGRADE is read-only.
		log.Printf("rdp: Progressive layered native encoder failed, switching to Go: %v", err)
		e.native.close()
		e.native = nil
	}
	return encodeProgressivePass(pixels, e.w, e.h, rects, frame, e.first, e.quantization, format, e.refineBits, upgrade, e.coefficients), nil
}

func (e *progressiveEncoder) upgrade(rects []bitmapRect, frame uint32) ([]byte, error) {
	if e.closed || len(e.pending) == 0 || len(rects) == 0 || len(rects) > progressiveBatchRects {
		return nil, errors.New("invalid Progressive upgrade batch")
	}
	for i, r := range rects {
		if r.x < 0 || r.y < 0 || r.x%64 != 0 || r.y%64 != 0 || r.x >= e.w || r.y >= e.h ||
			r.w != min(64, e.w-r.x) || r.h != min(64, e.h-r.y) ||
			!e.pending[progressiveTileIndex(e.w, r.x, r.y)] {
			return nil, errors.New("Progressive upgrade requires a pending FIRST tile")
		}
		for _, prior := range rects[:i] {
			if prior.x == r.x && prior.y == r.y {
				return nil, errors.New("duplicate Progressive upgrade tile")
			}
		}
	}
	out, err := e.encodeLayered(nil, rects, frame, progressiveBGRA, true)
	if err == nil {
		for _, r := range rects {
			e.pending[progressiveTileIndex(e.w, r.x, r.y)] = false
		}
	}
	return out, err
}

func encodeProgressivePass(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, quantization int, format progressivePixelFormat, bits int, upgrade bool, cache []int32) []byte {
	var tiles []byte
	count := 0
	for _, r := range rects {
		for y := r.y; y < r.y+r.h; y += 64 {
			for x := r.x; x < r.x+r.w; x += 64 {
				base := cache[progressiveTileIndex(w, x, y)*progressiveTileCoefficients:][:progressiveTileCoefficients]
				var components [6][]byte
				if upgrade {
					for c := 0; c < 3; c++ {
						components[2*c], components[2*c+1] = encodeProgressiveUpgradeComponent(base[c*4096:][:4096], bits)
					}
					body := join([]byte{0, 0, 0}, u16(x/64), u16(y/64), []byte{255})
					for _, c := range components {
						body = append(body, u16(len(c))...)
					}
					for _, c := range components {
						body = append(body, c...)
					}
					tiles = append(tiles, progressiveBlock(0xccc7, body)...)
				} else {
					var planes [3][4096]int32
					progressiveICT(&planes, pixels, w, h, x, y, format)
					for c := 0; c < 3; c++ {
						coeffs := progressiveReduceDWT(planes[c][:])
						quantizeProgressive(coeffs, quantization)
						copy(base[c*4096:], coeffs)
						// Preserve LL3 in FIRST: floor rounding of signed chroma DC
						// shifts the whole coarse tile's hue (often toward green).
						for i := 0; i < progressiveReduceLL; i++ {
							coeffs[i] /= int32(1 << bits)
						}
						for i := 4095; i > progressiveReduceLL; i-- {
							coeffs[i] -= coeffs[i-1]
						}
						components[c] = encodeRLGR1(coeffs)
					}
					body := join([]byte{0, 0, 0}, u16(x/64), u16(y/64), []byte{0, 0},
						u16(len(components[0])), u16(len(components[1])), u16(len(components[2])), u16(0),
						components[0], components[1], components[2])
					tiles = append(tiles, progressiveBlock(0xccc6, body)...)
				}
				count++
			}
		}
	}
	return progressiveLayeredWire(rects, frame, first, quantization, bits, upgrade, tiles, count)
}

func progressiveLayeredWire(rects []bitmapRect, frame uint32, first bool, quantization, bits int, upgrade bool, tiles []byte, count int) []byte {
	return progressiveWire(rects, frame, first, quantization, bits, upgrade, tiles, count, true)
}

func encodeProgressiveUpgradeComponent(coeffs []int32, bits int) ([]byte, []byte) {
	var srlValues [progressiveReduceLL]int32
	n := 0
	raw := rfxBitWriter{}
	mask := int32(1<<bits) - 1
	// LL3's BitPos does not change; the decoder skips that band entirely.
	for _, v := range coeffs[:progressiveReduceLL] {
		magnitude := v
		if magnitude < 0 {
			magnitude = -magnitude
		}
		if magnitude > mask {
			raw.bits(uint32(magnitude&mask), bits)
		} else {
			srlValues[n] = v
			n++
		}
	}
	return encodeProgressiveSRL(srlValues[:n], bits), raw.data
}

// MS-RDPEGFX 3.1.8.1.5.1/.2. Unlike RLGR, SRL remains in run mode
// even at K=0, and the maximum unary magnitude has no terminating one.
func encodeProgressiveSRL(values []int32, bits int) []byte {
	w := rfxBitWriter{}
	kp, maximum := 8, int32(1<<bits)-1
	for i := 0; i < len(values); {
		run := 0
		for i < len(values) && values[i] == 0 {
			run++
			i++
		}
		k := kp >> 3
		for run >= 1<<k {
			w.bits(0, 1)
			run -= 1 << k
			kp = min(80, kp+4)
			k = kp >> 3
		}
		w.bits(1, 1)
		w.bits(uint32(run), k)
		kp = max(0, kp-6)
		if i == len(values) {
			break
		}
		v := values[i]
		if v < 0 {
			w.bits(1, 1)
			v = -v
		} else {
			w.bits(0, 1)
		}
		for j := int32(1); j < v; j++ {
			w.bits(0, 1)
		}
		if v < maximum {
			w.bits(1, 1)
		}
		i++
	}
	return append(w.data, 0) // Required SRL lookahead byte, also for an empty stream.
}
