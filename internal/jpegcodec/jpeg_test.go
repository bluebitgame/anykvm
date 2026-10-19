package jpegcodec

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"testing"
)

func TestSelectedBackend(t *testing.T) {
	if want := os.Getenv("ANYKVM_TEST_JPEG_BACKEND"); want != "" && Backend() != want {
		t.Fatalf("JPEG backend = %s, want %s", Backend(), want)
	}
}

func colorFrame(w, h int) []byte {
	pixels := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			pixels[i], pixels[i+1], pixels[i+2] = byte(x*3+y), byte(x+y*2), byte(x*2+y*3)
			// Include unused/non-opaque alpha bytes: RGB must be preserved.
			pixels[i+3] = byte(x + y)
		}
	}
	return pixels
}

func TestRoundTrip(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {37, 25}, {320, 240}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			w, h := size[0], size[1]
			pixels := colorFrame(w, h)
			before := bytes.Clone(pixels)
			data, err := EncodeBGRA(pixels, w, h, 90)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pixels, before) {
				t.Fatal("encoder modified input")
			}
			// Decode with Go independently to check JPEG interoperability.
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
				t.Fatalf("invalid JPEG dimensions: %v", err)
			}
			decoded, dw, dh, err := DecodeBGRA(data)
			if err != nil || dw != w || dh != h || len(decoded) != len(pixels) {
				t.Fatalf("invalid BGRA output: %dx%d: %v", dw, dh, err)
			}
			// Compare selected points to the independent decoder. This detects
			// channel swaps, flipped rows and accidental alpha premultiplication.
			for _, p := range [][2]int{{0, 0}, {w / 2, h / 2}, {w - 1, h - 1}} {
				r, g, b, _ := img.At(p[0], p[1]).RGBA()
				i := (p[1]*w + p[0]) * 4
				want := [4]byte{byte(b >> 8), byte(g >> 8), byte(r >> 8), 255}
				for c := range want {
					if math.Abs(float64(decoded[i+c])-float64(want[c])) > 5 {
						t.Fatalf("pixel %v channel %d = %d, want %d", p, c, decoded[i+c], want[c])
					}
				}
			}
		})
	}
}

func TestDecodeGoJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 37, 25))
	for y := 0; y < 25; y++ {
		for x := 0; x < 37; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(y * 8), uint8(x * 5), 32, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	pixels, w, h, err := DecodeBGRA(buf.Bytes())
	if err != nil || w != 37 || h != 25 {
		t.Fatalf("decode: %dx%d: %v", w, h, err)
	}
	if pixels[2] > 15 || pixels[(h-1)*w*4+2] < 175 {
		t.Fatal("decoded rows or color channels reversed")
	}
}

func TestQuality(t *testing.T) {
	pixels := colorFrame(320, 240)
	low, err := EncodeBGRA(pixels, 320, 240, 25)
	if err != nil {
		t.Fatal(err)
	}
	high, err := EncodeBGRA(pixels, 320, 240, 90)
	if err != nil {
		t.Fatal(err)
	}
	if len(low) >= len(high) {
		t.Fatal("quality did not change JPEG size")
	}
	var errors [2]float64
	for n, encoded := range [][]byte{low, high} {
		img, err := jpeg.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 240; y++ {
			for x := 0; x < 320; x++ {
				r, g, b, _ := img.At(x, y).RGBA()
				i := (y*320 + x) * 4
				for c, v := range []uint32{b, g, r} {
					d := float64(v>>8) - float64(pixels[i+c])
					errors[n] += d * d
				}
			}
		}
	}
	if errors[1] >= errors[0] {
		t.Fatal("higher quality did not preserve more image detail")
	}
}

func TestInvalidInput(t *testing.T) {
	for _, size := range [][2]int{{0, 1}, {-1, 1}, {65536, 1}, {int(^uint(0) >> 1), 2}, {2, 2}} {
		if _, err := EncodeBGRA([]byte{0, 1, 2, 3}, size[0], size[1], 80); err == nil {
			t.Fatalf("accepted invalid frame %v", size)
		}
	}
	for _, data := range [][]byte{nil, {0xff, 0xd8}, []byte("not jpeg")} {
		if _, _, _, err := DecodeBGRA(data); err == nil {
			t.Fatal("accepted invalid JPEG")
		}
	}
}

func BenchmarkEncodeBGRA(b *testing.B) {
	for _, size := range [][2]int{{1920, 1080}, {2560, 1440}} {
		w, h := size[0], size[1]
		pixels := colorFrame(w, h)
		for _, backend := range []struct {
			name string
			fn   func([]byte, int, int, int) ([]byte, error)
		}{{Backend(), EncodeBGRA}, {"Go JPEG baseline", encodeGo}} {
			b.Run(fmt.Sprintf("%dx%d/%s", w, h, backend.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(pixels)))
				for i := 0; i < b.N; i++ {
					data, err := backend.fn(pixels, w, h, 80)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(len(data)), "JPEG-bytes")
				}
			})
		}
	}
}

func BenchmarkDecodeBGRA(b *testing.B) {
	w, h := 1920, 1080
	data, err := encodeGo(colorFrame(w, h), w, h, 80)
	if err != nil {
		b.Fatal(err)
	}
	for _, backend := range []struct {
		name string
		fn   func([]byte, int, int) ([]byte, error)
	}{{Backend(), decodeBGRA}, {"Go JPEG baseline", decodeGo}} {
		b.Run(backend.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := backend.fn(data, w, h); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
