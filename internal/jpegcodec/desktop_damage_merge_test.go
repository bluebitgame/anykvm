package jpegcodec

import (
	"image"
	"reflect"
	"testing"
)

func TestMergeDesktopDamage(t *testing.T) {
	makeDamage := func(prev, seq uint64, rects ...image.Rectangle) *DesktopDamage {
		return &DesktopDamage{Stream: 1, Previous: prev, Sequence: seq, SourceWidth: 256, SourceHeight: 256, Valid: true, Rects: rects}
	}
	a := makeDamage(10, 11, image.Rect(1, 1, 2, 2))
	b := makeDamage(11, 12, image.Rect(2, 2, 3, 3), image.Rect(130, 130, 131, 131))
	merged := MergeDesktopDamage(a, b)
	state := DesktopDamageState{}
	state.Commit(makeDamage(9, 10))
	ids, valid := state.CandidateTiles(merged, 256, 256, 0)
	if !valid || !reflect.DeepEqual(ids, []uint32{0, 10}) || merged.Previous != 10 || merged.Sequence != 12 {
		t.Fatalf("merged=%+v ids=%v valid=%v", merged, ids, valid)
	}
	if a.Previous != 10 || len(a.Rects) != 1 || b.Previous != 11 || len(b.Rects) != 2 {
		t.Fatal("inputs mutated")
	}
	// Replacing thousands of frames must not grow an unbounded rectangle list.
	for seq := uint64(13); seq < 2013; seq++ {
		merged = MergeDesktopDamage(merged, makeDamage(seq-1, seq, image.Rect(1, 1, 2, 2)))
		if !merged.Valid || len(merged.Rects) != 2 {
			t.Fatalf("unbounded/invalid merge at %d: %+v", seq, merged)
		}
	}
	for name, mutate := range map[string]func(*DesktopDamage){
		"invalid":   func(d *DesktopDamage) { d.Valid = false },
		"gap":       func(d *DesktopDamage) { d.Previous++ },
		"stream":    func(d *DesktopDamage) { d.Stream++ },
		"size":      func(d *DesktopDamage) { d.SourceWidth++ },
		"backwards": func(d *DesktopDamage) { d.Sequence = 10 },
		"bounds":    func(d *DesktopDamage) { d.Rects = []image.Rectangle{image.Rect(-1, 0, 2, 2)} },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *b
			mutate(&bad)
			got := MergeDesktopDamage(a, &bad)
			if got.Valid {
				t.Fatalf("unsafe merge: %+v", got)
			}
			next := makeDamage(got.Sequence, got.Sequence+1)
			if MergeDesktopDamage(got, next).Valid {
				t.Fatal("invalid interval lost on replacement")
			}
		})
	}
	if MergeDesktopDamage(nil, b).Valid || MergeDesktopDamage(a, nil) != nil {
		t.Fatal("missing metadata accepted")
	}
	// A valid empty successor retains the accumulated region.
	empty := makeDamage(11, 12)
	if got := MergeDesktopDamage(a, empty); !got.Valid || len(got.Rects) != 1 {
		t.Fatalf("empty successor: %+v", got)
	}
}

func TestDesktopDamageNearFull(t *testing.T) {
	state := DesktopDamageState{}
	state.Commit(&DesktopDamage{Stream: 1, Sequence: 1})
	d := &DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: 320, SourceHeight: 64, Valid: true}
	d.Rects = []image.Rectangle{image.Rect(0, 0, 192, 64), image.Rect(0, 0, 192, 64)}
	if ids, valid := state.CandidateTiles(d, 320, 64, 0); !valid || len(ids) != 3 {
		t.Fatal("overlaps counted more than once")
	}
	d.Rects = append(d.Rects, image.Rect(192, 0, 256, 64))
	if _, valid := state.CandidateTiles(d, 320, 64, 0); valid {
		t.Fatal("80 percent did not fall back")
	}
	a := *d
	a.Sequence = 1
	a.Previous = 0
	a.Rects = d.Rects[:1]
	b := *d
	b.Rects = d.Rects[2:]
	if MergeDesktopDamage(&a, &b).Valid {
		t.Fatal("merged near-full area did not fall back")
	}
	// Clipped edge tiles count their actual area, not a complete 64px tile.
	d.SourceWidth = 257
	d.Rects = []image.Rectangle{image.Rect(0, 0, 192, 64), image.Rect(256, 0, 257, 64)}
	if _, valid := state.CandidateTiles(d, 257, 64, 0); !valid {
		t.Fatal("partial edge inflated coverage")
	}
}

func TestDesktopDamageAcquiredDrops(t *testing.T) {
	var pending DesktopDamageAccumulator
	mk := func(prev, seq uint64, rect image.Rectangle) *DesktopDamage {
		d := &DesktopDamage{Stream: 11, Previous: prev, Sequence: seq, SourceWidth: 256, SourceHeight: 256, Valid: true}
		if !rect.Empty() {
			d.Rects = []image.Rectangle{rect}
		}
		return d
	}
	// These represent successive DXGI acquisitions, including both ends of a
	// move. The first two acquired inputs never produced an encoded/raw output.
	pending.Add(mk(1, 2, image.Rect(0, 0, 64, 64)), 0)
	move := mk(2, 3, image.Rect(128, 128, 192, 192))
	move.Rects = append(move.Rects, image.Rect(0, 0, 64, 64))
	pending.Add(move, 0)
	got := pending.Add(mk(3, 4, image.Rectangle{}), 0)
	state := DesktopDamageState{}
	state.Commit(mk(0, 1, image.Rectangle{}))
	ids, valid := state.CandidateTiles(got, 256, 256, 0)
	if !valid || !reflect.DeepEqual(ids, []uint32{0, 10}) || got.Previous != 1 || got.Sequence != 4 {
		t.Fatalf("acquired drops: %+v %v %v", got, ids, valid)
	}
	// Raw output carries normalized metadata into the VNC queue, where another
	// dropped picture adds a disjoint tile without losing the acquired interval.
	mapped := MapDesktopDamage(got, 256, 256, 0)
	next := mk(4, 5, image.Rect(192, 0, 256, 64))
	queued := MergeDesktopDamage(mapped, MapDesktopDamage(next, 256, 256, 0))
	if ids, ok := state.CandidateTiles(queued, 256, 256, 0); !ok || !reflect.DeepEqual(ids, []uint32{0, 3, 10}) {
		t.Fatalf("two queue stages: %v %v", ids, ok)
	}
	pending.Consumed()
	if single := pending.Add(next, 0); single != next {
		t.Fatal("consumed range was merged again")
	}
	pending.Consumed()
	bad := mk(1, 2, image.Rectangle{})
	bad.Valid = false
	pending.Add(bad, 0)
	if pending.Add(mk(2, 3, image.Rectangle{}), 0).Valid {
		t.Fatal("invalid acquisition hidden")
	}
	pending.Consumed()
	pending.Add(mk(1, 2, image.Rectangle{}), 0)
	if pending.Add(mk(2, 3, image.Rectangle{}), 180).Valid {
		t.Fatal("rotation change accepted")
	}
	if pending.Add(mk(3, 4, image.Rectangle{}), 180).Valid {
		t.Fatal("geometry transition hidden by later input")
	}
	pending.Consumed()
	pending.Add(mk(1, 2, image.Rectangle{}), 0)
	if pending.Add(mk(3, 4, image.Rectangle{}), 0).Valid {
		t.Fatal("unknown acquisition gap accepted")
	}
}

func TestMapDesktopDamageRotationScaling(t *testing.T) {
	d := &DesktopDamage{Stream: 7, Previous: 1, Sequence: 3, SourceWidth: 256, SourceHeight: 128, Valid: true, Rects: []image.Rectangle{image.Rect(0, 0, 64, 64)}}
	for _, rotation := range []int{0, 90, 180, 270} {
		for _, size := range []image.Point{{256, 256}, {128, 64}, {65, 131}} {
			state := DesktopDamageState{}
			state.Commit(&DesktopDamage{Stream: 7, Sequence: 1})
			want, valid := state.CandidateTiles(d, size.X, size.Y, rotation)
			mapped := MapDesktopDamage(d, size.X, size.Y, rotation)
			got, ok := state.CandidateTiles(mapped, size.X, size.Y, 0)
			if ok != valid || !reflect.DeepEqual(got, want) || mapped.Previous != 1 || mapped.Sequence != 3 || mapped.SourceWidth != size.X || mapped.SourceHeight != size.Y {
				t.Fatalf("rotation=%d output=%v want=%v/%v got=%v/%v", rotation, size, want, valid, got, ok)
			}
		}
	}
	if d.SourceWidth != 256 || d.Rects[0] != image.Rect(0, 0, 64, 64) {
		t.Fatal("mapping mutated source")
	}
}

func TestDesktopMoveMappingAndDrop(t *testing.T) {
	d := &DesktopDamage{Stream: 1, Previous: 1, Sequence: 2, SourceWidth: 256, SourceHeight: 128, Valid: true, Moves: []DesktopMove{{Source: image.Rect(0, 0, 64, 32), Destination: image.Rect(128, 64, 192, 96)}}}
	for _, rotation := range []int{0, 90, 180, 270} {
		w, h := 128, 64
		if rotation == 90 || rotation == 270 {
			w, h = h, w
		}
		mapped := MapDesktopDamage(d, w, h, rotation)
		if !mapped.Valid || len(mapped.Moves) != 1 || mapped.Moves[0].Source.Size() != mapped.Moves[0].Destination.Size() {
			t.Fatalf("rotation %d: %+v", rotation, mapped)
		}
		src, dst := mapped.Moves[0].Source, mapped.Moves[0].Destination
		// Source and destination stay covered even without explicit dirty Rects.
		state := DesktopDamageState{}
		state.Commit(&DesktopDamage{Stream: 1, Sequence: 1})
		ids, valid := state.CandidateTiles(mapped, w, h, 0)
		if valid && len(ids) == 0 || !src.In(image.Rect(0, 0, w, h)) || !dst.In(image.Rect(0, 0, w, h)) {
			t.Fatal("mapped endpoints lost")
		}
	}
	fractional := *d
	fractional.Moves = []DesktopMove{{Source: image.Rect(1, 0, 65, 32), Destination: image.Rect(128, 64, 192, 96)}}
	if got := MapDesktopDamage(&fractional, 128, 64, 0); len(got.Moves) != 0 || len(got.Rects) == 0 {
		t.Fatal("fractional scaling not downgraded to dirty rectangles")
	}
	next := *d
	next.Previous, next.Sequence = 2, 3
	merged := MergeDesktopDamage(d, &next)
	if !merged.Coalesced || len(merged.Moves) != 0 || len(merged.Rects) == 0 {
		t.Fatal("drop replayed move or lost endpoints")
	}
}
