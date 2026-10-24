package rdp

import (
	"errors"
	"time"

	"anykvm/internal/logging"
)

// MS-RDPEGFX 3.1.8.1.3 and 3.2.8.1.5: FIRST 50%, UPGRADE 75%, UPGRADE 100%.
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
	e.stages = nil
	e.refineCursor = 0
	n := ((w + 63) / 64) * ((h + 63) / 64)
	if e.refineBits == 0 {
		return
	}
	if n > progressiveRefineCacheBytes/(4*progressiveTileCoefficients) {
		logging.Infof("rdp: Progressive %dx%d exceeds refinement cache limit; using SIMPLE", w, h)
		return
	}
	// GPU backends own the coefficient cache; only scheduling bits live here.
	if _, resident := e.native.(interface{ residentCoefficients() }); !resident {
		e.coefficients = make([]int32, n*progressiveTileCoefficients)
	}
	e.pending = make([]bool, n)
	e.stages = make([]uint8, n)
}

func progressiveTileIndex(w, x, y int) int { return (y/64)*((w+63)/64) + x/64 }

func (e *progressiveEncoder) discardRefinements() {
	clear(e.pending)
	clear(e.stages)
	e.refineCursor = 0
}

func (e *progressiveEncoder) pendingRects(limit int) []bitmapRect {
	return e.pendingRectsExcept(limit, nil)
}

func (e *progressiveEncoder) hasRefinements() bool {
	if e != nil {
		for _, pending := range e.pending {
			if pending {
				return true
			}
		}
	}
	return false
}

// Rotate between tiles, preserving FIRST -> 75% -> 100% per tile rather than
// globally preferring all 75% passes. Repeated FIRSTs in a changing tile must
// not starve an unchanged neighbor's final pass. Each packet has one quality.
func (e *progressiveEncoder) pendingRectsExcept(limit int, blocked []bool) []bitmapRect {
	var rects []bitmapRect
	if limit <= 0 || len(e.pending) == 0 {
		return nil
	}
	columns := (e.w + 63) / 64
	stage := uint8(0)
	for offset := range e.pending {
		i := (e.refineCursor + offset) % len(e.pending)
		if !e.pending[i] || len(blocked) > i && blocked[i] {
			continue
		}
		if stage == 0 {
			stage = e.tileStage(i)
		}
		if e.tileStage(i) != stage {
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
	bits := progressiveFirst50
	if upgrade {
		bits += int(e.tileStage(progressiveTileIndex(e.w, rects[0].x, rects[0].y)))
	}
	if native, ok := e.native.(nativeProgressiveLayered); ok {
		out, err := native.encodePass(pixels, e.w, e.h, rects, frame, e.first, format, bits, upgrade, e.coefficients)
		if err == nil {
			return out, nil
		}
		if e.required {
			return nil, err
		}
		// Cache contains base coefficients in exactly the same order and scale
		// as Go. A failed unsent FIRST can be recomputed; UPGRADE is read-only.
		logging.Infof("rdp: Progressive layered native encoder failed, switching to Go: %v", err)
		e.native.close()
		e.native = nil
	}
	return encodeProgressivePass(pixels, e.w, e.h, rects, frame, e.first, e.quantization, format, bits, upgrade, e.coefficients), nil
}

func (e *progressiveEncoder) tileStage(i int) uint8 {
	if len(e.stages) == len(e.pending) && e.stages[i] != 0 {
		return e.stages[i]
	}
	return 1
}

// Source copies need final pixels before their scratch snapshot. Complete both
// remaining stages, retaining separate progressive packets and sign histories.
func (e *progressiveEncoder) finishUpgrades(rects []bitmapRect, frame uint32) ([][]byte, error) {
	var out [][]byte
	for stage := uint8(1); stage <= 2; stage++ {
		var batch []bitmapRect
		for _, r := range rects {
			i := progressiveTileIndex(e.w, r.x, r.y)
			if e.pending[i] && e.tileStage(i) == stage {
				batch = append(batch, r)
			}
		}
		if len(batch) > 0 {
			packet, err := e.upgrade(batch, frame)
			if err != nil {
				return nil, err
			}
			out = append(out, packet)
		}
	}
	return out, nil
}

func (e *progressiveEncoder) upgrade(rects []bitmapRect, frame uint32) ([]byte, error) {
	out, err := e.prepareUpgrade(rects, frame)
	if err == nil {
		e.commitUpgrade(rects)
	}
	return out, err
}

// Encoding an UPGRADE only reads the full coefficient cache, on both CPU and
// GPU. Background work can discard this packet without advancing the client's
// precision/sign history; commit only after its transport write succeeds.
func (e *progressiveEncoder) prepareUpgrade(rects []bitmapRect, frame uint32) ([]byte, error) {
	if e.closed || len(e.pending) == 0 || len(rects) == 0 || len(rects) > progressiveBatchRects {
		return nil, errors.New("invalid Progressive upgrade batch")
	}
	for i, r := range rects {
		if r.x < 0 || r.y < 0 || r.x%64 != 0 || r.y%64 != 0 || r.x >= e.w || r.y >= e.h ||
			r.w != min(64, e.w-r.x) || r.h != min(64, e.h-r.y) ||
			!e.pending[progressiveTileIndex(e.w, r.x, r.y)] {
			return nil, errors.New("Progressive upgrade requires a pending FIRST tile")
		}
		if e.tileStage(progressiveTileIndex(e.w, r.x, r.y)) != e.tileStage(progressiveTileIndex(e.w, rects[0].x, rects[0].y)) {
			return nil, errors.New("mixed Progressive upgrade stages")
		}
		for _, prior := range rects[:i] {
			if prior.x == r.x && prior.y == r.y {
				return nil, errors.New("duplicate Progressive upgrade tile")
			}
		}
	}
	started := time.Now()
	out, err := e.encodeLayered(nil, rects, frame, progressiveBGRA, true)
	if err == nil {
		cost := max(time.Nanosecond, time.Since(started)/time.Duration(len(rects)))
		e.upgradeCostAt = time.Now()
		if e.upgradeTileCost == 0 || cost > e.upgradeTileCost {
			e.upgradeTileCost = cost
		} else {
			e.upgradeTileCost += (cost - e.upgradeTileCost) / 4
		}
	}
	return out, err
}

func (e *progressiveEncoder) commitUpgrade(rects []bitmapRect) {
	for _, r := range rects {
		i := progressiveTileIndex(e.w, r.x, r.y)
		stage := e.tileStage(i)
		if len(e.stages) != len(e.pending) {
			e.stages = make([]uint8, len(e.pending))
		}
		e.pending[i] = stage == 1
		e.stages[i] = 0
		if stage == 1 {
			e.stages[i] = 2
		}
		e.refineCursor = (i + 1) % len(e.pending)
	}
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
						if progressiveOfficialPass(bits) {
							components[2*c], components[2*c+1] = encodeProgressiveQualityUpgrade(base[c*4096:][:4096], bits, c)
						} else {
							components[2*c], components[2*c+1] = encodeProgressiveUpgradeComponent(base[c*4096:][:4096], bits)
						}
					}
					quality := byte(255)
					if progressiveOfficialPass(bits) {
						quality = 0
					}
					body := join([]byte{0, 0, 0}, u16(x/64), u16(y/64), []byte{quality})
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
						// FIRST uses the stage's per-band precision; retain full
						// coefficients in the cache for the two UPGRADE passes.
						for i := 0; i < len(coeffs); i++ {
							coeffs[i] = progressiveCoarse(coeffs[i], bits, c, i)
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
