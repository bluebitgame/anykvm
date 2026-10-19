package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func progressiveNV12Pixels(w, h int, pattern string) []byte {
	pixels := make([]byte, progressivePixelBytes(w, h, progressiveNV12))
	if pattern == "noise" {
		_, _ = rand.New(rand.NewSource(91843)).Read(pixels)
		return pixels
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixels[y*w+x] = 81
			if pattern == "gradient" {
				pixels[y*w+x] = byte(16 + (x*3+y*5)%220)
			}
		}
	}
	stride := 2 * ((w + 1) / 2)
	for y := 0; y < (h+1)/2; y++ {
		for x := 0; x < (w+1)/2; x++ {
			i := w*h + y*stride + x*2
			pixels[i], pixels[i+1] = 90, 240
			if pattern == "gradient" {
				pixels[i], pixels[i+1] = byte(16+(x*11+y)%224), byte(16+(x+y*9)%224)
			}
		}
	}
	return pixels
}

func TestProgressiveNV12InputAndDirtyTiles(t *testing.T) {
	const w, h = 129, 131
	previous := progressiveNV12Pixels(w, h, "gradient")
	pixels, format, err := progressiveInputPixels(previous, w, h, "nv12")
	if err != nil || format != progressiveNV12 || &pixels[0] != &previous[0] {
		t.Fatal("NV12 input was converted or copied")
	}
	for _, data := range [][]byte{nil, previous[:len(previous)-1], append(append([]byte(nil), previous...), 0)} {
		if _, _, err := progressiveInputPixels(data, w, h, "nv12"); err == nil {
			t.Fatal("invalid NV12 length accepted")
		}
	}
	if got := dirtyProgressiveRectsFormat(previous, nil, w, h, format); len(got) != 9 || got[8] != (bitmapRect{128, 128, 1, 3}) {
		t.Fatalf("NV12 partial tile grid: %+v", got)
	}
	if got := dirtyProgressiveRectsFormat(previous, previous, w, h, format); len(got) != 0 {
		t.Fatal("unchanged NV12 picture emitted updates")
	}
	for _, point := range [][2]int{{0, 0}, {63, 63}, {64, 63}, {63, 64}, {64, 64}, {127, 127}, {128, 130}} {
		for plane := 0; plane < 3; plane++ {
			pixels := append([]byte(nil), previous...)
			x, y := point[0], point[1]
			i := y*w + x
			if plane != 0 {
				i = w*h + (y/2)*2*((w+1)/2) + (x/2)*2 + plane - 1
			}
			pixels[i] ^= 255
			tx, ty := x/64*64, y/64*64
			want := []bitmapRect{{tx, ty, min(64, w-tx), min(64, h-ty)}}
			if got := dirtyProgressiveRectsFormat(pixels, previous, w, h, format); !reflect.DeepEqual(got, want) {
				t.Fatalf("point %v plane %d: got %+v want %+v", point, plane, got, want)
			}
		}
	}
}

func TestProgressiveNV12ICT(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {2, 3}, {3, 2}, {4, 5}, {5, 4}, {7, 9}, {8, 7}, {9, 8}, {33, 53}, {64, 64}, {129, 131}, {257, 193}} {
		w, h := size[0], size[1]
		for _, pattern := range []string{"solid", "gradient", "noise"} {
			pixels := progressiveNV12Pixels(w, h, pattern)
			bgra, err := bitmapBGRA(pixels, w, h, "nv12")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range dirtyProgressiveRectsFormat(pixels, nil, w, h, progressiveNV12) {
				var got, want [3][4096]int32
				progressiveICT(&got, pixels, w, h, r.x, r.y, progressiveNV12)
				progressiveICT(&want, bgra, w, h, r.x, r.y, progressiveBGRA)
				if got != want {
					t.Fatalf("%dx%d %s tile %d,%d: direct NV12 color conversion differs", w, h, pattern, r.x, r.y)
				}
			}
		}
	}
}

func TestProgressiveNV12Wire(t *testing.T) {
	for _, backend := range []string{"go", "native-scalar", "native-sse2", "native-avx2", "native"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", backend)
			for q := 6; q <= 15; q++ {
				t.Run(fmt.Sprint(q), func(t *testing.T) {
					e, err := newProgressiveEncoder(q)
					if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
						t.Skip(err)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer e.close()
					for frame, size := range [][2]int{{1, 1}, {2, 3}, {3, 2}, {4, 5}, {5, 4}, {7, 9}, {8, 7}, {9, 8}, {33, 53}, {64, 64}, {129, 131}, {129, 131}, {257, 193}} {
						w, h := size[0], size[1]
						input := progressiveNV12Pixels(w, h, "noise")
						unaligned := make([]byte, len(input)+1)
						copy(unaligned[1:], input)
						input = unaligned[1:]
						bgra, err := bitmapBGRA(input, w, h, "nv12")
						if err != nil {
							t.Fatal(err)
						}
						rects := dirtyProgressiveRectsFormat(input, nil, w, h, progressiveNV12)
						for i := 0; i < len(rects); {
							end := progressiveBatchEnd(rects, i)
							batch := rects[i:end]
							first := e.first || e.w != w || e.h != h
							want := encodeProgressiveRects(bgra, w, h, batch, uint32(frame), first, q)
							got, err := e.encodeFormat(input, w, h, batch, uint32(frame), progressiveNV12)
							if err != nil || !bytes.Equal(got, want) {
								t.Fatalf("%dx%d frame=%d batch=%d: NV12 wire differs from BGRA reference: %v", w, h, frame, i, err)
							}
							i = end
						}
					}
				})
			}
		})
	}
}

func TestProgressiveNV12ValidationAndFallback(t *testing.T) {
	const w, h = 67, 53
	pixels := progressiveNV12Pixels(w, h, "gradient")
	rects := dirtyProgressiveRectsFormat(pixels, nil, w, h, progressiveNV12)
	native := &failedProgressiveCodec{}
	e := &progressiveEncoder{native: native, first: true, quantization: 8}
	defer e.close()
	if _, err := e.encodeFormat(pixels[:len(pixels)-1], w, h, rects, 1, progressiveNV12); err == nil {
		t.Fatal("truncated NV12 frame accepted")
	}
	if _, err := e.encodeFormat(pixels, w, h, rects, 1, 255); err == nil {
		t.Fatal("invalid pixel format accepted")
	}
	bgra, _ := bitmapBGRA(pixels, w, h, "nv12")
	got, err := e.encodeFormat(pixels, w, h, rects, 1, progressiveNV12)
	if err != nil || !bytes.Equal(got, encodeProgressiveRects(bgra, w, h, rects, 1, true, 8)) || !native.closed {
		t.Fatalf("NV12 fallback changed output: %v", err)
	}
	// A subsequent RGB source uses the same Progressive context safely.
	got, err = e.encode(bgra, w, h, rects, 2)
	if err != nil || !bytes.Equal(got, encodeProgressiveRects(bgra, w, h, rects, 2, false, 8)) {
		t.Fatalf("NV12 -> BGRA input switch changed output: %v", err)
	}
}

func BenchmarkProgressiveNV12Input(b *testing.B) {
	const w, h = 1280, 720
	for _, workload := range []string{"cursor", "full"} {
		previous := progressiveNV12Pixels(w, h, "gradient")
		pixels := append([]byte(nil), previous...)
		var previousBGRA []byte
		if workload == "cursor" {
			previousBGRA, _ = bitmapBGRA(previous, w, h, "nv12")
			for y := 19; y < 35; y++ {
				for x := 17; x < 33; x++ {
					pixels[y*w+x] = 235
				}
			}
		} else {
			previous = nil
		}
		for _, backend := range []string{"native-sse2", "native-avx2"} {
			for _, path := range []string{"convert", "direct"} {
				b.Run(workload+"/"+backend+"/"+path, func(b *testing.B) {
					b.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", backend)
					e, err := newProgressiveEncoder(0)
					if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
						b.Skip(err)
					}
					if err != nil {
						b.Fatal(err)
					}
					defer e.close()
					encode := func(frame uint32) int {
						input, prior, format := pixels, previous, progressiveNV12
						if path == "convert" {
							var err error
							input, err = bitmapBGRA(pixels, w, h, "nv12")
							if err != nil {
								b.Fatal(err)
							}
							prior, format = previousBGRA, progressiveBGRA
						}
						rects := dirtyProgressiveRectsFormat(input, prior, w, h, format)
						encoded := 0
						for i := 0; i < len(rects); {
							end := progressiveBatchEnd(rects, i)
							out, err := e.encodeFormat(input, w, h, rects[i:end], frame, format)
							if err != nil {
								b.Fatal(err)
							}
							encoded += len(out)
							i = end
						}
						return encoded
					}
					encode(0)
					b.ReportAllocs()
					b.ResetTimer()
					encoded := 0
					for frame := 0; frame < b.N; frame++ {
						encoded = encode(uint32(frame))
					}
					b.ReportMetric(float64(encoded), "encoded-B/frame")
				})
			}
		}
	}
}
