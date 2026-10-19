package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestProgressiveBatchTileLimit(t *testing.T) {
	const w, h = 385, 257
	pixels := progressivePixels(w, h, "gradient")
	all := dirtyProgressiveRects(pixels, nil, w, h)
	for _, backend := range []string{"go", "native-scalar", "native-sse2", "native-avx2", "native"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", backend)
			e, err := newProgressiveEncoder(0)
			if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer e.close()
			for _, rects := range [][]bitmapRect{
				all[:9],
				{{0, 0, 128, 128}, {128, 0, 128, 128}, {256, 0, 128, 128}},
			} {
				if _, err := e.encode(pixels, w, h, rects, 1); err == nil {
					t.Fatal("accepted a batch exceeding eight tiles")
				}
			}
			// Both eight individual tiles and two larger legacy rectangles fit.
			for _, rects := range [][]bitmapRect{
				all[:8], {{0, 0, 128, 128}, {128, 0, 128, 128}},
			} {
				first := e.first
				out, err := e.encode(pixels, w, h, rects, 1)
				if err != nil || !bytes.Equal(out, encodeProgressiveRects(pixels, w, h, rects, 1, first, 6)) {
					t.Fatalf("valid eight-tile batch failed: %v", err)
				}
			}
		})
	}
}

func TestProgressiveBatchEnd(t *testing.T) {
	const w, h = 1280, 720
	pixels := progressivePixels(w, h, "solid")
	for _, size := range []int{128, 64} {
		rects := dirtyBitmapRectsSized(pixels, nil, w, h, size)
		batches, tiles := 0, 0
		for i := 0; i < len(rects); {
			end := progressiveBatchEnd(rects, i)
			if end <= i || end-i > progressiveBatchRects {
				t.Fatal("invalid batch rectangle count")
			}
			n := 0
			for _, r := range rects[i:end] {
				n += progressiveRectTiles(r)
			}
			if n > progressiveBatchTiles {
				t.Fatal("batch exceeded encoded tile budget")
			}
			tiles += n
			batches++
			i = end
		}
		if tiles != 240 || batches != 30 {
			t.Fatalf("%dpx full-frame detection: %d tiles / %d batches", size, tiles, batches)
		}
	}
	mixed := []bitmapRect{{0, 0, 128, 128}, {128, 0, 64, 64}, {192, 0, 64, 128}, {256, 0, 64, 64},
		{0, 128, 128, 128}, {128, 128, 128, 128}}
	if progressiveBatchEnd(mixed, 0) != 4 || progressiveBatchEnd(mixed, 4) != 6 {
		t.Fatal("mixed rectangle sizes were batched by rectangle count instead of tiles")
	}
}

// Compare the old 128px detector with the production 64px detector, including
// dirty scanning, CGO calls, and output copies. Transport/capture are excluded.
func BenchmarkProgressiveDirtyTiles(b *testing.B) {
	const w, h = 1280, 720
	for _, workload := range []string{"cursor", "sparse", "full"} {
		previous := progressivePixels(w, h, "gradient")
		pixels := append([]byte(nil), previous...)
		patch := func(x, y int) {
			for yy := y; yy < y+16; yy++ {
				for xx := x; xx < x+16; xx++ {
					i := (yy*w + xx) * 4
					pixels[i], pixels[i+1], pixels[i+2] = 255, 255, 255
				}
			}
		}
		switch workload {
		case "cursor":
			patch(17, 19)
		case "sparse":
			for i := 0; i < 8; i++ {
				patch(i*128+17, (i%4)*128+19)
			}
		case "full":
			previous = nil
		}
		for _, size := range []int{128, 64} {
			b.Run(fmt.Sprintf("%s/%dpx", workload, size), func(b *testing.B) {
				e, err := newProgressiveEncoder(0)
				if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
					b.Skip(err)
				}
				if err != nil {
					b.Fatal(err)
				}
				defer e.close()
				b.Logf("encoder=%s quantization=6", e.backend())
				encode := func(frame uint32) (encoded, tiles, batches int) {
					var rects []bitmapRect
					if size == 64 {
						rects = dirtyProgressiveRects(pixels, previous, w, h)
					} else {
						rects = dirtyBitmapRectsSized(pixels, previous, w, h, size)
					}
					for i := 0; i < len(rects); {
						end := progressiveBatchEnd(rects, i)
						out, err := e.encode(pixels, w, h, rects[i:end], frame)
						if err != nil {
							b.Fatal(err)
						}
						encoded += len(out)
						for _, r := range rects[i:end] {
							tiles += progressiveRectTiles(r)
						}
						batches++
						i = end
					}
					return
				}
				encode(0) // Warm up persistent scratch/output; omit context headers.
				b.ReportAllocs()
				b.ResetTimer()
				var encoded, tiles, batches int
				for frame := 0; frame < b.N; frame++ {
					encoded, tiles, batches = encode(uint32(frame))
				}
				b.ReportMetric(float64(encoded), "encoded-B/frame")
				b.ReportMetric(float64(tiles), "tiles/frame")
				b.ReportMetric(float64(batches), "batches/frame")
			})
		}
	}
}
