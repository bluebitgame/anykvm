package vnc

import (
	"bytes"
	"io"
	"testing"
)

// Decode the three ZRLE subencodings emitted by this server independently of
// its pixel-format and packing helpers. These fixtures use native BGR CPIXELs.
func decodeTestZRLE(t *testing.T, wire []byte, width, height int) []byte {
	t.Helper()
	r := bytes.NewReader(wire)
	out := make([]byte, width*height*4)
	for y := 0; y < height; y += 64 {
		for x := 0; x < width; x += 64 {
			w, h := min(64, width-x), min(64, height-y)
			mode, err := r.ReadByte()
			if err != nil {
				t.Fatal(err)
			}
			index := 0
			for index < w*h {
				var color [3]byte
				if _, err := io.ReadFull(r, color[:]); err != nil {
					t.Fatal(err)
				}
				run := 1
				switch mode {
				case zrleSubSolid:
					run = w * h
				case zrleSubRLE:
					for {
						n, err := r.ReadByte()
						if err != nil {
							t.Fatal(err)
						}
						run += int(n)
						if n != 255 {
							break
						}
					}
				case zrleSubRaw:
				default:
					t.Fatalf("unexpected ZRLE mode %d", mode)
				}
				if run > w*h-index {
					t.Fatal("ZRLE run exceeded a tile")
				}
				for n := 0; n < run; n++ {
					i := ((y+index/w)*width + x + index%w) * 4
					copy(out[i:i+3], color[:])
					out[i+3] = 255
					index++
				}
			}
		}
	}
	if r.Len() != 0 {
		t.Fatal("trailing ZRLE bytes")
	}
	return out
}

func TestSharedPixelsZRLE(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {3, 2}, {63, 65}, {64, 64}, {129, 131}} {
		for _, pattern := range []string{"solid", "runs", "noise"} {
			w, h := size[0], size[1]
			pixels := vncPixelFixture(w, h, pattern)
			decoded := decodeTestZRLE(t, buildZRLETiles(pixels, w, h, defaultPixelFormat()), w, h)
			for i := range pixels {
				if i%4 != 3 && pixels[i] != decoded[i] {
					t.Fatalf("%s %dx%d color mismatch at %d", pattern, w, h, i)
				}
			}
		}
	}
	// The first color's run crosses the row boundary; it must not split there.
	pixels := []byte{1, 2, 3, 0, 1, 2, 3, 20, 1, 2, 3, 40, 1, 2, 3, 60, 7, 8, 9, 80, 7, 8, 9, 100}
	got := encodeRLETile(pixels, 12, 0, 0, 3, 2, defaultPixelFormat(), make([]byte, 4))
	if !bytes.Equal(got, []byte{1, 2, 3, 3, 7, 8, 9, 1}) {
		t.Fatalf("cross-row RLE bytes: %v", got)
	}
}

func TestVNCSharedDirtyGrid(t *testing.T) {
	const w, h = 129, 131
	previous := make([]byte, w*h*4)
	pixels := append([]byte(nil), previous...)
	for i := 3; i < len(pixels); i += 4 {
		pixels[i] = 255
	}
	if len(computeDirtyRects(previous, pixels, w, h, 0)) != 0 {
		t.Fatal("alpha changes marked opaque VNC pixels dirty")
	}
	pixels[(65*w+65)*4] = 4
	got := computeDirtyRects(previous, pixels, w, h, 0)
	if len(got) != 1 || got[0] != (rectangle{64, 64, 64, 64}) {
		t.Fatalf("VNC dirty grid: %+v", got)
	}
	if len(computeDirtyRects(previous, pixels, w, h, 4)) != 0 {
		t.Fatal("VNC tolerance changed")
	}
	for _, encoding := range []int32{encZRLE, encHextile, encZlib, encRaw} {
		if (&client{}).effectiveTileSize(encoding) != 64 {
			t.Fatalf("encoding %d does not use the 64px grid", encoding)
		}
	}
	for _, encoding := range []int32{encJPEG, encTight, encH264} {
		if (&client{}).effectiveTileSize(encoding) != 0 {
			t.Fatal("encoded video enabled pixel diffing")
		}
	}
}

func TestZRLEPackingKeepsNegotiatedFormats(t *testing.T) {
	formats := []pixelFormat{defaultPixelFormat(),
		{bpp: 2, depth: 16, trueColor: true, redMax: 31, greenMax: 63, blueMax: 31, redShift: 11, greenShift: 5},
		{bpp: 4, depth: 24, trueColor: true, bigEndian: true, redMax: 255, greenMax: 255, blueMax: 255, redShift: 16, greenShift: 8},
	}
	const w, h = 49, 3
	pixels := vncPixelFixture(w, h, "noise")
	for _, pf := range formats {
		got := buildZRLETiles(pixels, w, h, pf)
		want := []byte{zrleSubRaw}
		var color [4]byte
		for i := 0; i < len(pixels); i += 4 {
			n := pf.putCPIXEL(color[:], pixels[i+2], pixels[i+1], pixels[i])
			want = append(want, color[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("negotiated format changed: %+v", pf)
		}
	}
}

func vncPixelFixture(w, h int, pattern string) []byte {
	pixels := make([]byte, w*h*4)
	var state uint32 = 42
	for i := 0; i < w*h; i++ {
		color := uint32(0x1179b5)
		switch pattern {
		case "runs":
			color += uint32(i / 91)
		case "noise":
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			color = state
		}
		pixels[i*4], pixels[i*4+1], pixels[i*4+2], pixels[i*4+3] = byte(color), byte(color>>8), byte(color>>16), byte(i)
	}
	return pixels
}

func BenchmarkVNCSharedPixels(b *testing.B) {
	const w, h = 1280, 720
	for _, pattern := range []string{"solid", "noise"} {
		pixels := vncPixelFixture(w, h, pattern)
		previous := append([]byte(nil), pixels...)
		b.Run(pattern+"/dirty", func(b *testing.B) {
			b.SetBytes(int64(len(pixels)))
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				computeDirtyRects(previous, pixels, w, h, 0)
			}
		})
		b.Run(pattern+"/zrle", func(b *testing.B) {
			b.SetBytes(int64(len(pixels)))
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				buildZRLETiles(pixels, w, h, defaultPixelFormat())
			}
		})
	}
}
