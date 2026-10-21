package jpegcodec

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func constantJPEGCoefficients(w, h int, dc [3]int32) []int32 {
	coeff := make([]int32, ((w+15)/16)*((h+15)/16)*384)
	for offset := 0; offset < len(coeff); offset += 64 {
		component := 0
		if block := (offset / 64) % 6; block >= 4 {
			component = block - 3
		}
		coeff[offset] = dc[component]
	}
	return coeff
}

func TestSolidJPEGColorIndependentDecode(t *testing.T) {
	for _, dc := range [][3]int32{{0, 0, 0}, {-101, 0, 0}, {43, -19, 27}, {-63, 35, -11}} {
		for _, quality := range []int{1, 19, 75, 95, 100} {
			for _, restart := range []bool{false, true} {
				const w, h = 65, 67
				// Start with legal constant sample offsets, then quantize their
				// DC amplitudes for this quality instead of inventing huge IDCT
				// outputs when a low-quality quantizer is used.
				q := quantization420(quality)
				quantized := dc
				for c := range quantized {
					table := 0
					if c > 0 {
						table = 64
					}
					quantized[c] = dc[c] * 8 / int32(q[table])
				}
				data, err := encodeQuantized420Restart(constantJPEGCoefficients(w, h, quantized), w, h, quality, restart)
				if err != nil {
					t.Fatal(err)
				}
				got, ok := SolidJPEGColor(data)
				if !ok {
					t.Fatalf("constant rejected: DC=%v q=%d restart=%v", dc, quality, restart)
				}
				// The independent standard decoder performs the real IDCT and
				// dequantization; compare every visible pixel, including edges.
				img, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						r, g, b, _ := img.At(x, y).RGBA()
						want := uint32(r>>8)<<16 | uint32(g>>8)<<8 | uint32(b>>8)
						if got != want {
							t.Fatalf("DC=%v q=%d restart=%v pixel=(%d,%d) color=%06x want=%06x", dc, quality, restart, x, y, got, want)
						}
					}
				}
				if restart {
					source, err := ParseRestartJPEG(data)
					if err != nil {
						t.Fatal(err)
					}
					checker := source.NewSolidChecker()
					for _, r := range []image.Rectangle{image.Rect(0, 0, w, h), image.Rect(64, 64, w, h)} {
						if actual, ok := checker.Color(r); !ok || actual != got {
							t.Fatalf("independent-MCU color mismatch for %v: %06x %v", r, actual, ok)
						}
					}
				}
			}
		}
	}
}

func TestSolidJPEGRejectsACAndDifferentDC(t *testing.T) {
	for _, index := range []int{63, 64, 4*64 + 2, 384, 384 + 4*64} {
		coeff := constantJPEGCoefficients(32, 16, [3]int32{42, -3, 4})
		coeff[index]++
		for _, restart := range []bool{false, true} {
			data, err := encodeQuantized420Restart(coeff, 32, 16, 75, restart)
			if err != nil {
				t.Fatal(err)
			}
			if _, solid := SolidJPEGColor(data); solid {
				t.Fatalf("texture/DC change at %d flattened, restart=%v", index, restart)
			}
			if restart {
				source, _ := ParseRestartJPEG(data)
				if _, solid := source.NewSolidChecker().Color(image.Rect(0, 0, 32, 16)); solid {
					t.Fatal("independent-MCU probe flattened texture")
				}
			}
		}
	}
	invalid, err := encodeQuantized420Restart(constantJPEGCoefficients(16, 16, [3]int32{1000, 0, 0}), 16, 16, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, solid := SolidJPEGColor(invalid); solid {
		t.Fatal("extreme dequantized DC replaced by Fill")
	}
}

func TestRestartSolidCheckerSelectsOnlyRegion(t *testing.T) {
	const w, h = 128, 64
	coeff := constantJPEGCoefficients(w, h, [3]int32{40, -3, 4})
	coeff[4*384+1] = 1 // Texture in the right half, outside the requested left tile.
	data, err := encodeQuantized420Restart(coeff, w, h, 80, true)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := ParseRestartJPEG(data)
	checker := source.NewSolidChecker()
	if _, solid := checker.Color(image.Rect(0, 0, 64, 64)); !solid {
		t.Fatal("unrelated texture affected a solid region")
	}
	if _, solid := checker.Color(image.Rect(64, 0, 128, 64)); solid {
		t.Fatal("textured region reported solid")
	}
	for _, r := range []image.Rectangle{image.Rect(1, 0, 64, 64), image.Rect(0, 0, 129, 64), {}} {
		if _, solid := checker.Color(r); solid {
			t.Fatalf("invalid region accepted: %v", r)
		}
	}
}

func TestSolidJPEGGrayAndUnsupportedInputs(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 17, 19))
	for y := 0; y < 19; y++ {
		for x := 0; x < 17; x++ {
			gray.SetGray(x, y, color.Gray{Y: 37})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gray, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if _, solid := SolidJPEGColor(buf.Bytes()); !solid {
		t.Fatal("constant grayscale JPEG rejected")
	}
	valid, _ := encodeQuantized420Restart(constantJPEGCoefficients(16, 16, [3]int32{}), 16, 16, 80, true)
	for _, metadata := range [][]byte{
		{255, 0xe2, 0, 14, 'I', 'C', 'C', '_', 'P', 'R', 'O', 'F', 'I', 'L', 'E', 0},
		{255, 0xee, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}, // Adobe RGB.
	} {
		data := append(bytes.Clone(valid[:2]), metadata...)
		data = append(data, valid[2:]...)
		if _, solid := SolidJPEGColor(data); solid {
			t.Fatal("unknown color interpretation replaced by Fill")
		}
	}
	progressive := bytes.Clone(valid)
	sof := bytes.Index(progressive, []byte{255, 0xc0})
	progressive[sof+1] = 0xc2
	for _, data := range [][]byte{nil, valid[:len(valid)-3], progressive, {255, 0xd8, 255, 0xdb, 255, 255}} {
		if _, solid := SolidJPEGColor(data); solid {
			t.Fatal("unsupported/truncated scan reported solid")
		}
	}
	source, _ := ParseRestartJPEG(valid)
	checker := source.NewSolidChecker()
	// Corrupt only entropy after the header has been successfully validated.
	source.segments[0] = []byte{255}
	if _, solid := checker.Color(image.Rect(0, 0, 16, 16)); solid {
		t.Fatal("malformed entropy reported solid")
	}
}
