package rdp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"

	"anykvm/internal/bridge"
)

func TestProgressiveSRLProtocolExamples(t *testing.T) {
	// Zero run 0 is "10" at initial K=1, followed by positive sign 0.
	// Unary magnitude 7 (nBits=3) has no terminating one.
	for _, example := range []struct {
		values []int32
		bits   int
		want   []byte
	}{
		{[]int32{1}, 3, []byte{0x90, 0}},
		{[]int32{-2}, 3, []byte{0xa8, 0}},
		{[]int32{7}, 3, []byte{0x80, 0, 0}},
		{[]int32{-1}, 1, []byte{0xa0, 0}},
		{nil, 2, []byte{0}},
	} {
		if got := encodeProgressiveSRL(example.values, example.bits); !bytes.Equal(got, example.want) {
			t.Fatalf("SRL %v bits=%d: %x want %x", example.values, example.bits, got, example.want)
		}
	}
}

func TestProgressiveFirstPreservesSolidColors(t *testing.T) {
	const w, h = 64, 64
	rects := []bitmapRect{{0, 0, w, h}}
	tiles := func(stream []byte) []byte {
		for len(stream) >= 6 {
			kind, n := le.Uint16(stream), int(le.Uint32(stream[2:]))
			if kind == 0xccc4 {
				// Region header, one rectangle, base table and progressive table.
				return stream[6+12+8+5+16 : n]
			}
			stream = stream[n:]
		}
		t.Fatal("no FIRST region")
		return nil
	}
	for _, rgb := range [][3]byte{{200, 190, 180}, {90, 110, 120}, {200, 199, 200}, {255, 255, 255}} {
		pixels := bytes.Repeat([]byte{rgb[2], rgb[1], rgb[0], 255}, w*h)
		cache := make([]int32, progressiveTileCoefficients)
		full := tiles(encodeProgressivePass(pixels, w, h, rects, 1, true, 6, progressiveBGRA, 0, false, cache))
		for bits := 1; bits <= 4; bits++ {
			first := tiles(encodeProgressivePass(pixels, w, h, rects, 1, true, 6, progressiveBGRA, bits, false, cache))
			if !bytes.Equal(first, full) {
				t.Fatalf("FIRST changes a solid color before UPGRADE: rgb=%v bits=%d", rgb, bits)
			}
		}
	}
	// LL3 is absent from both UPGRADE streams, regardless of its signed values.
	coeffs := make([]int32, 4096)
	a, b := encodeProgressiveUpgradeComponent(coeffs, 2)
	for i := progressiveReduceLL; i < len(coeffs); i++ {
		coeffs[i] = int32(i*17 - 68000)
	}
	c, d := encodeProgressiveUpgradeComponent(coeffs, 2)
	if !bytes.Equal(a, c) || !bytes.Equal(b, d) {
		t.Fatal("UPGRADE sends already complete low frequencies")
	}
}

func TestProgressiveLayeredNativeEquivalence(t *testing.T) {
	modes := []string{"native-scalar", "native-sse2", "native-avx2"}
	if runtime.GOARCH == "arm64" {
		modes = append(modes, "native")
	}
	for _, mode := range modes {
		for _, quant := range []int{6, 8, 15} {
			for bits := 1; bits <= 4; bits++ {
				t.Run(fmt.Sprintf("%s/q%d/bits%d", mode, quant, bits), func(t *testing.T) {
					native, err := newNativeProgressive(mode, quant)
					if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
						t.Skip(err)
					}
					if err != nil {
						t.Fatal(err)
					}
					e := &progressiveEncoder{native: native, required: true, first: true, quantization: quant}
					defer e.close()
					if err := e.setRefinement(bits); err != nil {
						t.Fatal(err)
					}
					for _, format := range []progressivePixelFormat{progressiveBGRA, progressiveNV12} {
						for _, size := range [][2]int{{1, 1}, {129, 131}, {257, 193}, {64, 64}} {
							w, h := size[0], size[1]
							for _, pattern := range []string{"solid", "gradient", "edge", "noise"} {
								pixels := progressivePixels(w, h, pattern)
								if format == progressiveNV12 {
									pixels = progressiveNV12Pixels(w, h, pattern)
								}
								rects := dirtyProgressiveRectsFormat(pixels, nil, w, h, format)
								cache := make([]int32, ((w+63)/64)*((h+63)/64)*progressiveTileCoefficients)
								for i := 0; i < len(rects); {
									end := progressiveBatchEnd(rects, i)
									batch := rects[i:end]
									initial := e.first || e.w != w || e.h != h
									want := encodeProgressivePass(pixels, w, h, batch, 1, initial, quant, format, bits, false, cache)
									got, err := e.encodeFormat(pixels, w, h, batch, 1, format)
									if err != nil || !bytes.Equal(got, want) {
										t.Fatalf("FIRST %dx%d %s format=%d: %v", w, h, pattern, format, err)
									}
									i = end
								}
								if !reflect.DeepEqual(e.coefficients, cache) {
									t.Fatal("cached base coefficients differ")
								}
								for batch := e.pendingRects(8); len(batch) > 0; batch = e.pendingRects(8) {
									want := encodeProgressivePass(nil, w, h, batch, 2, false, quant, format, bits, true, cache)
									got, err := e.upgrade(batch, 2)
									if err != nil || !bytes.Equal(got, want) {
										t.Fatalf("UPGRADE %dx%d %s: %v", w, h, pattern, err)
									}
									if _, err := e.upgrade(batch, 3); err == nil {
										t.Fatal("repeated upgrade accepted")
									}
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestProgressiveRefinementState(t *testing.T) {
	e := &progressiveEncoder{first: true, quantization: 6}
	defer e.close()
	if err := e.setRefinement(2); err != nil {
		t.Fatal(err)
	}
	pixels := progressivePixels(129, 65, "gradient")
	rects := dirtyProgressiveRects(pixels, nil, 129, 65)
	if _, err := e.encode(pixels, 129, 65, rects, 1); err != nil {
		t.Fatal(err)
	}
	old := append([]int32(nil), e.coefficients...)
	pixels = progressivePixels(129, 65, "noise")
	if _, err := e.encode(pixels, 129, 65, rects[1:2], 2); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(old, e.coefficients) || !reflect.DeepEqual(old[:progressiveTileCoefficients], e.coefficients[:progressiveTileCoefficients]) {
		t.Fatal("replacement FIRST changed unrelated cache")
	}
	if len(e.pendingRects(64)) != 6 {
		t.Fatal("superseded FIRST duplicated pending tile")
	}
	if _, err := e.upgrade([]bitmapRect{rects[0], rects[0]}, 3); err == nil {
		t.Fatal("duplicate upgrade accepted")
	}
	e.discardRefinements()
	if len(e.pendingRects(64)) != 0 {
		t.Fatal("refresh retained pending upgrades")
	}
	if _, err := e.upgrade(rects[:1], 3); err == nil {
		t.Fatal("cancelled upgrade accepted")
	}
	if _, err := e.encode(progressivePixels(1, 1, "solid"), 1, 1, []bitmapRect{{0, 0, 1, 1}}, 4); err != nil {
		t.Fatal(err)
	}
	if len(e.pending) != 1 || len(e.coefficients) != progressiveTileCoefficients || !e.pending[0] {
		t.Fatal("resize retained old tile state")
	}
	e.initRefinement(8192, 8192)
	if len(e.coefficients) != 0 || len(e.pending) != 0 {
		t.Fatal("oversized cache exceeded bound")
	}
	for _, bits := range []int{-1, 5, 100} {
		if validateProgressiveRefinement(bits) == nil {
			t.Fatal("invalid refine accepted")
		}
	}
}

type failedLayeredCodec struct{ failedProgressiveCodec }

func (e *failedLayeredCodec) encodePass([]byte, int, int, []bitmapRect, uint32, bool, progressivePixelFormat, int, bool, []int32) ([]byte, error) {
	return nil, errors.New("injected layered failure")
}

func TestProgressiveLayeredFailureFallback(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		for _, required := range []bool{false, true} {
			e := &progressiveEncoder{first: true, quantization: 8}
			defer e.close()
			if err := e.setRefinement(2); err != nil {
				t.Fatal(err)
			}
			pixels := progressivePixels(64, 64, "noise")
			rects := []bitmapRect{{0, 0, 64, 64}}
			if _, err := e.encode(pixels, 64, 64, rects, 1); err != nil {
				t.Fatal(err)
			}
			cache := append([]int32(nil), e.coefficients...)
			broken := &failedLayeredCodec{}
			e.native = broken
			e.required = required
			var got []byte
			var err error
			if upgrade {
				got, err = e.upgrade(rects, 2)
			} else {
				got, err = e.encode(pixels, 64, 64, rects, 2)
			}
			if required {
				if err == nil || broken.closed || !e.pending[0] {
					t.Fatal("forced native failure lost state")
				}
				continue
			}
			want := encodeProgressivePass(pixels, 64, 64, rects, 2, false, 8, progressiveBGRA, 2, upgrade, cache)
			if err != nil || !bytes.Equal(got, want) || !broken.closed || e.pending[0] == upgrade {
				t.Fatalf("fallback upgrade=%v err=%v", upgrade, err)
			}
		}
	}
}

func TestProgressiveRefinementPriority(t *testing.T) {
	e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer e.close()
	const w, h = 128, 64
	pixels := progressivePixels(w, h, "gradient")
	rects := dirtyProgressiveRects(pixels, nil, w, h)
	if _, err := e.encode(pixels, w, h, rects, 1); err != nil {
		t.Fatal(err)
	}
	// Replacing a changing tile must not introduce an idle-age requirement for
	// either that tile or its unchanged neighbor. Both can refine immediately.
	if _, err := e.encode(progressivePixels(w, h, "noise"), w, h, rects[1:], 2); err != nil {
		t.Fatal(err)
	}
	c := channelTestClient()
	c.frames = make(chan bridge.Frame, 2)
	c.acknowledged = c.sent - 1 // FIRST can still be in flight.
	n, payload, _, _, err := c.sendProgressiveRefinement(e, time.Now())
	if err != nil || n != 2 || payload == 0 || len(e.pendingRects(8)) != 0 {
		t.Fatalf("fresh FIRST was not immediately refined: tiles=%d payload=%d err=%v", n, payload, err)
	}
}

func TestProgressiveRefinementRespectsWindowAndCapturePriority(t *testing.T) {
	e := &progressiveEncoder{w: 64, h: 64, pending: []bool{true}}
	for _, reason := range []string{"full", "queued", "reset", "closed"} {
		c := &client{frames: make(chan bridge.Frame, 2), done: make(chan struct{})}
		switch reason {
		case "full":
			c.sent = 2
		case "queued":
			c.frames <- bridge.Frame{}
		case "reset":
			c.reset.Store(true)
		case "closed":
			close(c.done)
		}
		before := c.sent
		n, payload, _, _, err := c.sendProgressiveRefinement(e, time.Now())
		if err != nil || n != 0 || payload != 0 || c.sent != before || !e.pending[0] {
			t.Fatalf("%s sent an ineligible refinement: %v", reason, err)
		}
	}
}

type captureDuringUpgradeCodec struct {
	failedProgressiveCodec
	queued chan bridge.Frame
}

func (e *captureDuringUpgradeCodec) encodePass(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, bits int, upgrade bool, cache []int32) ([]byte, error) {
	encoded := encodeProgressivePass(pixels, w, h, rects, frame, first, 6, format, bits, upgrade, cache)
	e.queued <- bridge.Frame{}
	return encoded, nil
}

func TestProgressiveRefinementYieldsToNewCapture(t *testing.T) {
	const w, h = 1024, 64
	e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer e.close()
	pixels := progressivePixels(w, h, "gradient")
	rects := dirtyProgressiveRects(pixels, nil, w, h)
	for i := 0; i < len(rects); {
		end := progressiveBatchEnd(rects, i)
		if _, err := e.encode(pixels, w, h, rects[i:end], 1); err != nil {
			t.Fatal(err)
		}
		i = end
	}
	c := channelTestClient()
	c.frames = make(chan bridge.Frame, 2)
	c.acknowledged = c.sent
	e.native = &captureDuringUpgradeCodec{queued: c.frames}
	n, _, _, _, err := c.sendProgressiveRefinement(e, time.Now())
	if err != nil || n != 8 || len(e.pendingRects(64)) != 8 || len(c.frames) != 1 {
		t.Fatalf("new capture did not stop refinement after its current batch: tiles=%d err=%v", n, err)
	}
	c.acknowledged = c.sent
	before := c.sent
	n, _, _, _, err = c.sendProgressiveRefinement(e, time.Now())
	if err != nil || n != 0 || c.sent != before || len(e.pendingRects(64)) != 8 {
		t.Fatal("queued capture did not take priority over remaining refinements")
	}
}

func TestProgressiveRefinementConfiguration(t *testing.T) {
	for _, value := range []int{-1, 0, 1, 2, 4, 5, 256} {
		s, err := NewServer(Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test", Codec: "progressive", ProgressiveRefinement: value, TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}})
		if value < 0 || value > 4 {
			if err == nil {
				t.Fatal("invalid refinement accepted")
			}
			continue
		}
		if err != nil || s.cfg.ProgressiveRefinement != value {
			t.Fatalf("refine=%d: %v", value, err)
		}
	}
}

func TestProgressiveReduceExtrapolateRamp(t *testing.T) {
	for _, size := range []int{64, 33, 17} {
		input := make([]int32, size)
		for i := range input {
			input[i] = int32(i*32) - 1024
		}
		n, lo := size/2, (size+1)/2
		if size == 64 {
			n, lo = 31, 33
		}
		low, high := make([]int32, lo), make([]int32, n)
		progressiveReduceLine(input, low, high)
		for i, v := range high {
			if v != 0 {
				t.Fatalf("ramp high[%d]=%d", i, v)
			}
		}
		for i, v := range low {
			if v != int32(i*64)-1024 {
				t.Fatalf("ramp low[%d]=%d", i, v)
			}
		}
	}
}
