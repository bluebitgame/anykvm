package rdp

import (
	"bytes"
	"testing"
)

func TestClearGlyphLifetime(t *testing.T) {
	e := newClearEncoder()
	pixels := []byte{10, 20, 30, 0, 40, 50, 60, 255}
	first := e.encode(pixels, 2, 1)
	if first[0] != 5 || first[1] != 0 || le.Uint16(first[2:]) != 0 {
		t.Fatal("first glyph missing index or cursor reset")
	}
	want := []byte{3, 1, 0, 0}
	if got := e.encode(pixels, 2, 1); !bytes.Equal(got, want) {
		t.Fatalf("glyph hit %x, want %x", got, want)
	}
	// Alpha is omitted for opaque XRGB updates and must not defeat a hit.
	pixels[3] = 255
	if got := e.encode(pixels, 2, 1); got[0] != 3 || len(got) != 4 {
		t.Fatal("unused alpha changed cache identity")
	}
	// Identical scan-order pixels with different dimensions are different glyphs.
	if got := e.encode(pixels, 1, 2); got[0]&2 != 0 {
		t.Fatal("glyph hit with incorrect dimensions")
	}
	for i := 0; i < 4000; i++ {
		e.encode([]byte{byte(i), byte(i >> 8), 210, 0}, 1, 1)
	}
	if got := e.encode(pixels, 2, 1); got[0]&2 != 0 {
		t.Fatal("evicted glyph still referenced")
	}
	if len(e.glyph.lookup) > 4000 {
		t.Fatal("glyph cache grew beyond slot count")
	}
}

func TestClearBandsAndRollback(t *testing.T) {
	e := newClearEncoder()
	const w, h = 128, 32
	image := func(bg byte, on int) []byte {
		p := bytes.Repeat([]byte{bg, bg, bg, 255}, w*h)
		for y := on; y < on+12; y++ {
			for x := 0; x < w; x++ {
				i := (y*w + x) * 4
				p[i], p[i+1], p[i+2] = byte(x%3), byte(y-on), 180
			}
		}
		return p
	}
	first := e.encode(image(255, 2), w, h)
	if le.Uint32(first[6:]) == 0 || e.short.cursor != 3 || e.bar.cursor != 3 {
		t.Fatal("bands did not populate the three repeating columns")
	}
	second := e.encode(image(250, 5), w, h)
	// Changing the background and placement still reuses the short content.
	if le.Uint16(second[25:]) != 0x4000 || second[27] != 5 || e.short.cursor != 3 || e.bar.cursor != 6 {
		t.Fatal("short bar hit or full bar insertion incorrect")
	}
	third := e.encode(image(250, 5), w, h)
	if le.Uint16(third[25:]) != 0x8003 || e.bar.cursor != 6 {
		t.Fatal("full bar hit advanced the cursor")
	}
	oldBar, oldShort := e.bar.cursor, e.short.cursor
	// A single RGB run wins over bands. Speculative cache entries must vanish.
	e.encode(bytes.Repeat([]byte{201, 202, 203, 255}, w*h), w, h)
	if e.bar.cursor != oldBar || e.short.cursor != oldShort {
		t.Fatal("unsent band writes changed the peer cache model")
	}
	// A cached prefix makes the sampled estimate attractive, but the long
	// horizontal runs in the remainder make residual coding smaller overall.
	// This forces real speculative inserts followed by rollback.
	var prefix []byte
	for y := 0; y < h; y++ {
		prefix = append(prefix, byte(y), byte(2*y), byte(3*y))
	}
	e.bar.put(string(prefix))
	misleading := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			if x < 16 {
				copy(misleading[i:i+3], prefix[y*3:y*3+3])
			} else {
				group := (x - 16) / 28
				misleading[i], misleading[i+1], misleading[i+2] = byte(y+40+group), byte(2*y+40+group), byte(3*y+40+group)
			}
		}
	}
	limit := len(encodeClearCodec(misleading, w, h, 0)) - 2
	if _, worthwhile := e.bandEstimate(misleading, w, h, limit); !worthwhile {
		t.Fatal("fixture no longer exercises speculative bands")
	}
	oldBar, oldShort = e.bar.cursor, e.short.cursor
	if got := e.encodeBands(misleading, w, h, limit); got != nil || e.bar.cursor != oldBar || e.short.cursor != oldShort {
		t.Fatal("discarded band candidate retained cache writes")
	}
	e.short.cursor = 16380
	if got := e.encode(image(255, 2), w, h); got[0]&4 == 0 || e.short.cursor != 3 || e.bar.cursor != 3 {
		t.Fatal("short cursor exhaustion did not synchronize reset")
	}
}

func TestTrimBitmapRect(t *testing.T) {
	previous := make([]byte, 128*128*4)
	pixels := append([]byte(nil), previous...)
	pixels[(17*128+9)*4] = 1
	pixels[(20*128+31)*4+1] = 2
	r := trimBitmapRect(pixels, previous, 128, bitmapRect{0, 0, 128, 128})
	if r != (bitmapRect{9, 17, 23, 4}) {
		t.Fatalf("changed bounds: %+v", r)
	}
}
