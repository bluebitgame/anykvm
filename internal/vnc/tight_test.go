package vnc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"anykvm/internal/capture"
)

func TestTightMixedFillAndJPEG(t *testing.T) {
	const w, h = 193, 131
	pixels := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			switch {
			case x < 64:
				pixels[i+2] = 211 // A solid vertical red region.
			case x < 128:
				pixels[i], pixels[i+1], pixels[i+2] = byte(x*y), byte(x+y), byte(x*3+y)
			case y < 64:
				pixels[i] = 73 // Blue, across a partial right-edge tile.
			default:
				pixels[i+1] = 157 // Green, including the partial bottom row.
			}
			pixels[i+3] = byte(x + y) // Alpha variations must not defeat Fill.
		}
	}
	c, conn := newJPEGWireClient(encTight, w, h)
	frame := &frameData{format: capture.FormatRaw, width: w, height: h, data: pixels}
	c.sendFrame(frame)
	got := readJPEGWire(t, conn, encTight)
	wantFills := map[rectangle][3]byte{
		{0, 0, 64, h}:         {211, 0, 0},
		{128, 0, 65, 64}:      {0, 0, 73},
		{128, 64, 65, h - 64}: {0, 157, 0},
	}
	coverage := make([]bool, w*h)
	jpegCount := 0
	for _, part := range got {
		if part.fill {
			if color, ok := wantFills[part.r]; !ok || color != part.rgb {
				t.Fatalf("unexpected Fill: %+v", part)
			}
			delete(wantFills, part.r)
		} else {
			jpegCount++
			if part.r != (rectangle{64, 0, 64, h}) {
				t.Fatalf("textured area did not remain JPEG: %+v", part.r)
			}
		}
		for y := part.r.y; y < part.r.y+part.r.h; y++ {
			for x := part.r.x; x < part.r.x+part.r.w; x++ {
				i := y*w + x
				if x < 0 || x >= w || y < 0 || y >= h || coverage[i] {
					t.Fatal("out-of-bounds or overlapping update")
				}
				coverage[i] = true
			}
		}
	}
	if len(wantFills) != 0 || jpegCount != 1 {
		t.Fatalf("missing Fill or JPEG regions: %+v", got)
	}
	for _, covered := range coverage {
		if !covered {
			t.Fatal("incomplete framebuffer refresh")
		}
	}
	c.sendFrame(frame)
	if conn.wire.Len() != 0 || !c.pendingReq {
		t.Fatal("unchanged mixed image resent or pending request lost")
	}
	// A pure-color change should produce only a Fill for that changed tile.
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			pixels[(y*w+x)*4+2] = 212
		}
	}
	c.sendFrame(frame)
	got = readJPEGWire(t, conn, encTight)
	if len(got) != 1 || !got[0].fill || got[0].rgb != [3]byte{212, 0, 0} || got[0].r != (rectangle{0, 0, 64, 64}) {
		t.Fatalf("pure color update did not use Fill: %+v", got)
	}
}

func TestTightFillPixelFormat(t *testing.T) {
	native := defaultPixelFormat()
	big := native
	big.bigEndian, big.redShift, big.blueShift = true, 0, 16
	full := native
	full.depth = 32
	le565 := pixelFormat{bpp: 2, depth: 16, trueColor: true, redMax: 31, greenMax: 63, blueMax: 31, redShift: 11, greenShift: 5}
	be565 := le565
	be565.bigEndian = true
	for _, tc := range []struct {
		name string
		pf   pixelFormat
		want []byte
	}{
		{"RGB888", native, []byte{0x80, 0xf8, 0x80, 0x18}},
		{"RGB888-swapped-big-endian", big, []byte{0x80, 0xf8, 0x80, 0x18}},
		{"RGB565-little-endian", le565, []byte{0x80, 0x03, 0xf4}},
		{"RGB565-big-endian", be565, []byte{0x80, 0xf4, 0x03}},
		{"32bit-full-pixel", full, []byte{0x80, 0x18, 0x80, 0xf8, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newJPEGWireClient(encTight, 64, 64)
			r := rectangle{0, 0, 64, 64}
			frame := &frameData{tightFills: map[rectangle]uint32{r: 0xf88018}}
			var wire bytes.Buffer
			if err := c.encodeRect(&wire, encTight, frame, tc.pf, r); err != nil {
				t.Fatal(err)
			}
			if wire.Len() != 12+len(tc.want) || int32(binary.BigEndian.Uint32(wire.Bytes()[8:12])) != encTight || !bytes.Equal(wire.Bytes()[12:], tc.want) {
				t.Fatalf("invalid Tight Fill wire data: %x, want body %x", wire.Bytes(), tc.want)
			}
		})
	}
}

func TestTightFillWidthLimit(t *testing.T) {
	const w, h = 2051, 257
	c, conn := newJPEGWireClient(encTight, w, h)
	c.sendFrame(&frameData{format: capture.FormatRaw, width: w, height: h, data: make([]byte, w*h*4)})
	if conn.wire.Len() != 4+2*(12+4) {
		t.Fatalf("solid screen did not use two compact Fill rectangles: %d bytes", conn.wire.Len())
	}
	got := readJPEGWire(t, conn, encTight)
	if len(got) != 2 || !got[0].fill || !got[1].fill || got[0].r != (rectangle{0, 0, 2048, h}) || got[1].r != (rectangle{2048, 0, 3, h}) {
		t.Fatalf("Fill width limit or vertical merging failed: %+v", got)
	}
}

func TestTightSolidDetectionIsExact(t *testing.T) {
	c, conn := newJPEGWireClient(encTight, 64, 64)
	c.pixelTol = 20
	pixels := make([]byte, 64*64*4)
	pixels[len(pixels)-4] = 1 // Below temporal tolerance, but not a solid color.
	c.sendFrame(&frameData{format: capture.FormatRaw, width: 64, height: 64, data: pixels})
	got := readJPEGWire(t, conn, encTight)
	if len(got) != 1 || got[0].fill || len(got[0].data) == 0 {
		t.Fatal("temporal tolerance was incorrectly used to flatten texture")
	}
}
