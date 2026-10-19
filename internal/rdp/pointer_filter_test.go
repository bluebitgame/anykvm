package rdp

import (
	"bytes"
	"testing"

	"anykvm/internal/capture"
)

func TestPointerReductionAveragesPremultipliedAlpha(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pixels []byte
		want   [4]byte
	}{
		{"opaque-checker", []byte{
			255, 255, 255, 255, 0, 0, 0, 255,
			0, 0, 0, 255, 255, 255, 255, 255,
		}, [4]byte{128, 128, 128, 255}},
		{"transparent-color-does-not-bleed", []byte{
			0, 0, 255, 255, 255, 255, 0, 0,
			255, 255, 0, 0, 255, 255, 0, 0,
		}, [4]byte{0, 0, 255, 64}},
		{"partial-alpha-weighted-colors", []byte{
			0, 0, 255, 128, 255, 0, 0, 64,
			0, 0, 255, 128, 255, 0, 0, 64,
		}, [4]byte{85, 0, 170, 96}},
		{"all-transparent", []byte{
			255, 0, 0, 0, 0, 255, 0, 0,
			0, 0, 255, 0, 255, 255, 255, 0,
		}, [4]byte{}},
		{"legacy-threshold-after-average", []byte{
			0, 0, 255, 255, 0, 0, 255, 255,
			0, 255, 0, 0, 0, 255, 0, 0,
		}, [4]byte{0, 0, 255, 128}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, premultiplied := range []bool{false, true} {
				pixels := bytes.Clone(tc.pixels)
				if premultiplied {
					for i := 0; i < len(pixels); i += 4 {
						for c := 0; c < 3; c++ {
							pixels[i+c] = byte((int(pixels[i+c])*int(pixels[i+3]) + 127) / 255)
						}
					}
				}
				cs := &capture.CursorState{Width: 2, Height: 2, Image: pixels, Premultiplied: premultiplied}
				for _, alpha := range []bool{false, true} {
					p, err := scaledCursorPointer(cs, alpha, 96, .5, .5)
					if err != nil {
						t.Fatal(err)
					}
					a := p[22:]
					if alpha {
						a = a[2:]
					}
					if le.Uint16(a[6:]) != 1 || le.Uint16(a[8:]) != 1 {
						t.Fatal("incorrect reduced dimensions")
					}
					xor := a[14 : 14+int(le.Uint16(a[12:]))]
					and := a[14+int(le.Uint16(a[12:])):]
					transparent := tc.want[3] == 0 || (!alpha && tc.want[3] < 128)
					if (and[0]&0x80 != 0) != transparent {
						t.Fatal("transparency mask disagrees with averaged alpha")
					}
					if transparent {
						if !bytes.Equal(xor, make([]byte, len(xor))) {
							t.Fatal("transparent XOR must remain black")
						}
					} else {
						if !bytes.Equal(xor[:3], tc.want[:3]) {
							t.Fatalf("premultiplied=%t alpha=%t BGRA=%v want %v", premultiplied, alpha, xor, tc.want)
						}
						if alpha && xor[3] != tc.want[3] {
							t.Fatalf("averaged alpha=%d want %d", xor[3], tc.want[3])
						}
					}
				}
			}
		})
	}
}

func TestPointerReductionIncludesFractionalBorderCoverage(t *testing.T) {
	// 5->3 pixels: the coverage weights are 3:2, 1:3:1 and 2:3.
	// All five source pixels contribute, including the rightmost border.
	cs := &capture.CursorState{Width: 5, Height: 1, HotX: 4, Image: []byte{
		0, 0, 0, 255, 30, 0, 0, 255, 60, 0, 0, 255, 90, 0, 0, 255, 120, 0, 0, 255,
	}}
	p, err := scaledCursorPointer(cs, true, 96, .6, .6)
	if err != nil {
		t.Fatal(err)
	}
	a := p[24:]
	if le.Uint16(a[6:]) != 3 || le.Uint16(a[8:]) != 1 || le.Uint16(a[2:]) != 2 {
		t.Fatal("rounded dimensions/hotspot changed")
	}
	want := []byte{12, 0, 0, 255, 60, 0, 0, 255, 108, 0, 0, 255}
	if !bytes.Equal(a[14:26], want) {
		t.Fatalf("coverage average=%v want %v", a[14:26], want)
	}
}

func TestPointerFractionalReductionWeightsPremultipliedAlphaInBothAxes(t *testing.T) {
	cs := &capture.CursorState{Width: 4, Height: 4, Premultiplied: true, Image: make([]byte, 4*4*4)}
	// Two diagonal pixels contribute partial coverage to neighboring outputs.
	red, blue := (1*4+1)*4, (2*4+2)*4
	cs.Image[red+2], cs.Image[red+3] = 128, 128
	cs.Image[blue], cs.Image[blue+3] = 64, 64
	p, err := scaledCursorPointer(cs, true, 96, .75, .75)
	if err != nil {
		t.Fatal(err)
	}
	a := p[24:]
	if le.Uint16(a[6:]) != 3 || le.Uint16(a[8:]) != 3 {
		t.Fatal("fractional reduction did not produce 3x3 pixels")
	}
	for _, tc := range []struct {
		x, y int
		want []byte
	}{
		{0, 0, []byte{0, 0, 255, 8}},
		{1, 1, []byte{85, 0, 170, 48}},
		{2, 2, []byte{255, 0, 0, 4}},
	} {
		got := a[14+(2-tc.y)*12+tc.x*4:][:4]
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("fractional pixel %d,%d = %v want %v", tc.x, tc.y, got, tc.want)
		}
	}
}

func TestPointerReductionThirdAndQuarter(t *testing.T) {
	for _, divisor := range []int{3, 4} {
		size := 2 * divisor
		cs := &capture.CursorState{Width: size, Height: size, Image: make([]byte, size*size*4)}
		// Each block has one opaque pixel, positioned at its bottom-right.
		// Point sampling would miss all of them. Other transparent pixels
		// contain unrelated colors, which must not affect the output.
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				i := (y*size + x) * 4
				cs.Image[i+1] = 255
				if x%divisor == divisor-1 && y%divisor == divisor-1 {
					cs.Image[i], cs.Image[i+1], cs.Image[i+3] = byte(40*(y/divisor*2+x/divisor+1)), 0, 255
				}
			}
		}
		p, err := scaledCursorPointer(cs, true, 96, 1/float64(divisor), 1/float64(divisor))
		if err != nil {
			t.Fatal(err)
		}
		a := p[24:]
		if le.Uint16(a[6:]) != 2 || le.Uint16(a[8:]) != 2 {
			t.Fatal("incorrect divisor")
		}
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				got := a[14+((1-y)*2+x)*4:][:4]
				want := []byte{byte(40 * (y*2 + x + 1)), 0, 0, byte((255 + divisor*divisor/2) / (divisor * divisor))}
				if !bytes.Equal(got, want) {
					t.Fatalf("1/%d block %d,%d = %v want %v", divisor, x, y, got, want)
				}
			}
		}
	}
}
