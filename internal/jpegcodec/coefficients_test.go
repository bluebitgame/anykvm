package jpegcodec

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"testing"
)

// Independent scalar DCT oracle for the shader's block order, scale, rotation,
// edge extension and chroma averaging. Tests feed the resulting coefficients
// through our Huffman writer and decode with Go's separate JPEG decoder.
func referenceCoefficients(pixels []byte, sw, sh, rotation, width, height, quality int) []int32 {
	q := quantization420(quality)
	columns, rows := (width+15)/16, (height+15)/16
	out := make([]int32, columns*rows*384)
	dw, dh := sw, sh
	if rotation == 90 || rotation == 270 {
		dw, dh = sh, sw
	}
	rgb := func(x, y int) [3]float64 {
		x, y = min(x, width-1), min(y, height-1)
		x, y = min(int((float64(x)+0.5)*float64(dw)/float64(width)), dw-1), min(int((float64(y)+0.5)*float64(dh)/float64(height)), dh-1)
		sx, sy := x, y
		switch rotation {
		case 90:
			sx, sy = y, dw-1-x
		case 180:
			sx, sy = dw-1-x, dh-1-y
		case 270:
			sx, sy = dh-1-y, x
		}
		i := (sy*sw + sx) * 4
		return [3]float64{float64(pixels[i+2]), float64(pixels[i+1]), float64(pixels[i])}
	}
	var basis [8][8]float64
	for k := range 8 {
		factor := 0.5
		if k == 0 {
			factor = math.Sqrt(1.0 / 8)
		}
		for x := range 8 {
			basis[k][x] = factor * math.Cos(float64((2*x+1)*k)*math.Pi/16)
		}
	}
	for my := range rows {
		for mx := range columns {
			for block := range 6 {
				var samples [8][8]float64
				for y := range 8 {
					for x := range 8 {
						if block < 4 {
							c := rgb(mx*16+block%2*8+x, my*16+block/2*8+y)
							samples[y][x] = .299*c[0] + .587*c[1] + .114*c[2] - 128
						} else {
							for dy := range 2 {
								for dx := range 2 {
									c := rgb(mx*16+x*2+dx, my*16+y*2+dy)
									if block == 4 {
										samples[y][x] += (-.168736*c[0] - .331264*c[1] + .5*c[2]) / 4
									} else {
										samples[y][x] += (.5*c[0] - .418688*c[1] - .081312*c[2]) / 4
									}
								}
							}
						}
					}
				}
				for v := range 8 {
					for u := range 8 {
						sum := 0.0
						for y := range 8 {
							for x := range 8 {
								sum += basis[v][y] * basis[u][x] * samples[y][x]
							}
						}
						table := 0
						if block >= 4 {
							table = 64
						}
						value := int32(math.Round(sum / float64(q[table+v*8+u])))
						low := int32(-1023)
						if v == 0 && u == 0 {
							low = -1024
						}
						out[((my*columns+mx)*6+block)*64+v*8+u] = min(int32(1023), max(low, value))
					}
				}
			}
		}
	}
	return out
}

func jpegTestPixels(w, h int) []byte {
	p := make([]byte, w*h*4)
	for y := range h {
		for x := range w {
			i := (y*w + x) * 4
			p[i], p[i+1], p[i+2], p[i+3] = byte(x*120/max(1, w-1)+25), byte(y*150/max(1, h-1)+30), byte(220-x*70/max(1, w-1)), 255
		}
	}
	return p
}

func TestQuantizedJPEGIndependentDecode(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {3, 5}, {16, 16}, {17, 19}, {48, 32}} {
		w, h := size[0], size[1]
		pixels := jpegTestPixels(w, h)
		for _, quality := range []int{1, 50, 80, 100} {
			coefficients := referenceCoefficients(pixels, w, h, 0, w, h, quality)
			data, err := encodeQuantized420(coefficients, w, h, quality)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil || decoded.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatalf("%dx%d q%d: %v", w, h, quality, err)
			}
			if quality >= 80 {
				var total float64
				for y := range h {
					for x := range w {
						c := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
						i := (y*w + x) * 4
						total += math.Abs(float64(c.R)-float64(pixels[i+2])) + math.Abs(float64(c.G)-float64(pixels[i+1])) + math.Abs(float64(c.B)-float64(pixels[i]))
					}
				}
				if mae := total / float64(w*h*3); mae > 18 {
					t.Fatalf("%dx%d q%d color error %.2f", w, h, quality, mae)
				}
			}
		}
	}
}

func TestQuantizedJPEGEntropyExtremes(t *testing.T) {
	// Exercise DC sign/category changes, long zero runs, all AC categories and
	// stuffed FF bytes with a decoder independent of the encoder.
	rng := rand.New(rand.NewSource(34))
	c := make([]int32, 384*4)
	for offset := 0; offset < len(c); offset += 64 {
		c[offset] = int32(rng.Intn(2048) - 1024)
		for i := 1; i < 64; i++ {
			if rng.Intn(3) == 0 {
				c[offset+i] = int32(rng.Intn(2047) - 1023)
			}
		}
	}
	data, err := encodeQuantized420(c, 32, 32, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 63} {
		bad := append([]int32(nil), c...)
		bad[offset] = 2048
		if _, err := encodeQuantized420(bad, 32, 32, 80); err == nil {
			t.Fatal("accepted out-of-range coefficient")
		}
	}
	if _, err := encodeQuantized420(c[:4], 32, 32, 80); err == nil {
		t.Fatal("accepted incomplete coefficients")
	}
}

func TestQuantizedJPEGQuality(t *testing.T) {
	pixels := jpegTestPixels(64, 48)
	var sizes []int
	for _, q := range []int{20, 90} {
		data, err := encodeQuantized420(referenceCoefficients(pixels, 64, 48, 0, 64, 48, q), 64, 48, q)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(data))
	}
	if sizes[0] >= sizes[1] {
		t.Fatalf("quality did not increase payload: %v", sizes)
	}
}

func TestRestartJPEGGoldenScan(t *testing.T) {
	// Zero coefficients encode four Y blocks as 001010 each, then Cb/Cr
	// as four zero bits each: 28 A2 8A 00. Restart markers are not stuffed.
	for _, size := range [][2]int{{1, 1}, {32, 16}, {48, 48}, {64, 48}} {
		width, height := size[0], size[1]
		count := ((width + 15) / 16) * ((height + 15) / 16)
		coefficients := make([]int32, count*384)
		actual, err := encodeQuantized420Restart(coefficients, width, height, 80, true)
		if err != nil {
			t.Fatal(err)
		}
		expected := jpegHeader420(width, height, 80, true)
		for mcu := range count {
			expected = append(expected, 0x28, 0xa2, 0x8a, 0)
			if mcu+1 < count {
				expected = append(expected, 255, byte(0xd0+mcu%8))
			}
		}
		expected = append(expected, 255, 0xd9)
		if !bytes.Equal(actual, expected) {
			t.Fatal("restart scan differs from golden bits")
		}
		image, err := jpeg.Decode(bytes.NewReader(actual))
		if err != nil {
			t.Fatal(err)
		}
		pixel := color.NRGBAModel.Convert(image.At(width-1, height-1)).(color.NRGBA)
		if pixel.R != 128 || pixel.G != 128 || pixel.B != 128 {
			t.Fatalf("restart/DC reset changed gray: %v", pixel)
		}
	}
}

func TestGPUJPEGFrameBounds(t *testing.T) {
	var parameters jpegFrameParameters
	if err := parameters.prepare(3840, 2160, 80); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{0, 16}, {16, -1}, {8192, 8192}, {8193, 16}} {
		if err := parameters.prepare(size[0], size[1], 80); err == nil {
			t.Fatalf("accepted unbounded frame %v", size)
		}
	}
}

func TestJPEGFrameParameterCache(t *testing.T) {
	var parameters jpegFrameParameters
	for _, frame := range [][3]int{{32, 16, 80}, {48, 16, 80}, {48, 32, 80}, {48, 32, 20}, {48, 32, 100}, {48, 32, 200}, {48, 32, 0}, {48, 32, -10}} {
		width, height, quality := frame[0], frame[1], frame[2]
		previousHeader := parameters.header
		reuse := parameters.width == width && parameters.height == height && parameters.quality == min(100, max(1, quality))
		if err := parameters.prepare(width, height, quality); err != nil {
			t.Fatal(err)
		}
		if reuse && &parameters.header[0] != &previousHeader[0] {
			t.Fatalf("equivalent quality rebuilt the JPEG header for %v", frame)
		}
		if !bytes.Equal(parameters.header, jpegHeader420(width, height, quality, true)) ||
			parameters.quantization != quantization420(quality) {
			t.Fatalf("stale JPEG parameters for %v", frame)
		}
		header := parameters.header
		allocations := testing.AllocsPerRun(100, func() {
			if err := parameters.prepare(width, height, quality); err != nil {
				t.Fatal(err)
			}
		})
		if allocations != 0 || &parameters.header[0] != &header[0] {
			t.Fatalf("cache hit rebuilt parameters for %v: %v allocations", frame, allocations)
		}
	}
}
