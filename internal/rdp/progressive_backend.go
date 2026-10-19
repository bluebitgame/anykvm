package rdp

import (
	"errors"
	"fmt"
	"log"
	"os"
)

var errProgressiveNativeUnavailable = errors.New("native Progressive encoder unavailable")
var errProgressiveSIMDUnavailable = errors.New("requested Progressive SIMD backend unavailable")

type nativeProgressiveCodec interface {
	encode([]byte, int, int, []bitmapRect, uint32, bool, progressivePixelFormat) ([]byte, error)
	name() string
	close()
}

// Batch by encoded tile count to bound output size and CGO calls per frame.
const progressiveBatchTiles = 8
const progressiveBatchRects = progressiveBatchTiles

func progressiveRectTiles(r bitmapRect) int {
	return ((r.w + 63) / 64) * ((r.h + 63) / 64)
}

func progressiveBatchEnd(rects []bitmapRect, start int) int {
	end, tiles := start, 0
	for end < len(rects) && end-start < progressiveBatchRects {
		n := progressiveRectTiles(rects[end])
		if end > start && tiles+n > progressiveBatchTiles {
			break
		}
		// Always include the first rectangle; encode validates its bounds.
		tiles += n
		end++
	}
	return end
}

// DefaultProgressiveQuantization applies to all bands and components.
// Larger factors discard more coefficient bits.
const DefaultProgressiveQuantization = 6

func normalizeProgressiveQuantization(value int) (int, error) {
	if value == 0 {
		value = DefaultProgressiveQuantization
	}
	if value < DefaultProgressiveQuantization || value > 15 {
		return 0, errors.New("RDP quant must be 6 through 15 (0 uses 6)")
	}
	return value, nil
}

type progressiveEncoder struct {
	native       nativeProgressiveCodec
	required     bool
	first        bool
	w, h         int
	closed       bool
	quantization int
	refineBits   int
	coefficients []int32
	pending      []bool
}

func newProgressiveEncoder(quantization int) (*progressiveEncoder, error) {
	quantization, err := normalizeProgressiveQuantization(quantization)
	if err != nil {
		return nil, err
	}
	e := &progressiveEncoder{first: true, quantization: quantization}
	mode := os.Getenv("ANYKVM_RDP_PROGRESSIVE_BACKEND")
	if mode == "go" {
		return e, nil
	}
	if mode != "" && mode != "auto" && mode != "native" && mode != "native-scalar" && mode != "native-sse2" && mode != "native-avx2" {
		return nil, fmt.Errorf("invalid ANYKVM_RDP_PROGRESSIVE_BACKEND %q", mode)
	}
	e.required = mode == "native" || mode == "native-scalar" || mode == "native-sse2" || mode == "native-avx2"
	e.native, err = newNativeProgressive(mode, quantization)
	if err != nil && e.required {
		return nil, err
	}
	return e, nil
}

func (e *progressiveEncoder) backend() string {
	if e.native != nil {
		return e.native.name()
	}
	return "Go"
}

func (e *progressiveEncoder) close() {
	if e.native != nil {
		e.native.close()
		e.native = nil
	}
	e.closed = true
	e.coefficients, e.pending = nil, nil
}

func (e *progressiveEncoder) encode(pixels []byte, w, h int, rects []bitmapRect, frame uint32) ([]byte, error) {
	return e.encodeFormat(pixels, w, h, rects, frame, progressiveBGRA)
}

func (e *progressiveEncoder) encodeFormat(pixels []byte, w, h int, rects []bitmapRect, frame uint32, format progressivePixelFormat) ([]byte, error) {
	if e.closed {
		return nil, errors.New("Progressive encoder closed")
	}
	if w < 1 || h < 1 || w > 8192 || h > 8192 || len(pixels) != progressivePixelBytes(w, h, format) || len(rects) > progressiveBatchRects {
		return nil, errors.New("invalid Progressive frame")
	}
	tiles := 0
	for _, r := range rects {
		if r.x < 0 || r.y < 0 || r.w < 1 || r.h < 1 || r.x%64 != 0 || r.y%64 != 0 ||
			r.w > 128 || r.h > 128 || r.x+r.w > w || r.y+r.h > h {
			return nil, errors.New("invalid Progressive rectangle")
		}
		tiles += progressiveRectTiles(r)
	}
	if tiles > progressiveBatchTiles {
		return nil, errors.New("Progressive batch exceeds eight tiles")
	}
	if len(rects) == 0 {
		return nil, nil
	}
	if w != e.w || h != e.h {
		e.first, e.w, e.h = true, w, h
		e.initRefinement(w, h)
	}
	if len(e.coefficients) != 0 {
		out, err := e.encodeLayered(pixels, rects, frame, format, false)
		if err == nil {
			for _, r := range rects {
				for y := r.y; y < r.y+r.h; y += 64 {
					for x := r.x; x < r.x+r.w; x += 64 {
						e.pending[progressiveTileIndex(w, x, y)] = true
					}
				}
			}
			e.first = false
		}
		return out, err
	}
	if e.native != nil {
		out, err := e.native.encode(pixels, w, h, rects, frame, e.first, format)
		if err == nil {
			e.first = false
			return out, nil
		}
		if e.required {
			return nil, err
		}
		// Both paths send original SIMPLE tiles with the same quantization and
		// context flags. An unsent failed batch can safely be retried in Go.
		log.Printf("rdp: Progressive native encoder failed, switching to Go: %v", err)
		e.native.close()
		e.native = nil
	}
	out := encodeProgressiveRectsFormat(pixels, w, h, rects, frame, e.first, e.quantization, format)
	e.first = false
	return out, nil
}
