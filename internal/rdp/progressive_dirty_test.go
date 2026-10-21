package rdp

import (
	"bytes"
	"errors"
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
