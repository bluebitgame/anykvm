package vnc

import (
	"bytes"
	"encoding/binary"
	"image"

	"anykvm/internal/capture"
	"anykvm/internal/pixelops"
)

type copyRectangle struct{ source, destination image.Rectangle }

func losslessPixelEncoding(enc int32) bool {
	return enc == encRaw || enc == encHextile || enc == encZlib || enc == encZRLE
}

// planCopies treats DXGI moves as hints, then proves each against the actual
// viewer baseline. Queue coalescing discards moves and keeps their damage union.
func (c *client) planCopies(frame *frameData, full bool, enc int32) []copyRectangle {
	d := frame.damage
	if full || !c.hasEncoding(encCopyRect) || !c.damageState.Continuous(d) || len(d.Moves) > 64 ||
		d.SourceWidth != int(frame.width) || d.SourceHeight != int(frame.height) {
		return nil
	}
	bounds := image.Rect(0, 0, int(frame.width), int(frame.height))
	var copies []copyRectangle
	for _, move := range d.Moves {
		src, dst := move.Source, move.Destination
		if src.Empty() || src.Size() != dst.Size() || !src.In(bounds) || !dst.In(bounds) || src == dst {
			continue
		}
		match := false
		if frame.format == capture.FormatJPEG && frame.jpegRegions != nil && c.jpegPrevious != nil {
			match = frame.jpegRegions.EqualMovedRect(c.jpegPrevious.restart, src, dst)
		} else if frame.format == capture.FormatRaw && c.rawCopySafe && losslessPixelEncoding(enc) &&
			c.prevW == int(frame.width) && c.prevH == int(frame.height) && len(c.prevFrame) == int(frame.width)*int(frame.height)*4 && len(frame.data) == len(c.prevFrame) {
			stride := int(frame.width) * 4
			match = pixelops.EqualBGRBlock(c.prevFrame[src.Min.Y*stride+src.Min.X*4:], frame.data[dst.Min.Y*stride+dst.Min.X*4:], stride, src.Dx(), src.Dy())
		}
		if match {
			copies = append(copies, copyRectangle{src, dst})
		}
	}
	// A single self-overlapping move is valid (memmove semantics). Independent
	// copies may run in any order. Reject interdependent copies, including cycles,
	// rather than reading pixels overwritten by an earlier CopyRect.
	unsafe := make([]bool, len(copies))
	for i, a := range copies {
		for j := i + 1; j < len(copies); j++ {
			b := copies[j]
			if a.destination.Overlaps(b.source) || b.destination.Overlaps(a.source) || a.destination.Overlaps(b.destination) {
				unsafe[i], unsafe[j] = true, true
			}
		}
	}
	result := copies[:0]
	for i, copy := range copies {
		if !unsafe[i] {
			result = append(result, copy)
		}
	}
	return result
}

func copyPixelBaseline(previous []byte, width int, copies []copyRectangle) []byte {
	out := bytes.Clone(previous)
	for _, move := range copies {
		for y := 0; y < move.source.Dy(); y++ {
			src := ((move.source.Min.Y+y)*width + move.source.Min.X) * 4
			dst := ((move.destination.Min.Y+y)*width + move.destination.Min.X) * 4
			// Always read the original baseline, including self-overlapping moves.
			copy(out[dst:dst+move.source.Dx()*4], previous[src:src+move.source.Dx()*4])
		}
	}
	return out
}

func omitCopyDestinations(rects []rectangle, copies []copyRectangle) []rectangle {
	for _, copy := range copies {
		var next []rectangle
		for _, r := range rects {
			region := image.Rect(r.x, r.y, r.x+r.w, r.y+r.h)
			cut := region.Intersect(copy.destination)
			if cut.Empty() {
				next = append(next, r)
				continue
			}
			for _, p := range []image.Rectangle{
				image.Rect(region.Min.X, region.Min.Y, region.Max.X, cut.Min.Y),
				image.Rect(region.Min.X, cut.Max.Y, region.Max.X, region.Max.Y),
				image.Rect(region.Min.X, cut.Min.Y, cut.Min.X, cut.Max.Y),
				image.Rect(cut.Max.X, cut.Min.Y, region.Max.X, cut.Max.Y),
			} {
				if !p.Empty() {
					next = append(next, rectangle{p.Min.X, p.Min.Y, p.Dx(), p.Dy()})
				}
			}
		}
		rects = next
	}
	return rects
}

func writeCopyRectangle(buf *bytes.Buffer, move copyRectangle) {
	var packet [16]byte
	for i, v := range []int{move.destination.Min.X, move.destination.Min.Y, move.destination.Dx(), move.destination.Dy()} {
		binary.BigEndian.PutUint16(packet[i*2:], uint16(v))
	}
	binary.BigEndian.PutUint32(packet[8:], uint32(encCopyRect))
	binary.BigEndian.PutUint16(packet[12:], uint16(move.source.Min.X))
	binary.BigEndian.PutUint16(packet[14:], uint16(move.source.Min.Y))
	buf.Write(packet[:])
}
