package rdp

import (
	"image"
	"sort"

	"anykvm/internal/bridge"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/pixelops"
)

// A dedicated unmapped surface snapshots the old client pixels before any move.
// This gives self-overlap, interdependent moves and cycles identical semantics.
const progressiveMoveScratch = 0xfffe

func graphicsSurfaceCopy(source, destination int, r image.Rectangle, point image.Point) []byte {
	return gfxPDU(5, join(u16(source), u16(destination), u16(r.Min.X), u16(r.Min.Y), u16(r.Max.X), u16(r.Max.Y), u16(1), u16(point.X), u16(point.Y)))
}

func mergeProgressiveDamage(old, next bridge.Frame) bridge.Frame {
	if predictionCoded(old.Format) || predictionCoded(next.Format) {
		return next
	}
	if old.Width == next.Width && old.Height == next.Height && old.Format == next.Format {
		next.Damage = jpegcodec.MergeDesktopDamage(old.Damage, next.Damage)
	} else if next.Damage != nil {
		d := *next.Damage
		d.Valid = false
		d.Moves = nil
		next.Damage = &d
	}
	return next
}

func fittedProgressiveDamage(d *jpegcodec.DesktopDamage, w, h int) *jpegcodec.DesktopDamage {
	if d == nil {
		return nil
	}
	if d.SourceWidth < 1 || d.SourceHeight < 1 {
		return nil
	}
	fit := fittedFrameRect(d.SourceWidth, d.SourceHeight, w, h, false)
	mapped := jpegcodec.MapDesktopDamage(d, fit.Dx(), fit.Dy(), 0)
	mapped.SourceWidth, mapped.SourceHeight = w, h
	offset := fit.Min
	for i := range mapped.Moves {
		mapped.Moves[i].Source = mapped.Moves[i].Source.Add(offset)
		mapped.Moves[i].Destination = mapped.Moves[i].Destination.Add(offset)
	}
	for i := range mapped.Rects {
		mapped.Rects[i] = mapped.Rects[i].Add(offset)
	}
	return mapped
}

// Surface copies move pixels, not Progressive contexts. Finish pending source
// tiles from their old coefficient cache before snapshotting the client surface.
// Select only verified copies, and deduplicate unaligned/overlapping sources.
func (e *progressiveEncoder) moveSourceRefinements(moves []gpuconv.SurfaceMove, w, h int) []bitmapRect {
	needed := make([]bool, len(e.pending))
	cols := (w + 63) / 64
	for _, m := range moves {
		for y := m.SourceY / 64; y < (m.SourceY+m.Height+63)/64; y++ {
			for x := m.SourceX / 64; x < (m.SourceX+m.Width+63)/64; x++ {
				i := y*cols + x
				if i < len(e.pending) && e.pending[i] {
					needed[i] = true
				}
			}
		}
	}
	var rects []bitmapRect
	for i, needed := range needed {
		if needed {
			x, y := i%cols*64, i/cols*64
			rects = append(rects, bitmapRect{x, y, min(64, w-x), min(64, h-y)})
		}
	}
	return rects
}

func (e *progressiveEncoder) moveCandidates(d *jpegcodec.DesktopDamage, state jpegcodec.DesktopDamageState, w, h int, force bool) []gpuconv.SurfaceMove {
	if force || !state.Continuous(d) || d.SourceWidth != w || d.SourceHeight != h || len(d.Moves) > 64 {
		return nil
	}
	used := make([]bool, ((w+63)/64)*((h+63)/64))
	cols := (w + 63) / 64
	var moves []gpuconv.SurfaceMove
	bounds := image.Rect(0, 0, w, h)
	for _, m := range d.Moves {
		if m.Source.Empty() || m.Source.Size() != m.Destination.Size() || !m.Source.In(bounds) || !m.Destination.In(bounds) || m.Source == m.Destination {
			continue
		}
		// Partial destination tiles are encoded normally; copying a whole tile
		// avoids retaining stale residual/sign state on an uncovered tile fragment.
		for y := (m.Destination.Min.Y + 63) / 64 * 64; y+64 <= m.Destination.Max.Y; y += 64 {
			for x := (m.Destination.Min.X + 63) / 64 * 64; x+64 <= m.Destination.Max.X; x += 64 {
				sx, sy := m.Source.Min.X+x-m.Destination.Min.X, m.Source.Min.Y+y-m.Destination.Min.Y
				i := y/64*cols + x/64
				if !used[i] {
					used[i] = true
					moves = append(moves, gpuconv.SurfaceMove{SourceX: sx, SourceY: sy, DestinationX: x, DestinationY: y, Width: 64, Height: 64})
				}
			}
		}
	}
	return moves
}

func validatedPixelMoves(moves []gpuconv.SurfaceMove, previous, current []byte, w, h int, format progressivePixelFormat) []gpuconv.SurfaceMove {
	if format != progressiveBGRA || len(previous) != w*h*4 || len(current) != len(previous) {
		return nil
	}
	var out []gpuconv.SurfaceMove
	for _, m := range moves {
		if pixelops.EqualBGRBlock(previous[(m.SourceY*w+m.SourceX)*4:], current[(m.DestinationY*w+m.DestinationX)*4:], w*4, 64, 64) {
			out = append(out, m)
		}
	}
	return out
}

func copyProgressiveBaseline(previous []byte, w int, moves []gpuconv.SurfaceMove) []byte {
	out := append([]byte(nil), previous...)
	for _, m := range moves {
		for y := 0; y < 64; y++ {
			src := ((m.SourceY+y)*w + m.SourceX) * 4
			dst := ((m.DestinationY+y)*w + m.DestinationX) * 4
			copy(out[dst:dst+256], previous[src:src+256])
		}
	}
	return out
}

func (e *progressiveEncoder) invalidateMovedTiles(moves []gpuconv.SurfaceMove, w int) {
	for _, m := range moves {
		i := progressiveTileIndex(w, m.DestinationX, m.DestinationY)
		if i < len(e.pending) {
			e.pending[i] = false
			if i < len(e.stages) {
				e.stages[i] = 0
			}
		}
		start := i * progressiveTileCoefficients
		if start+progressiveTileCoefficients <= len(e.coefficients) {
			clear(e.coefficients[start : start+progressiveTileCoefficients])
		}
	}
}

// Reuse the shared GPU comparison shader, but compare each proposed destination
// against its source in the previous committed image. A dirty flag rejects it.
type progressiveSurfaceMoveCompute interface {
	PrepareSurfaceMoves(*gpuconv.Surface, int, int, bool, []gpuconv.SurfaceMove) ([]uint32, bool, error)
}

func graphicsProgressiveCopies(w, h int, moves []gpuconv.SurfaceMove, create bool) []byte {
	var wire []byte
	if create {
		wire = append(wire, graphicsCreateSurface(progressiveMoveScratch, w, h)...)
	}
	wire = append(wire, graphicsSurfaceCopy(1, progressiveMoveScratch, image.Rect(0, 0, w, h), image.Point{})...)
	// Coalesce adjacent destination tiles with the same source displacement.
	ordered := append([]gpuconv.SurfaceMove(nil), moves...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.DestinationY != b.DestinationY {
			return a.DestinationY < b.DestinationY
		}
		return a.DestinationX < b.DestinationX
	})
	for i := 0; i < len(ordered); {
		m := ordered[i]
		i++
		for i < len(ordered) {
			n := ordered[i]
			if n.DestinationY != m.DestinationY || n.SourceY != m.SourceY || n.DestinationX != m.DestinationX+m.Width || n.SourceX != m.SourceX+m.Width {
				break
			}
			m.Width += n.Width
			i++
		}
		wire = append(wire, graphicsSurfaceCopy(progressiveMoveScratch, 1, image.Rect(m.SourceX, m.SourceY, m.SourceX+m.Width, m.SourceY+m.Height), image.Pt(m.DestinationX, m.DestinationY))...)
	}
	return wire
}
