package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"testing"
)

type failedProgressiveCodec struct{ closed bool }

func (e *failedProgressiveCodec) encode([]byte, int, int, []bitmapRect, uint32, bool, progressivePixelFormat) ([]byte, error) {
	return nil, errors.New("injected native encoding failure")
}
func (e *failedProgressiveCodec) close()       { e.closed = true }
func (e *failedProgressiveCodec) name() string { return "injected failure" }

func BenchmarkProgressiveQuantization(b *testing.B) {
	const w, h = 1280, 720
	for _, pattern := range []string{"solid", "gradient", "noise"} {
		pixels := progressivePixels(w, h, pattern)
		rects := dirtyProgressiveRects(pixels, nil, w, h)
		for _, factor := range []int{6, 7, 8} {
			b.Run(fmt.Sprintf("%s/q%d", pattern, factor), func(b *testing.B) {
				e, err := newProgressiveEncoder(factor)
				if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
					b.Skip(err)
				}
				if err != nil {
					b.Fatal(err)
				}
				defer e.close()
				b.Logf("encoder=%s quantization=%d", e.backend(), factor)
				encode := func(frame uint32) int64 {
					var encoded int64
					for i := 0; i < len(rects); i += progressiveBatchRects {
						out, err := e.encode(pixels, w, h, rects[i:min(i+progressiveBatchRects, len(rects))], frame)
						if err != nil {
							b.Fatal(err)
						}
						encoded += int64(len(out))
					}
					return encoded
				}
				encode(0)
				b.SetBytes(int64(len(pixels)))
				b.ReportAllocs()
				b.ResetTimer()
				var encoded int64
				for frame := 0; frame < b.N; frame++ {
					encoded += encode(uint32(frame))
				}
				b.ReportMetric(float64(encoded)/float64(b.N), "encoded-B/frame")
			})
		}
	}
}
func TestProgressiveNativeFailure(t *testing.T) {
	pixels := progressivePixels(64, 64, "gradient")
	r := bitmapRect{0, 0, 64, 64}
	for _, first := range []bool{true, false} {
		native := &failedProgressiveCodec{}
		e := &progressiveEncoder{native: native, first: first, w: 64, h: 64, quantization: 8}
		got, err := e.encode(pixels, 64, 64, []bitmapRect{r}, 17)
		if err != nil || !bytes.Equal(got, encodeProgressive(pixels, 64, 64, r, 17, first, 8)) || !native.closed || e.native != nil {
			t.Fatalf("unsafe native fallback: first=%v err=%v", first, err)
		}
		e.close()
	}
	native := &failedProgressiveCodec{}
	e := &progressiveEncoder{native: native, first: true, required: true, quantization: 8}
	if _, err := e.encode(pixels, 64, 64, []bitmapRect{r}, 17); err == nil || e.native == nil || native.closed {
		t.Fatal("required native mode silently switched to Go")
	}
	e.close()
}

func progressivePixels(w, h int, pattern string) []byte {
	pixels := make([]byte, w*h*4)
	rng := rand.New(rand.NewSource(71482))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			switch pattern {
			case "solid":
				pixels[i], pixels[i+1], pixels[i+2] = 13, 79, 201
			case "gradient":
				pixels[i], pixels[i+1], pixels[i+2] = byte(x), byte(y), byte(x+y)
			case "edge":
				v := byte(0)
				if x%9 < 4 || y%13 < 5 {
					v = 255
				}
				pixels[i], pixels[i+1], pixels[i+2] = v, v, v
			case "noise":
				_, _ = rng.Read(pixels[i : i+3])
			}
			pixels[i+3] = 255
		}
	}
	return pixels
}

func TestProgressiveNativeMatchesGo(t *testing.T) {
	for _, mode := range []string{"native-scalar", "native-sse2", "native-avx2"} {
		t.Run(mode, func(t *testing.T) {
			testProgressiveNativeMatchesGo(t, mode)
		})
	}
	if runtime.GOARCH != "amd64" {
		t.Run("native", func(t *testing.T) { testProgressiveNativeMatchesGo(t, "native") })
	}
}

func testProgressiveNativeMatchesGo(t *testing.T, mode string) {
	native, err := newNativeProgressive(mode, DefaultProgressiveQuantization)
	if err != nil {
		if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	e := &progressiveEncoder{native: native, required: true, first: true, quantization: DefaultProgressiveQuantization}
	defer e.close()
	t.Log(e.backend())
	wantBackend := "native C scalar"
	switch mode {
	case "native-sse2":
		wantBackend = "native C SSE2"
	case "native-avx2":
		wantBackend = "native C AVX2"
	case "native":
		switch runtime.GOARCH {
		case "arm64":
			wantBackend = "native C NEON"
		}
	}
	if e.backend() != wantBackend {
		t.Fatalf("backend=%q, want %q", e.backend(), wantBackend)
	}
	var retained, saved []byte
	for _, size := range [][2]int{{1, 1}, {2, 3}, {3, 2}, {4, 5}, {5, 4}, {7, 9}, {8, 7}, {9, 8}, {33, 53}, {64, 64}, {129, 131}, {257, 193}, {1280, 720}, {1, 1}} {
		w, h := size[0], size[1]
		for _, pattern := range []string{"solid", "gradient", "edge", "noise"} {
			pixels := progressivePixels(w, h, pattern)
			// The pixel slice can start at any byte alignment. Row strides and
			// partial tiles also exercise vector tails without reading past it.
			unaligned := make([]byte, len(pixels)+1)
			copy(unaligned[1:], pixels)
			pixels = unaligned[1:]
			rects := dirtyProgressiveRects(pixels, nil, w, h)
			for _, frame := range []uint32{1, ^uint32(0)} {
				for i := 0; i < len(rects); i += progressiveBatchRects {
					batch := rects[i:min(i+progressiveBatchRects, len(rects))]
					first := e.first || e.w != w || e.h != h
					want := encodeProgressiveRects(pixels, w, h, batch, frame, first, DefaultProgressiveQuantization)
					got, err := e.encode(pixels, w, h, batch, frame)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						offset := 0
						for offset < min(len(got), len(want)) && got[offset] == want[offset] {
							offset++
						}
						t.Fatalf("%dx%d %s frame=%d batch=%d: difference at byte %d; native=%d Go=%d bytes", w, h, pattern, frame, i, offset, len(got), len(want))
					}
					if retained != nil && !bytes.Equal(retained, saved) {
						t.Fatal("native encode mutated a previously returned output")
					}
					retained, saved = got, append(saved[:0], got...)
				}
			}
		}
	}
}

func TestProgressiveBackendLifecycle(t *testing.T) {
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
	e, err := newProgressiveEncoder(0)
	if err != nil || e.backend() != "Go" {
		t.Fatalf("Go selection failed: %v", err)
	}
	pixels := progressivePixels(128, 128, "solid")
	for _, rects := range [][]bitmapRect{{{-1, 0, 64, 64}}, {{1, 0, 64, 64}}, {{0, 0, 129, 128}}, {{0, 0, 128, 129}}, {{0, 0, 0, 64}}} {
		if _, err := e.encode(pixels, 128, 128, rects, 1); err == nil {
			t.Fatal("invalid region accepted")
		}
	}
	if _, err := e.encode(nil, 128, 128, []bitmapRect{{0, 0, 128, 128}}, 1); err == nil {
		t.Fatal("invalid pixel buffer accepted")
	}
	if _, err := e.encode(pixels, 128, 128, []bitmapRect{{0, 0, 128, 128}}, 1); err != nil {
		t.Fatal(err)
	}
	e.close()
	e.close()
	if _, err := e.encode(pixels, 128, 128, nil, 1); err == nil {
		t.Fatal("closed encoder accepted a frame")
	}
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "unknown")
	if _, err := newProgressiveEncoder(0); err == nil {
		t.Fatal("invalid backend selection accepted")
	}
	for _, mode := range []string{"native", "native-scalar", "native-sse2", "native-avx2", "auto"} {
		t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", mode)
		e, err := newProgressiveEncoder(0)
		if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode == "native-scalar" && e.backend() != "native C scalar" {
			t.Fatalf("scalar selection failed: %s", e.backend())
		}
		if mode == "native" || mode == "auto" {
			// Check automatic dispatch against forced AVX2
			// availability, then exercise the selected entry point as well.
			if avx2, err := newNativeProgressive("native-avx2", DefaultProgressiveQuantization); err == nil {
				avx2.close()
				if e.backend() != "native C AVX2" {
					t.Fatalf("auto missed available AVX2: %s", e.backend())
				}
			}
			out, err := e.encode(pixels, 128, 128, []bitmapRect{{0, 0, 128, 128}}, 1)
			if err != nil || !bytes.Equal(out, encodeProgressive(pixels, 128, 128, bitmapRect{0, 0, 128, 128}, 1, true, DefaultProgressiveQuantization)) {
				t.Fatalf("auto encoding mismatch: %v", err)
			}
			t.Logf("%s selected %s", mode, e.backend())
		}
		e.close()
	}
}

func BenchmarkProgressiveBackends(b *testing.B) {
	const w, h = 1280, 720
	for _, pattern := range []string{"solid", "gradient", "noise"} {
		pixels := progressivePixels(w, h, pattern)
		rects := dirtyProgressiveRects(pixels, nil, w, h)
		backends := []string{"go", "native-scalar", "native-sse2", "native-avx2"}
		if runtime.GOARCH != "amd64" {
			backends = []string{"go", "native-scalar", "native"}
		}
		for _, backend := range backends {
			b.Run(pattern+"/"+backend, func(b *testing.B) {
				e := &progressiveEncoder{first: true, required: true, quantization: DefaultProgressiveQuantization}
				if backend != "go" {
					var err error
					if e.native, err = newNativeProgressive(backend, DefaultProgressiveQuantization); err != nil {
						if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
							b.Skip(err)
						}
						b.Fatal(err)
					}
				}
				defer e.close()
				encode := func(frame uint32) int64 {
					var encoded int64
					for i := 0; i < len(rects); i += progressiveBatchRects {
						out, err := e.encode(pixels, w, h, rects[i:min(i+progressiveBatchRects, len(rects))], frame)
						if err != nil {
							b.Fatal(err)
						}
						encoded += int64(len(out))
					}
					return encoded
				}
				encode(0) // Allocate persistent C scratch/output before timing.
				b.SetBytes(int64(len(pixels)))
				b.ReportAllocs()
				b.ResetTimer()
				var encoded int64
				for frame := 0; frame < b.N; frame++ {
					encoded += encode(uint32(frame))
				}
				b.ReportMetric(float64(encoded)/float64(b.N), "encoded-B/frame")
			})
		}
	}
}
