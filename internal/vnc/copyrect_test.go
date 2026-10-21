package vnc

import (
	"bytes"
	"encoding/binary"
	"image"
	"testing"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/pixelops"
)

// Apply the actual mixed RFB update to a simulated viewer, including CopyRect
// ordering and memmove semantics, rather than only checking planner results.
func applyCopyWire(t *testing.T, conn *jpegWireConn, viewer []byte, w int) []int32 {
	t.Helper()
	wire := conn.wire.Bytes()
	defer conn.wire.Reset()
	if len(wire) < 4 {
		t.Fatal("missing update")
	}
	n := int(binary.BigEndian.Uint16(wire[2:]))
	wire = wire[4:]
	var encs []int32
	for i := 0; i < n; i++ {
		if len(wire) < 12 {
			t.Fatal("truncated rectangle")
		}
		r := rect{x: binary.BigEndian.Uint16(wire), y: binary.BigEndian.Uint16(wire[2:]), width: binary.BigEndian.Uint16(wire[4:]), height: binary.BigEndian.Uint16(wire[6:])}
		enc := int32(binary.BigEndian.Uint32(wire[8:]))
		wire = wire[12:]
		encs = append(encs, enc)
		switch enc {
		case encCopyRect:
			if len(wire) < 4 {
				t.Fatal("truncated copy")
			}
			decodeCopyRectClient(viewer, w, r, wire[:4])
			wire = wire[4:]
		case encRaw:
			size := int(r.width) * int(r.height) * 4
			if len(wire) < size {
				t.Fatal("truncated raw")
			}
			decodeRawClient(viewer, w, r, wire[:size], defaultPixelFormat())
			wire = wire[size:]
		case encJPEG, encTight:
			if enc == encTight && len(wire) >= 4 && wire[0] == tightFill {
				for y := int(r.y); y < int(r.y+r.height); y++ {
					for x := int(r.x); x < int(r.x+r.width); x++ {
						i := (y*w + x) * 4
						viewer[i], viewer[i+1], viewer[i+2], viewer[i+3] = wire[3], wire[2], wire[1], 255
					}
				}
				wire = wire[4:]
				continue
			}
			size := 0
			if enc == encJPEG {
				size = bytes.Index(wire, []byte{255, 217}) + 2
			} else {
				if len(wire) == 0 || wire[0] != tightJPEG {
					t.Fatal("expected tight JPEG")
				}
				wire = wire[1:]
				for j := 0; j < 3; j++ {
					v := wire[0]
					wire = wire[1:]
					if j == 2 {
						size |= int(v) << 14
					} else {
						size |= int(v&127) << (j * 7)
					}
					if v&128 == 0 || j == 2 {
						break
					}
				}
			}
			if size < 2 || size > len(wire) {
				t.Fatal("invalid JPEG payload")
			}
			pixels, iw, ih, err := jpegcodec.DecodeBGRA(wire[:size])
			if err != nil || iw != int(r.width) || ih != int(r.height) {
				t.Fatal("decode JPEG", err)
			}
			decodeRawClient(viewer, w, r, pixels, defaultPixelFormat())
			wire = wire[size:]
		default:
			t.Fatalf("unexpected encoding %d", enc)
		}
	}
	if len(wire) != 0 {
		t.Fatal("trailing wire data")
	}
	return encs
}

func rawMoveFrame(prev, seq uint64, w, h int, pixels []byte, moves ...jpegcodec.DesktopMove) *frameData {
	return &frameData{format: capture.FormatRaw, width: uint16(w), height: uint16(h), data: pixels,
		damage: &jpegcodec.DesktopDamage{Stream: 1, Previous: prev, Sequence: seq, SourceWidth: w, SourceHeight: h, Valid: true, Moves: moves}}
}
func containsCopy(encs []int32) bool {
	for _, enc := range encs {
		if enc == encCopyRect {
			return true
		}
	}
	return false
}

func TestCopyRectRawDelivery(t *testing.T) {
	for _, move := range []jpegcodec.DesktopMove{
		{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)},
		{Source: image.Rect(0, 0, 100, 80), Destination: image.Rect(7, 9, 107, 89)}, // self overlap down/right
		{Source: image.Rect(7, 9, 107, 89), Destination: image.Rect(0, 0, 100, 80)}, // up/left
	} {
		const w, h = 192, 128
		c, conn := newJPEGWireClient(encRaw, w, h)
		c.encodings = append(c.encodings, encCopyRect)
		old := make([]byte, w*h*4)
		for i := range old {
			old[i] = byte((i*13 + i/11) % 251)
		}
		viewer := make([]byte, len(old))
		c.sendFrame(rawMoveFrame(0, 1, w, h, old))
		applyCopyWire(t, conn, viewer, w)
		next := copyPixelBaseline(old, w, []copyRectangle{{move.Source, move.Destination}})
		// An unrelated dirty area and the exposed source must follow the copy.
		next[(110*w+180)*4] = 99
		f := rawMoveFrame(1, 2, w, h, next, move)
		f.damage.Rects = []image.Rectangle{image.Rect(180, 110, 181, 111)}
		before := bytes.Clone(c.prevFrame)
		c.sendFrame(f)
		if !containsCopy(applyCopyWire(t, conn, viewer, w)) {
			t.Fatal("continuous validated move did not use CopyRect")
		}
		if !pixelops.EqualBGR(viewer, next) || !pixelops.EqualBGR(c.prevFrame, next) {
			t.Fatal("viewer/baseline differs after copy and exposed pixels")
		}
		if bytes.Equal(before, c.prevFrame) {
			t.Fatal("copy baseline was not committed")
		}
		// A future change inside the copied region compares against the new baseline.
		after := bytes.Clone(next)
		after[(move.Destination.Min.Y*w+move.Destination.Min.X)*4]++
		follow := rawMoveFrame(2, 3, w, h, after)
		follow.damage.Rects = []image.Rectangle{move.Destination}
		c.sendFrame(follow)
		applyCopyWire(t, conn, viewer, w)
		if !pixelops.EqualBGR(viewer, after) {
			t.Fatal("subsequent update lost")
		}
	}
}

func TestCopyRectRawFallback(t *testing.T) {
	for _, kind := range []string{"unsupported", "gap", "full", "mismatch", "lossy-baseline", "cycle", "queue-drop"} {
		t.Run(kind, func(t *testing.T) {
			const w, h = 192, 128
			c, conn := newJPEGWireClient(encRaw, w, h)
			c.encodings = append(c.encodings, encCopyRect)
			old := make([]byte, w*h*4)
			for i := range old {
				old[i] = byte(i % 241)
			}
			viewer := make([]byte, len(old))
			c.sendFrame(rawMoveFrame(0, 1, w, h, old))
			applyCopyWire(t, conn, viewer, w)
			move := jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 0, 128, 64)}
			next := copyPixelBaseline(old, w, []copyRectangle{{move.Source, move.Destination}})
			f := rawMoveFrame(1, 2, w, h, next, move)
			switch kind {
			case "unsupported":
				c.encodings = []int32{encRaw}
			case "gap":
				f.damage.Previous = 2
				f.damage.Sequence = 3
			case "full":
				c.needFull = true
			case "mismatch":
				next[64*4]++
			case "lossy-baseline":
				c.rawCopySafe = false
			case "cycle":
				reverse := jpegcodec.DesktopMove{Source: move.Destination, Destination: move.Source}
				next = copyPixelBaseline(old, w, []copyRectangle{{move.Source, move.Destination}, {reverse.Source, reverse.Destination}})
				f = rawMoveFrame(1, 2, w, h, next, move, reverse)
			case "queue-drop":
				c.offerFrame(f)
				last := rawMoveFrame(2, 3, w, h, next)
				c.offerFrame(last)
				f = c.latest
				c.latest = nil
				defer f.release()
				if f.damage.Valid && (!f.damage.Coalesced || len(f.damage.Moves) != 0 || len(f.damage.Rects) == 0) {
					t.Fatal("dropped move not converted to damage")
				}
			}
			c.sendFrame(f)
			if containsCopy(applyCopyWire(t, conn, viewer, w)) {
				t.Fatal("unsafe copy was emitted")
			}
			if !pixelops.EqualBGR(viewer, next) {
				t.Fatal("fallback lost pixels")
			}
		})
	}
}

// Build a synthetic GPU snapshot by moving independent MCU entropy segments.
// The wire still contains real, independently decodable baseline JPEG data.
func movedRestartFixture(t *testing.T, data []byte, move jpegcodec.DesktopMove) *jpegcodec.RestartJPEG {
	t.Helper()
	scan := bytes.Index(data, []byte{255, 218})
	if scan < 0 {
		t.Fatal("missing scan")
	}
	headerEnd := scan + 2 + int(binary.BigEndian.Uint16(data[scan+2:]))
	var segments [][]byte
	start := headerEnd
	for pos := start; pos+1 < len(data); pos++ {
		if data[pos] != 255 || data[pos+1] == 0 {
			continue
		}
		marker := data[pos+1]
		if marker >= 208 && marker <= 215 || marker == 217 {
			segments = append(segments, data[start:pos])
			pos++
			start = pos + 1
		}
	}
	original, err := jpegcodec.ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	current := append([][]byte(nil), segments...)
	cols := (original.Width + 15) / 16
	for y := 0; y < move.Source.Dy()/16; y++ {
		for x := 0; x < move.Source.Dx()/16; x++ {
			current[(move.Destination.Min.Y/16+y)*cols+move.Destination.Min.X/16+x] = segments[(move.Source.Min.Y/16+y)*cols+move.Source.Min.X/16+x]
		}
	}
	wire := bytes.Clone(data[:headerEnd])
	for i, segment := range current {
		if i > 0 {
			wire = append(wire, 255, byte(208+(i-1)%8))
		}
		wire = append(wire, segment...)
	}
	wire = append(wire, 255, 217)
	result, err := jpegcodec.ParseRestartJPEG(wire)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCopyRectGPUJPEGDelivery(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			const w, h = 193, 131
			c, conn := newJPEGWireClient(enc, w, h)
			c.encodings = append(c.encodings, encCopyRect)
			original, err := jpegcodec.ParseRestartJPEG(restartWireFixture(t, "base"))
			if err != nil {
				t.Fatal(err)
			}
			base := &frameData{jpegRegions: original, format: capture.FormatJPEG, width: w, height: h, damage: &jpegcodec.DesktopDamage{Stream: 1, Sequence: 1, SourceWidth: w, SourceHeight: h, Valid: true}}
			viewer := make([]byte, w*h*4)
			c.sendFrame(base)
			applyCopyWire(t, conn, viewer, w)
			move := jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)}
			current := movedRestartFixture(t, restartWireFixture(t, "base"), move)
			want := copyPixelBaseline(viewer, w, []copyRectangle{{move.Source, move.Destination}})
			frame := &frameData{jpegRegions: current, format: capture.FormatJPEG, width: w, height: h, damage: &jpegcodec.DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: w, SourceHeight: h, Valid: true, Moves: []jpegcodec.DesktopMove{move}}}
			c.sendFrame(frame)
			encs := applyCopyWire(t, conn, viewer, w)
			if len(encs) != 1 || encs[0] != encCopyRect {
				t.Fatalf("expected pure CopyRect, got %v", encs)
			}
			if !bytes.Equal(viewer, want) || c.jpegPrevious.restart != current {
				t.Fatal("viewer or compressed baseline not advanced")
			}
			// No redundant JPEG after a successful copy.
			next := *frame
			d := *frame.damage
			d.Previous, d.Sequence, d.Moves = 2, 3, nil
			next.damage = &d
			c.sendFrame(&next)
			if conn.wire.Len() != 0 {
				t.Fatal("copy target was resent")
			}
		})
	}
}

func TestCopyRectGPUQueueDrop(t *testing.T) {
	const w, h = 193, 131
	c, conn := newJPEGWireClient(encJPEG, w, h)
	c.encodings = append(c.encodings, encCopyRect)
	data := restartWireFixture(t, "base")
	source, err := jpegcodec.ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	initial := &frameData{jpegRegions: source, format: capture.FormatJPEG, width: w, height: h, damage: &jpegcodec.DesktopDamage{Stream: 1, Sequence: 1, SourceWidth: w, SourceHeight: h, Valid: true}}
	viewer := make([]byte, w*h*4)
	c.sendFrame(initial)
	applyCopyWire(t, conn, viewer, w)
	move := jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)}
	moved := movedRestartFixture(t, data, move)
	middle := *initial
	middle.jpegRegions = moved
	middle.damage = &jpegcodec.DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: w, SourceHeight: h, Valid: true, Moves: []jpegcodec.DesktopMove{move}}
	c.offerFrame(&middle)
	last := middle
	last.damage = &jpegcodec.DesktopDamage{Stream: 1, Previous: 2, Sequence: 3, SourceWidth: w, SourceHeight: h, Valid: true}
	c.offerFrame(&last)
	queued := c.latest
	c.latest = nil
	defer queued.release()
	if !queued.damage.Coalesced || len(queued.damage.Moves) != 0 || len(queued.damage.Rects) == 0 {
		t.Fatal("GPU queue did not retain move endpoint damage")
	}
	c.sendFrame(queued)
	if encs := applyCopyWire(t, conn, viewer, w); len(encs) == 0 || containsCopy(encs) {
		t.Fatalf("dropped GPU move replayed: %v", encs)
	}
}

func TestCopyRectFailedWritePreservesBaseline(t *testing.T) {
	const w, h = 192, 128
	c, conn := newJPEGWireClient(encRaw, w, h)
	c.encodings = append(c.encodings, encCopyRect)
	old := make([]byte, w*h*4)
	for i := range old {
		old[i] = byte(i % 251)
	}
	c.sendFrame(rawMoveFrame(0, 1, w, h, old))
	conn.wire.Reset()
	move := jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)}
	next := copyPixelBaseline(old, w, []copyRectangle{{move.Source, move.Destination}})
	conn.fail = true
	c.sendFrame(rawMoveFrame(1, 2, w, h, next, move))
	if !bytes.Equal(c.prevFrame, old) || !c.damageState.Continuous(rawMoveFrame(1, 2, w, h, next, move).damage) {
		t.Fatal("failed copy advanced baseline")
	}
}

func TestCopyRectRotatedScaledDelivery(t *testing.T) {
	cases := []struct {
		rotation, w, h      int
		source, destination image.Rectangle
	}{
		{0, 128, 64, image.Rect(0, 0, 32, 16), image.Rect(64, 32, 96, 48)},
		{90, 64, 128, image.Rect(48, 0, 64, 32), image.Rect(16, 64, 32, 96)},
		{180, 128, 64, image.Rect(96, 48, 128, 64), image.Rect(32, 16, 64, 32)},
		{270, 64, 128, image.Rect(0, 96, 16, 128), image.Rect(32, 32, 48, 64)},
	}
	for _, tc := range cases {
		c, conn := newJPEGWireClient(encRaw, uint16(tc.w), uint16(tc.h))
		c.encodings = append(c.encodings, encCopyRect)
		old := make([]byte, tc.w*tc.h*4)
		for i := range old {
			old[i] = byte((i + i/7) % 251)
		}
		viewer := make([]byte, len(old))
		c.sendFrame(rawMoveFrame(0, 1, tc.w, tc.h, old))
		applyCopyWire(t, conn, viewer, tc.w)
		metadata := &jpegcodec.DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: 256, SourceHeight: 128, Valid: true, Moves: []jpegcodec.DesktopMove{{Source: image.Rect(0, 0, 64, 32), Destination: image.Rect(128, 64, 192, 96)}}}
		mapped := jpegcodec.MapDesktopDamage(metadata, tc.w, tc.h, tc.rotation)
		if len(mapped.Moves) != 1 || mapped.Moves[0].Source != tc.source || mapped.Moves[0].Destination != tc.destination {
			t.Fatalf("rotation %d: %+v", tc.rotation, mapped.Moves)
		}
		next := copyPixelBaseline(old, tc.w, []copyRectangle{{tc.source, tc.destination}})
		frame := rawMoveFrame(1, 2, tc.w, tc.h, next)
		frame.damage = mapped
		c.sendFrame(frame)
		if !containsCopy(applyCopyWire(t, conn, viewer, tc.w)) || !pixelops.EqualBGR(viewer, next) {
			t.Fatalf("rotation %d copy failed", tc.rotation)
		}
	}
}

func TestCopyRectJPEGRejectsUnverifiedSource(t *testing.T) {
	const w, h = 193, 131
	c, conn := newJPEGWireClient(encJPEG, w, h)
	c.encodings = append(c.encodings, encCopyRect)
	data := restartWireFixture(t, "base")
	original, err := jpegcodec.ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	base := &frameData{jpegRegions: original, format: capture.FormatJPEG, width: w, height: h, damage: &jpegcodec.DesktopDamage{Stream: 1, Sequence: 1, SourceWidth: w, SourceHeight: h, Valid: true}}
	c.sendFrame(base)
	conn.wire.Reset()
	moved := movedRestartFixture(t, data, jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)})
	for _, move := range []jpegcodec.DesktopMove{
		{Source: image.Rect(16, 0, 80, 64), Destination: image.Rect(64, 64, 128, 128)}, // different MCU contents
		{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(65, 64, 129, 128)},  // unaligned
	} {
		next := *base
		next.jpegRegions = moved
		next.damage = &jpegcodec.DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: w, SourceHeight: h, Valid: true, Moves: []jpegcodec.DesktopMove{move}}
		if copies := c.planCopies(&next, false, encJPEG); len(copies) != 0 {
			t.Fatal("unverified compressed source accepted")
		}
	}
	// CPU-encoded JPEG pixels have no exact decoded-source mirror.
	raw := rawMoveFrame(1, 2, w, h, make([]byte, w*h*4), jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)})
	c.rawCopySafe = true
	c.prevW, c.prevH = w, h
	c.prevFrame = make([]byte, w*h*4)
	if len(c.planCopies(raw, false, encTight)) != 0 {
		t.Fatal("unverified CPU JPEG baseline copied")
	}
}
