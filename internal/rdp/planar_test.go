package rdp

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"anykvm/internal/pixelconv"
)

// Decode the Planar container and RLE grammar independently of the encoder.
// This test decoder covers only the always-selected opaque YCoCg420 mode.
func decodePlanar420(t *testing.T, wire []byte, w, h int) []byte {
	t.Helper()
	if len(wire) == 0 || wire[0]&^byte(0x10) != planar420Header {
		t.Fatal("missing CLL=1/CS/NA flags")
	}
	cw, ch := (w+1)/2, (h+1)/2
	planes := make([]byte, pixelconv.Size420(w, h))
	offset, out := 1, 0
	for _, geometry := range [][2]int{{w, h}, {cw, ch}, {cw, ch}} {
		pw, ph := geometry[0], geometry[1]
		plane := planes[out : out+pw*ph]
		out += len(plane)
		if wire[0]&0x10 == 0 {
			if offset+len(plane) > len(wire) {
				t.Fatal("truncated raw plane")
			}
			copy(plane, wire[offset:])
			offset += len(plane)
			continue
		}
		for y := 0; y < ph; y++ {
			var value byte
			for x := 0; x < pw; {
				if offset >= len(wire) {
					t.Fatal("truncated RLE control")
				}
				control := wire[offset]
				offset++
				raw, run := int(control>>4), int(control&15)
				if run == 1 {
					run = raw + 16
					raw = 0
				} else if run == 2 {
					run = raw + 32
					raw = 0
				}
				if raw+run == 0 || x+raw+run > pw || offset+raw > len(wire) {
					t.Fatal("invalid RLE segment")
				}
				for k := 0; k < raw+run; k++ {
					if k < raw {
						value = wire[offset]
						offset++
					}
					v := value
					if y != 0 {
						delta := v >> 1
						if v&1 != 0 {
							delta = ^delta
						}
						v = plane[(y-1)*pw+x] + delta
					}
					plane[y*pw+x] = v
					x++
				}
			}
		}
	}
	if wire[0]&0x10 == 0 {
		offset++
	}
	if offset != len(wire) {
		t.Fatalf("wire consumption=%d length=%d", offset, len(wire))
	}
	return planes
}

func planarPixels(planes []byte, w, h int) []byte {
	out := make([]byte, w*h*4)
	cw, ch := (w+1)/2, (h+1)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			c := (y/2)*cw + x/2
			yy, co, cg := int(planes[i]), int(int8(planes[w*h+c])), int(int8(planes[w*h+cw*ch+c]))
			out[i*4], out[i*4+1], out[i*4+2], out[i*4+3] = byte(min(255, max(0, yy-cg+co))), byte(min(255, max(0, yy+cg))), byte(min(255, max(0, yy-cg-co))), 255
		}
	}
	return out
}

func TestPlanar420RoundTripAndNV12(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {2, 1}, {3, 3}, {7, 9}, {64, 64}, {129, 131}} {
		w, h := size[0], size[1]
		for _, pattern := range []string{"solid", "gradient", "noise"} {
			nv12 := progressiveNV12Pixels(w, h, pattern)
			bgra, err := bitmapBGRA(nv12, w, h, "nv12")
			if err != nil {
				t.Fatal(err)
			}
			var a, b planarEncoder
			for _, r := range coalesceBitmapRects(dirtyProgressiveRectsFormat(nv12, nil, w, h, progressiveNV12), w, h) {
				wire := a.encodeRect(nv12, w, h, r, progressiveNV12)
				other := b.encodeRect(bgra, w, h, r, progressiveBGRA)
				if !bytes.Equal(wire, other) {
					t.Fatalf("%dx%d %s rect=%+v: NV12 differs from clipped BGRA", w, h, pattern, r)
				}
				planes := decodePlanar420(t, wire, r.w, r.h)
				tile := bitmapTile(bgra, w, r)
				want := make([]byte, pixelconv.Size420(r.w, r.h))
				pixelconv.BGRAToYCoCg420(want, tile, r.w*4, r.w, r.h)
				if !bytes.Equal(planes, want) {
					t.Fatal("wire planes differ")
				}
			}
		}
	}
}

func TestPlanar420RLEBoundaryAndColors(t *testing.T) {
	for _, w := range []int{1, 2, 3, 15, 16, 17, 31, 32, 33, 47, 48, 49, 63, 64, 65, 128, 129, 257} {
		for _, color := range [][]byte{{0, 0, 0, 255}, {255, 255, 255, 0}, {0, 0, 255, 255}, {0, 255, 0, 255}, {255, 0, 0, 255}} {
			pixels := bytes.Repeat(color, w*3)
			wire := encodePlanar(pixels, w, 3)
			decoded := planarPixels(decodePlanar420(t, wire, w, 3), w, 3)
			for i := range decoded {
				difference := int(decoded[i]) - int(pixels[i])
				if i%4 != 3 && (difference < -1 || difference > 1) {
					t.Fatalf("color swap or excessive error w=%d", w)
				}
			}
		}
	}
	// The unchanged source planes remain available after a raw fallback.
	var e planarEncoder
	for i := 0; i < 3; i++ {
		p := progressivePixels(3, 3, "noise")
		wire := e.encodeRect(p, 3, 3, bitmapRect{0, 0, 3, 3}, progressiveBGRA)
		decodePlanar420(t, wire, 3, 3)
	}
}

func TestPlanar420Export(t *testing.T) {
	directory := os.Getenv("ANYKVM_TEST_PLANAR_EXPORT")
	if directory == "" {
		t.Skip("set ANYKVM_TEST_PLANAR_EXPORT for independent FreeRDP decoder fixtures")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{1, 1}, {2, 1}, {3, 3}, {7, 9}, {64, 64}, {129, 131}} {
		w, h := size[0], size[1]
		for _, pattern := range []string{"solid", "gradient", "noise", "red", "green", "blue"} {
			p := progressiveNV12Pixels(w, h, pattern)
			format := progressiveNV12
			if pattern == "red" || pattern == "green" || pattern == "blue" {
				c := []byte{0, 0, 0, 255}
				component := map[string]int{"red": 2, "green": 1, "blue": 0}[pattern]
				c[component] = 255
				p = bytes.Repeat(c, w*h)
				format = progressiveBGRA
			}
			var e planarEncoder
			wire := e.encodeRect(p, w, h, bitmapRect{0, 0, w, h}, format)
			name := fmt.Sprintf("%dx%d-%s", w, h, pattern)
			if err := os.WriteFile(filepath.Join(directory, name+".bin"), wire, 0600); err != nil {
				t.Fatal(err)
			}
			decoded := planarPixels(decodePlanar420(t, wire, w, h), w, h)
			if err := os.WriteFile(filepath.Join(directory, name+".bgra"), decoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Compare the same source pictures and complete 128px dirty groups. The NV12
// path includes ClearCodec's full-frame conversion; BGRA excludes it for both.
// A sequence of varying pictures exercises caches without benchmarking one
// repeatedly identical tile. Capture, transport and client decoding are excluded.
func BenchmarkPlanar420VsClear(b *testing.B) {
	const w, h = 1280, 720
	for _, content := range []string{"solid", "gradient", "noise"} {
		nv, bgra := make([][]byte, 8), make([][]byte, 8)
		for f := range nv {
			nv[f] = progressiveNV12Pixels(w, h, content)
			if content == "noise" {
				rand.New(rand.NewSource(int64(991 + f))).Read(nv[f])
			} else {
				for i := 0; i < w*h; i++ {
					nv[f][i] = byte(16 + (int(nv[f][i])-16+f*7)%220)
				}
			}
			bgra[f], _ = bitmapBGRA(nv[f], w, h, "nv12")
		}
		rects := coalesceBitmapRects(dirtyBitmapRects(bgra[0], nil, w, h), w, h)
		for _, format := range []string{"nv12", "bgra"} {
			for _, codec := range []string{"planar", "clearcodec"} {
				b.Run(content+"/"+format+"/"+codec, func(b *testing.B) {
					clear := newClearEncoder()
					var e planarEncoder
					var total int64
					b.ReportAllocs()
					b.SetBytes(int64(w * h * 4))
					b.ResetTimer()
					for f := 0; f < b.N; f++ {
						pixels, pf := bgra[f%len(nv)], progressiveBGRA
						if format == "nv12" {
							pixels = nv[f%len(nv)]
							if codec == "planar" {
								pf = progressiveNV12
							} else {
								pixels, _ = bitmapBGRA(pixels, w, h, "nv12")
							}
						}
						for _, r := range rects {
							if codec == "planar" {
								total += int64(len(e.encodeRect(pixels, w, h, r, pf)))
							} else {
								total += int64(len(clear.encode(bitmapTile(pixels, w, r), r.w, r.h)))
							}
						}
					}
					b.ReportMetric(float64(total)/float64(b.N), "encoded-B/frame")
				})
			}
		}
	}
}
