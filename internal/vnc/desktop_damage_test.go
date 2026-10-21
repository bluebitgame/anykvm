package vnc

import (
	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"image"
	"testing"
)

func TestDesktopDamageDelivery(t *testing.T) {
	const w, h = 192, 128
	c, conn := newJPEGWireClient(encJPEG, w, h)
	pixels := make([]byte, w*h*4)
	seq := uint64(0)
	send := func(previous uint64, valid bool, rects []image.Rectangle) []jpegWireRect {
		seq++
		d := &jpegcodec.DesktopDamage{Stream: 1, Previous: previous, Sequence: seq, SourceWidth: w, SourceHeight: h, Valid: valid, Rects: rects}
		c.sendFrame(&frameData{format: capture.FormatRaw, width: w, height: h, data: pixels, damage: d})
		if conn.wire.Len() == 0 {
			return nil
		}
		return readJPEGWire(t, conn, encJPEG)
	}
	if got := send(0, true, nil); len(got) != 1 || got[0].r != (rectangle{0, 0, w, h}) {
		t.Fatalf("first frame: %v", got)
	}
	// Multiple overlapping source rectangles cover the same tile only once.
	pixels[(70*w+70)*4] = 90
	rects := []image.Rectangle{image.Rect(68, 68, 75, 75), image.Rect(69, 69, 72, 72)}
	if got := send(1, true, rects); len(got) != 1 || got[0].r != (rectangle{64, 64, 64, 64}) {
		t.Fatalf("overlapping damage: %v", got)
	}
	// Suppressed updates must still advance acquisition continuity.
	if got := send(2, true, nil); len(got) != 0 {
		t.Fatal("empty metadata sent pixels")
	}
	pixels[(70*w+70)*4] = 100
	if got := send(3, true, rects); len(got) != 1 {
		t.Fatal("continuity lost after unchanged input")
	}
	// Capture/queue drop: this frame reports only the final update. The earlier
	// dropped frame changed the top-left tile, so both must be found on fallback.
	pixels[0] = 90
	pixels[(70*w+70)*4] = 110
	seq++
	if got := send(seq, true, rects); len(got) != 2 {
		t.Fatalf("dropped frame failed full comparison: %v", got)
	}
	pixels[0] = 100
	if got := send(seq, false, nil); len(got) != 1 {
		t.Fatal("invalid metadata skipped comparison")
	}
	pixels[0] = 110
	c.sendFrame(&frameData{format: capture.FormatRaw, width: w, height: h, data: pixels})
	if got := readJPEGWire(t, conn, encJPEG); len(got) != 1 {
		t.Fatal("missing metadata skipped comparison")
	}
}

func TestDesktopDamageCandidatesSkipAndTolerance(t *testing.T) {
	const w, h = 130, 65
	prev, cur := make([]byte, w*h*4), make([]byte, w*h*4)
	cur[0] = 8
	cur[(64*w+129)*4] = 9
	if got := computeDirtyRectsCandidates(prev, cur, w, h, 0, []uint32{5}, true); len(got) != 1 || got[0] != (rectangle{128, 64, 2, 1}) {
		t.Fatalf("candidate selection: %v", got)
	}
	if got := computeDirtyRectsCandidates(prev, cur, w, h, 0, nil, true); len(got) != 0 {
		t.Fatal("empty candidates compared pixels")
	}
	if got := computeDirtyRectsCandidates(prev, cur, w, h, 10, []uint32{5}, true); len(got) != 0 {
		t.Fatal("tolerance ignored")
	}
	if got := computeDirtyRectsCandidates(prev, cur, w, h, 0, []uint32{999}, true); len(got) != 2 {
		t.Fatal("malformed candidates did not fall back")
	}
}

func TestDesktopDamageToleranceDrift(t *testing.T) {
	const w, h = 128, 64
	c, conn := newJPEGWireClient(encJPEG, w, h)
	c.pixelTol = 10
	pixels := make([]byte, w*h*4)
	send := func(seq uint64, value byte, rects []image.Rectangle) []jpegWireRect {
		pixels[0] = value
		c.sendFrame(&frameData{format: capture.FormatRaw, width: w, height: h, data: pixels, damage: &jpegcodec.DesktopDamage{Stream: 2, Previous: seq - 1, Sequence: seq, SourceWidth: w, SourceHeight: h, Valid: true, Rects: rects}})
		if conn.wire.Len() == 0 {
			return nil
		}
		return readJPEGWire(t, conn, encJPEG)
	}
	send(1, 0, nil)
	dirty := []image.Rectangle{image.Rect(0, 0, 1, 1)}
	if got := send(2, 6, dirty); len(got) != 0 {
		t.Fatal("sub-tolerance change sent")
	}
	if got := send(3, 6, nil); len(got) != 0 {
		t.Fatal("unchanged input sent")
	}
	if got := send(4, 12, dirty); len(got) != 1 {
		t.Fatal("drift was compared against unsent input")
	}
	// A failed write cannot advance metadata continuity.
	conn.fail = true
	send(5, 30, dirty)
	d := &jpegcodec.DesktopDamage{Stream: 2, Previous: 5, Sequence: 6, SourceWidth: w, SourceHeight: h, Valid: true}
	if _, valid := c.damageState.CandidateTiles(d, w, h, 0); valid {
		t.Fatal("failed write advanced damage state")
	}
}

func TestDesktopDamageQueueCoalescing(t *testing.T) {
	const w, h = 256, 256
	c, conn := newJPEGWireClient(encJPEG, w, h)
	pixels := make([]byte, w*h*4)
	frame := func(prev, seq uint64, rects ...image.Rectangle) *frameData {
		return &frameData{format: capture.FormatRaw, width: w, height: h, data: pixels, damage: &jpegcodec.DesktopDamage{Stream: 3, Previous: prev, Sequence: seq, SourceWidth: w, SourceHeight: h, Valid: true, Rects: rects}}
	}
	c.sendFrame(frame(0, 1))
	readJPEGWire(t, conn, encJPEG)
	pixels[0] = 30
	b := frame(1, 2, image.Rect(0, 0, 1, 1))
	c.offerFrame(b)
	pixels[(130*w+130)*4] = 40
	// The native queue has already merged acquisitions 3 and 4 into this one.
	d := frame(2, 4, image.Rect(130, 130, 131, 131), image.Rect(131, 131, 132, 132))
	c.offerFrame(d)
	if c.latest.damage.Previous != 1 || c.latest.damage.Sequence != 4 {
		t.Fatalf("interval lost: %+v", c.latest.damage)
	}
	ids, valid := c.damageState.CandidateTiles(c.latest.damage, w, h, 0)
	if !valid || len(ids) != 2 || ids[0] != 0 || ids[1] != 10 {
		t.Fatalf("queue caused full comparison: %v %v", ids, valid)
	}
	if b.damage.Previous != 1 || len(b.damage.Rects) != 1 || d.damage.Previous != 2 {
		t.Fatal("producer metadata mutated")
	}
	queued := c.latest
	c.latest = nil
	c.hasLatest = false
	c.sendFrame(queued)
	queued.release()
	if got := readJPEGWire(t, conn, encJPEG); len(got) != 2 {
		t.Fatalf("missing queued updates: %v", got)
	}
	// An invalid or absent intermediate update must poison all replacements.
	for _, missing := range []bool{false, true} {
		bad := frame(4, 5)
		bad.damage.Valid = false
		if missing {
			bad.damage = nil
		}
		c.offerFrame(bad)
		c.offerFrame(frame(5, 6))
		if c.latest.damage != nil && c.latest.damage.Valid {
			t.Fatal("invalid intermediate update hidden")
		}
		c.latest.release()
		c.latest = nil
	}
	// Geometry change and large accumulated coverage also force full comparison.
	old := frame(4, 5, image.Rect(0, 0, 192, 256))
	c.offerFrame(old)
	c.offerFrame(frame(5, 6, image.Rect(192, 0, 256, 256)))
	if c.latest.damage.Valid {
		t.Fatal("full-screen accumulation accepted")
	}
	c.latest.release()
	c.latest = nil
	c.offerFrame(frame(4, 5))
	resized := frame(5, 6)
	resized.width = 128
	c.offerFrame(resized)
	if c.latest.damage.Valid {
		t.Fatal("resized queued frame accepted")
	}
	c.latest.release()
	c.latest = nil
}
