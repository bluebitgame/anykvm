package jpegcodec

import "image"

// DesktopDamage is an advisory update from one desktop capture acquisition.
// Rects use source pixel coordinates and include both ends of moves.
// Previous/Sequence bound the acquisition interval covered by Rects. Queues may
// merge consecutive intervals when replacing a frame.
type DesktopDamage struct {
	Moves                      []DesktopMove
	Coalesced                  bool // A queue dropped an intermediate acquisition; moves cannot be replayed.
	Stream, Previous, Sequence uint64
	SourceWidth, SourceHeight  int
	Valid                      bool
	Rects                      []image.Rectangle
}

type desktopDamageState struct {
	stream, sequence uint64
	valid            bool
}

func (s *desktopDamageState) commit(d *DesktopDamage) {
	s.valid = d != nil && d.Stream != 0 && d.Sequence != 0
	if s.valid {
		s.stream, s.sequence = d.Stream, d.Sequence
	}
}

// candidateTiles returns a sorted, unique superset of affected output tiles.
// Filtering is safe only when this encoder consumed the immediately preceding
// interval from the same capture session. An empty valid result means
// no comparison dispatch; false means compare the whole frame.
func (s desktopDamageState) candidateTiles(d *DesktopDamage, width, height, rotation int) ([]uint32, bool) {
	if d == nil || !d.Valid || !s.valid || d.Stream != s.stream || d.Previous != s.sequence ||
		d.Sequence < d.Previous || d.Sequence == 0 || d.SourceWidth < 1 || d.SourceHeight < 1 ||
		d.SourceWidth > 16384 || d.SourceHeight > 16384 || width < 1 || height < 1 || width > 8192 || height > 8192 ||
		(rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270) {
		return nil, false
	}
	sw, sh := d.SourceWidth, d.SourceHeight
	dw, dh := sw, sh
	if rotation == 90 || rotation == 270 {
		dw, dh = sh, sw
	}
	cols, rows := (width+63)/64, (height+63)/64
	tiles := make([]bool, cols*rows)
	for _, r := range damageRectangles(d) {
		if r.Empty() || r.Min.X < 0 || r.Min.Y < 0 || r.Max.X > sw || r.Max.Y > sh {
			return nil, false
		}
		switch rotation {
		case 90:
			r = image.Rect(sh-r.Max.Y, r.Min.X, sh-r.Min.Y, r.Max.X)
		case 180:
			r = image.Rect(sw-r.Max.X, sh-r.Max.Y, sw-r.Min.X, sh-r.Min.Y)
		case 270:
			r = image.Rect(r.Min.Y, sw-r.Max.X, r.Max.Y, sw-r.Min.X)
		}
		// Outward scaling plus a one-pixel halo covers nearest-neighbor sample
		// centers and shader float rounding, including upscaled and clipped edges.
		x0 := max(0, int(int64(r.Min.X)*int64(width)/int64(dw))-1)
		y0 := max(0, int(int64(r.Min.Y)*int64(height)/int64(dh))-1)
		x1 := min(width, int((int64(r.Max.X)*int64(width)+int64(dw)-1)/int64(dw))+1)
		y1 := min(height, int((int64(r.Max.Y)*int64(height)+int64(dh)-1)/int64(dh))+1)
		if width == dw {
			x0, x1 = r.Min.X, r.Max.X
		}
		if height == dh {
			y0, y1 = r.Min.Y, r.Max.Y
		}
		for y := y0 / 64; y < (y1+63)/64; y++ {
			for x := x0 / 64; x < (x1+63)/64; x++ {
				tiles[y*cols+x] = true
			}
		}
	}
	var result []uint32
	for i, selected := range tiles {
		if selected {
			result = append(result, uint32(i))
		}
	}
	return result, true
}

// DesktopDamageState tracks the last compared input independently of the last
// transmitted pixels. Missing or skipped inputs force a full comparison.
type DesktopDamageState struct{ state desktopDamageState }

func (s *DesktopDamageState) Commit(d *DesktopDamage) { s.state.commit(d) }
func (s DesktopDamageState) CandidateTiles(d *DesktopDamage, width, height, rotation int) ([]uint32, bool) {
	ids, valid := s.state.candidateTiles(d, width, height, rotation)
	if valid && damageNearFull(ids, width, height) {
		return nil, false
	}
	return ids, valid
}

// MergeDesktopDamage carries updates from a replaced queued frame into its
// successor. Neither input is modified. Invalid intervals stay invalid until
// consumed by a full comparison; a later valid frame cannot hide a lost update.
func MergeDesktopDamage(previous, next *DesktopDamage) *DesktopDamage {
	if next == nil {
		return nil
	}
	merged := *next
	merged.Moves, merged.Coalesced = nil, true
	merged.Rects = nil
	merged.Valid = false
	if previous == nil || !previous.Valid || !next.Valid ||
		previous.Stream == 0 || previous.Sequence == 0 || previous.Sequence < previous.Previous || next.Sequence < next.Previous ||
		previous.Stream != next.Stream || previous.Sequence != next.Previous ||
		previous.SourceWidth != next.SourceWidth || previous.SourceHeight != next.SourceHeight {
		return &merged
	}
	merged.Previous = previous.Previous
	merged.Valid = true
	merged.Rects = append(append([]image.Rectangle(nil), damageRectangles(previous)...), damageRectangles(next)...)
	state := desktopDamageState{stream: merged.Stream, sequence: merged.Previous, valid: true}
	ids, valid := state.candidateTiles(&merged, merged.SourceWidth, merged.SourceHeight, 0)
	merged.Rects = nil
	if !valid || damageNearFull(ids, merged.SourceWidth, merged.SourceHeight) {
		merged.Valid = false
		return &merged
	}
	// Canonical row runs bound metadata by the source tile grid rather than by
	// the number of frames dropped. Mapping again after scaling is conservative.
	merged.Rects = damageTileRects(ids, merged.SourceWidth, merged.SourceHeight)
	return &merged
}

// damageTileRects canonicalizes sorted unique IDs into bounded tile-row runs.
func damageTileRects(ids []uint32, width, height int) []image.Rectangle {
	cols := (width + 63) / 64
	var rects []image.Rectangle
	for i := 0; i < len(ids); {
		start, end := int(ids[i]), int(ids[i])+1
		i++
		for i < len(ids) && int(ids[i]) == end && int(ids[i])/cols == start/cols {
			end++
			i++
		}
		rects = append(rects, image.Rect((start%cols)*64, (start/cols)*64, min(width, ((end-1)%cols+1)*64), min(height, (start/cols+1)*64)))
	}
	return rects
}

// Use pixel area, including clipped edge tiles, rather than rectangle area or
// summed overlaps. At 80% coverage a full comparison avoids filtering overhead.
func damageNearFull(ids []uint32, width, height int) bool {
	if width <= 0 || height <= 0 {
		return true
	}
	cols := (width + 63) / 64
	area := int64(0)
	for _, id := range ids {
		x, y := (int(id)%cols)*64, (int(id)/cols)*64
		area += int64(min(64, width-x)) * int64(min(64, height-y))
	}
	return area*5 >= int64(width)*int64(height)*4
}

// DesktopDamageAccumulator bridges acquired pictures that a capture/encoding
// stage drops before producing output. Call Consumed only after an output was
// produced; network queues track their own immutable damage intervals.
type DesktopDamageAccumulator struct {
	pending     *DesktopDamage
	havePending bool
	rotation    int
}

func (a *DesktopDamageAccumulator) Add(d *DesktopDamage, rotation int) *DesktopDamage {
	if a.havePending {
		d = MergeDesktopDamage(a.pending, d)
		if rotation != a.rotation && d != nil {
			invalid := *d
			invalid.Valid, invalid.Rects = false, nil
			d = &invalid
		}
	}
	a.pending, a.havePending, a.rotation = d, true, rotation
	return d
}
func (a *DesktopDamageAccumulator) Consumed() { *a = DesktopDamageAccumulator{} }

// MapDesktopDamage normalizes source damage to a raw output picture after the
// same rotation/scaling used by capture. It preserves acquisition identity.
func MapDesktopDamage(d *DesktopDamage, width, height, rotation int) *DesktopDamage {
	if d == nil {
		return nil
	}
	mapped := *d
	mapped.SourceWidth, mapped.SourceHeight = width, height
	mapped.Rects, mapped.Valid = nil, false
	mapped.Moves = nil
	state := DesktopDamageState{state: desktopDamageState{stream: d.Stream, sequence: d.Previous, valid: d.Stream != 0}}
	ids, valid := state.state.candidateTiles(d, width, height, rotation)
	if valid {
		mapped.Valid = true
		mapped.Rects = damageTileRects(ids, width, height)
		if !d.Coalesced {
			mapped.Moves = mapDesktopMoves(d, width, height, rotation)
		}
	}
	return &mapped
}

// DesktopMove copies unchanged pixels from the preceding acquisition.
type DesktopMove struct{ Source, Destination image.Rectangle }

// Continuous permits move reuse only across a single, unmerged acquisition.
// Area thresholds affect comparison dispatch, not the safety of a copy.
func (s DesktopDamageState) Continuous(d *DesktopDamage) bool {
	return d != nil && d.Valid && !d.Coalesced && s.state.valid && d.Stream == s.state.stream && d.Previous == s.state.sequence && d.Sequence > d.Previous
}

func mapDesktopMoves(d *DesktopDamage, width, height, rotation int) []DesktopMove {
	sw, sh := d.SourceWidth, d.SourceHeight
	if sw < 1 || sh < 1 {
		return nil
	}
	dw, dh := sw, sh
	if rotation == 90 || rotation == 270 {
		dw, dh = sh, sw
	}
	transform := func(r image.Rectangle) (image.Rectangle, bool) {
		if r.Empty() || !r.In(image.Rect(0, 0, sw, sh)) {
			return image.Rectangle{}, false
		}
		switch rotation {
		case 90:
			r = image.Rect(sh-r.Max.Y, r.Min.X, sh-r.Min.Y, r.Max.X)
		case 180:
			r = image.Rect(sw-r.Max.X, sh-r.Max.Y, sw-r.Min.X, sh-r.Min.Y)
		case 270:
			r = image.Rect(r.Min.Y, sw-r.Max.X, r.Max.Y, sw-r.Min.X)
		}
		if int64(r.Min.X)*int64(width)%int64(dw) != 0 || int64(r.Max.X)*int64(width)%int64(dw) != 0 || int64(r.Min.Y)*int64(height)%int64(dh) != 0 || int64(r.Max.Y)*int64(height)%int64(dh) != 0 {
			return image.Rectangle{}, false
		}
		return image.Rect(r.Min.X*width/dw, r.Min.Y*height/dh, r.Max.X*width/dw, r.Max.Y*height/dh), true
	}
	var moves []DesktopMove
	for _, m := range d.Moves {
		src, a := transform(m.Source)
		dst, b := transform(m.Destination)
		if !a || !b || src.Size() != dst.Size() {
			continue
		}
		moves = append(moves, DesktopMove{src, dst})
	}
	return moves
}

// Move endpoints are always comparison candidates, even after moves are dropped.
func damageRectangles(d *DesktopDamage) []image.Rectangle {
	if len(d.Moves) == 0 {
		return d.Rects
	}
	rects := append([]image.Rectangle(nil), d.Rects...)
	for _, move := range d.Moves {
		rects = append(rects, move.Source, move.Destination)
	}
	return rects
}
