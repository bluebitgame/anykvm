package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestProgressiveQuantizationRounding(t *testing.T) {
	for factor := 6; factor <= 15; factor++ {
		step := int32(1 << (factor - 1))
		input := []int32{-step - step/2 - 1, -step - step/2, -step - step/2 + 1,
			-step/2 - 1, -step / 2, -step/2 + 1, 0, step/2 - 1, step / 2, step/2 + 1,
			step + step/2 - 1, step + step/2, step + step/2 + 1}
		want := []int32{-2, -1, -1, -1, 0, 0, 0, 0, 1, 1, 1, 2, 2}
		quantizeProgressive(input, factor)
		for i, value := range input {
			if value != want[i] {
				t.Fatalf("factor=%d coefficient=%d: got %d want %d", factor, i, value, want[i])
			}
		}
	}
}

func TestProgressiveQuantizationWire(t *testing.T) {
	for _, backend := range []string{"go", "native-scalar", "native-sse2", "native-avx2", "native"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", backend)
			for factor := 6; factor <= 15; factor++ {
				t.Run(fmt.Sprint(factor), func(t *testing.T) {
					e, err := newProgressiveEncoder(factor)
					if errors.Is(err, errProgressiveNativeUnavailable) || errors.Is(err, errProgressiveSIMDUnavailable) {
						t.Skip(err)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer e.close()
					// Include eight clipping rectangles, padded edge tiles, resize
					// and a continuation frame with the same quantization.
					for frame, size := range [][2]int{{257, 193}, {257, 193}, {129, 131}} {
						w, h := size[0], size[1]
						for _, pattern := range []string{"solid", "gradient", "edge", "noise"} {
							pixels := progressivePixels(w, h, pattern)
							all := dirtyProgressiveRects(pixels, nil, w, h)
							rects := append(all[:7:7], all[len(all)-1])
							first := e.first || e.w != w || e.h != h
							want := encodeProgressiveRects(pixels, w, h, rects, uint32(frame), first, factor)
							got, err := e.encode(pixels, w, h, rects, uint32(frame))
							if err != nil || !bytes.Equal(got, want) {
								t.Fatalf("%dx%d %s frame=%d: native/Go quantization mismatch: %v", w, h, pattern, frame, err)
							}
							found := false
							for stream := got; len(stream) >= 6; {
								kind, length := le.Uint16(stream), int(le.Uint32(stream[2:]))
								if length < 6 || length > len(stream) {
									t.Fatal("invalid block length")
								}
								if kind == 0xccc4 {
									found = true
									offset := 6 + 12 + 8*len(rects)
									q := byte(factor | factor<<4)
									if length < offset+5 || !bytes.Equal(stream[offset:offset+5], []byte{q, q, q, q, q}) {
										t.Fatal("wire quantization does not match coefficient quantization")
									}
								}
								stream = stream[length:]
							}
							if !found {
								t.Fatal("missing quantization region")
							}
						}
					}
				})
			}
		})
	}
}

func TestProgressiveQuantizationDefaults(t *testing.T) {
	t.Setenv("ANYKVM_RDP_PROGRESSIVE_BACKEND", "go")
	pixels := progressivePixels(64, 64, "noise")
	r := bitmapRect{0, 0, 64, 64}
	defaultWire := encodeProgressive(pixels, 64, 64, r, 1, true, 6)
	for _, value := range []int{0, 6, 7, 8, 15} {
		e, err := newProgressiveEncoder(value)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := e.encode(pixels, 64, 64, []bitmapRect{r}, 1)
		e.close()
		if err != nil {
			t.Fatal(err)
		}
		if value == 0 || value == 6 {
			if !bytes.Equal(wire, defaultWire) {
				t.Fatal("default Progressive output changed")
			}
		} else if len(wire) >= len(defaultWire) {
			t.Fatalf("factor %d failed to reduce this noise fixture: %d >= %d", value, len(wire), len(defaultWire))
		}
	}
	for _, value := range []int{-1, 1, 5, 16, 256} {
		if e, err := newProgressiveEncoder(value); err == nil {
			e.close()
			t.Fatalf("invalid quantization %d accepted", value)
		}
	}
}

func TestRLGR1BitSequences(t *testing.T) {
	// Hand-derived MSB-first RLGR1 codewords with initial k=kr=1.
	for _, example := range []struct {
		coeffs []int32
		wire   []byte
	}{
		{[]int32{1}, []byte{0x80}},
		{[]int32{-1}, []byte{0xa0}},
		{[]int32{-2}, []byte{0xa8}},
		{[]int32{0, 0, 1}, []byte{0x40}},
		{[]int32{1, 0, 1}, []byte{0x83, 0}},
	} {
		if got := encodeRLGR1(example.coeffs); !bytes.Equal(got, example.wire) {
			t.Fatalf("%v: %x, want %x", example.coeffs, got, example.wire)
		}
	}
}

func TestProgressiveDWTUniform(t *testing.T) {
	input := make([]int32, 4096)
	for i := range input {
		input[i] = -3072
	}
	coeffs := progressiveDWT(input)
	for i, v := range coeffs {
		want := int32(0)
		if i >= 4032 {
			want = -3072
		}
		if v != want {
			t.Fatalf("coefficient %d: %d, want %d", i, v, want)
		}
	}
}

func TestProgressiveFrameStructure(t *testing.T) {
	pixels := bytes.Repeat([]byte{13, 79, 201, 255}, 129*131)
	r := bitmapRect{128, 128, 1, 3}
	stream := encodeProgressive(pixels, 129, 131, r, 17, true, DefaultProgressiveQuantization)
	var kinds []uint16
	for len(stream) > 0 {
		if len(stream) < 6 {
			t.Fatal("truncated block header")
		}
		kind, size := le.Uint16(stream), int(le.Uint32(stream[2:]))
		if size < 6 || size > len(stream) {
			t.Fatal("invalid progressive block length")
		}
		kinds = append(kinds, kind)
		body := stream[6:size]
		switch kind {
		case 0xccc0:
			if size != 12 || le.Uint32(body) != 0xcaccacca || le.Uint16(body[4:]) != 0x100 {
				t.Fatal("bad sync")
			}
		case 0xccc3:
			if size != 10 || le.Uint16(body[1:]) != 64 || body[3] != 0 {
				t.Fatal("unexpected context flags")
			}
		case 0xccc1:
			if size != 12 || le.Uint32(body) != 17 || le.Uint16(body[4:]) != 1 {
				t.Fatal("bad frame begin")
			}
		case 0xccc4:
			if body[0] != 64 || le.Uint16(body[1:]) != 1 || body[3] != 1 || body[4] != 0 || body[5] != 0 || le.Uint16(body[6:]) != 1 {
				t.Fatal("bad region counters")
			}
			if !bytes.Equal(body[12:20], join(u16(128), u16(128), u16(1), u16(3))) {
				t.Fatal("edge clipping rectangle changed")
			}
			tile := body[25:]
			if le.Uint16(tile) != 0xccc5 || int(le.Uint32(tile[2:])) != len(tile) || int(le.Uint32(body[8:])) != len(tile) ||
				le.Uint16(tile[9:]) != 2 || le.Uint16(tile[11:]) != 2 || tile[13] != 0 {
				t.Fatal("bad SIMPLE tile coordinates or flags")
			}
			if len(tile) != 22+int(le.Uint16(tile[14:]))+int(le.Uint16(tile[16:]))+int(le.Uint16(tile[18:])) || le.Uint16(tile[20:]) != 0 {
				t.Fatal("component lengths disagree")
			}
		case 0xccc2:
			if size != 6 {
				t.Fatal("bad frame end")
			}
		}
		stream = stream[size:]
	}
	want := []uint16{0xccc0, 0xccc3, 0xccc1, 0xccc4, 0xccc2}
	if len(kinds) != len(want) {
		t.Fatal("missing progressive blocks")
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("progressive block order: %x", kinds)
		}
	}
	wire := graphicsProgressive([]byte{1, 2, 3})
	if le.Uint16(wire) != 2 || le.Uint16(wire[10:]) != 9 || le.Uint32(wire[12:]) != 1 || wire[16] != 0x20 || le.Uint32(wire[17:]) != 3 {
		t.Fatal("WIRETOSURFACE_2 layout")
	}
}

// A WIRETOSURFACE_2 payload is one Progressive frame. Decoders such as
// FreeRDP reject a second FRAME_BEGIN and clip drawing to the REGION rects.
func TestProgressiveBatchIsOneFrame(t *testing.T) {
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
			for frame, size := range [][2]int{{385, 257}, {385, 257}, {257, 129}} {
				w, h := size[0], size[1]
				rects := []bitmapRect{
					{0, 0, 64, 64}, {128, 0, 64, 64}, {256, 0, min(64, w-256), 64},
					{64, 64, 64, 64}, {192, 64, 64, 64},
					{0, 128, 64, min(64, h-128)}, {128, 128, 64, min(64, h-128)},
					{w - 1, h - 1, 1, 1},
				}
				stream, err := e.encode(progressivePixels(w, h, "gradient"), w, h, rects, uint32(frame))
				if err != nil {
					t.Fatal(err)
				}
				var kinds []uint16
				for len(stream) > 0 {
					if len(stream) < 6 {
						t.Fatal("truncated block header")
					}
					kind, size := le.Uint16(stream), int(le.Uint32(stream[2:]))
					if size < 6 || size > len(stream) {
						t.Fatal("invalid block length")
					}
					kinds = append(kinds, kind)
					body := stream[6:size]
					if kind == 0xccc4 {
						headerSize := 12 + 8*len(rects) + 5
						if len(body) < headerSize || int(le.Uint16(body[1:])) != len(rects) || le.Uint16(body[6:]) != 8 {
							t.Fatal("batch lost clipping rectangles or tiles")
						}
						for i, r := range rects {
							if !bytes.Equal(body[12+i*8:20+i*8], join(u16(r.x), u16(r.y), u16(r.w), u16(r.h))) {
								t.Fatal("nonadjacent or clipped rectangle changed")
							}
						}
						if int(le.Uint32(body[8:])) != len(body)-headerSize {
							t.Fatal("combined tile data length changed")
						}
						tiles := body[headerSize:]
						for _, r := range rects {
							if len(tiles) < 22 || le.Uint16(tiles) != 0xccc5 ||
								int(le.Uint16(tiles[9:])) != r.x/64 || int(le.Uint16(tiles[11:])) != r.y/64 {
								t.Fatal("tile coordinates do not match dirty rectangles")
							}
							n := int(le.Uint32(tiles[2:]))
							if n < 22 || n > len(tiles) {
								t.Fatal("invalid tile length")
							}
							tiles = tiles[n:]
						}
						if len(tiles) != 0 {
							t.Fatal("batch encoded extra unchanged tiles")
						}
					}
					stream = stream[size:]
				}
				want := []uint16{0xccc1, 0xccc4, 0xccc2}
				if frame != 1 {
					want = append([]uint16{0xccc0, 0xccc3}, want...)
				}
				if len(kinds) != len(want) {
					t.Fatalf("frame %d contains repeated or missing blocks: %x; want %x", frame, kinds, want)
				}
				for i := range want {
					if kinds[i] != want[i] {
						t.Fatalf("frame %d has invalid block order: %x; want %x", frame, kinds, want)
					}
				}
			}
		})
	}
}
