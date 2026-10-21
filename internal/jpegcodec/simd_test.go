//go:build cgo && (amd64 || 386 || arm64) && (linux || windows || (darwin && amd64) || anykvm_jpeg_simd)

package jpegcodec

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/cpu"
)

func simdTestLevels() []int {
	if runtime.GOARCH == "arm64" && cpu.ARM64.HasASIMD {
		return []int{jpegNEON}
	}
	var levels []int
	if cpu.X86.HasSSE2 {
		levels = append(levels, jpegSSE2)
	}
	if cpu.X86.HasAVX2 {
		levels = append(levels, jpegAVX2)
	}
	return levels
}

func TestSIMDJPEG(t *testing.T) {
	for _, level := range simdTestLevels() {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			for _, size := range [][2]int{{1, 1}, {17, 19}, {64, 48}} {
				width, height := size[0], size[1]
				pixels := colorFrame(width, height)
				if width == 64 {
					rand.New(rand.NewSource(19)).Read(pixels)
				}
				for _, quality := range []int{1, 80, 100} {
					buffer, err := encodeSIMDOwned(pixels, width, height, quality, level)
					if err != nil {
						t.Fatal(err)
					}
					data := bytes.Clone(buffer.Bytes())
					buffer.Release()
					img, err := jpeg.Decode(bytes.NewReader(data))
					if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
						t.Fatalf("SIMD JPEG dimensions: %v", err)
					}
					decoded, err := decodeSIMD(data, width, height, level)
					if err != nil {
						t.Fatal(err)
					}
					checkSIMDPixels(t, decoded, img)
				}
			}
		})
	}
}

func checkSIMDPixels(t *testing.T, got []byte, want image.Image) {
	t.Helper()
	width, height := want.Bounds().Dx(), want.Bounds().Dy()
	if len(got) != width*height*4 {
		t.Fatal("incorrect SIMD output size")
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, _ := want.At(x, y).RGBA()
			for c, value := range []uint32{b, g, r, 65535} {
				if math.Abs(float64(got[(y*width+x)*4+c])-float64(value>>8)) > 5 {
					t.Fatalf("pixel (%d,%d), channel %d = %d, want %d", x, y, c, got[(y*width+x)*4+c], value>>8)
				}
			}
		}
	}
}

func TestSIMDDecodeCompatibility(t *testing.T) {
	// The Go distribution includes baseline, progressive, grayscale and CMYK
	// variants of the same image, including unusual sampling and scan layouts.
	fixtures, err := filepath.Glob(filepath.Join(runtime.GOROOT(), "src/image/testdata/*.jpeg"))
	if err != nil || len(fixtures) == 0 {
		t.Fatal("missing Go JPEG fixtures")
	}
	for _, name := range fixtures {
		t.Run(filepath.Base(name), func(t *testing.T) {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				return // Some fixtures deliberately contain invalid data.
			}
			for _, level := range simdTestLevels() {
				pixels, err := decodeSIMD(data, img.Bounds().Dx(), img.Bounds().Dy(), level)
				if err != nil {
					t.Fatal(err)
				}
				checkSIMDPixels(t, pixels, img)
			}
		})
	}
	gray := image.NewGray(image.Rect(0, 0, 19, 17))
	for y := 0; y < 17; y++ {
		for x := 0; x < 19; x++ {
			gray.SetGray(x, y, color.Gray{Y: byte(x*7 + y)})
		}
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, gray, nil); err != nil {
		t.Fatal(err)
	}
	want, err := jpeg.Decode(bytes.NewReader(data.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range simdTestLevels() {
		pixels, err := decodeSIMD(data.Bytes(), 19, 17, level)
		if err != nil {
			t.Fatal(err)
		}
		checkSIMDPixels(t, pixels, want)
	}
}

func TestSIMDEncodeReference(t *testing.T) {
	fixtures := []struct {
		name          string
		width, height int
		pixels        []byte
	}{
		{"gradient", 35, 19, colorFrame(35, 19)},
		{"noise", 33, 17, make([]byte, 33*17*4)},
		{"checkerboard", 16, 16, make([]byte, 16*16*4)},
		{"black", 1, 1, make([]byte, 4)},
		{"white", 17, 19, bytes.Repeat([]byte{255}, 17*19*4)},
	}
	rand.New(rand.NewSource(31)).Read(fixtures[1].pixels)
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			for c := 0; c < 3; c++ {
				fixtures[2].pixels[(y*16+x)*4+c] = byte(((x + y) & 1) * 255)
			}
		}
	}
	for _, fixture := range fixtures {
		for _, quality := range []int{1, 80, 100} {
			t.Run(fmt.Sprintf("%s/%d", fixture.name, quality), func(t *testing.T) {
				w, h := fixture.width, fixture.height
				want := referenceCoefficients(fixture.pixels, w, h, 0, w, h, quality)
				for _, level := range simdTestLevels() {
					buffer, err := encodeSIMDOwned(fixture.pixels, w, h, quality, level)
					if err != nil {
						t.Fatal(err)
					}
					frame, err := parseComputeJPEG(buffer.Bytes(), max(2, (w+1)&^1), max(2, (h+1)&^1))
					buffer.Release()
					if err != nil {
						t.Fatal(err)
					}
					got, err := entropyOracle(frame)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != len(want) {
						t.Fatal("SIMD block count differs from reference")
					}
					for i, coefficient := range got {
						if math.Abs(float64(coefficient-want[i])) > 1 {
							t.Fatalf("level %d, coefficient %d = %d, want %d", level, i, coefficient, want[i])
						}
					}
				}
			})
		}
	}
}

func FuzzSIMDDecode(f *testing.F) {
	data, err := encodeGo(colorFrame(19, 17), 19, 17, 80)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte{0xff, 0xd8, 0xff, 0xd9})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 64 || config.Height > 64 {
			return
		}
		got, err := decodeSIMD(data, config.Width, config.Height, jpegSIMDLevel)
		if err != nil {
			return
		}
		want, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal("SIMD accepted invalid JPEG")
		}
		checkSIMDPixels(t, got, want)
	})
}

func TestSIMDOwnedFrameLifetime(t *testing.T) {
	for _, level := range simdTestLevels() {
		pixels := colorFrame(35, 19)
		first, err := encodeSIMDOwned(pixels, 35, 19, 80, level)
		if err != nil {
			t.Fatal(err)
		}
		saved := bytes.Clone(first.Bytes())
		for i := range pixels {
			pixels[i] ^= 255
		}
		for n := 0; n < 3; n++ {
			next, err := encodeSIMDOwned(pixels, 35, 19, 80, level)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Bytes(), saved) {
				t.Fatal("encoding overwrote an outstanding frame")
			}
			if _, err := jpeg.Decode(bytes.NewReader(next.Bytes())); err != nil {
				t.Fatal(err)
			}
			next.Release()
		}
		held := first.Retain()
		first.Release()
		next, err := encodeSIMDOwned(pixels, 35, 19, 80, level)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(held.Bytes(), saved) {
			t.Fatal("encoding overwrote a retained frame")
		}
		next.Release()
		held.Release()
	}
}
