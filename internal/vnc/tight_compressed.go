package vnc

import (
	"image"

	"anykvm/internal/jpegcodec"
)

// prepareCompressedTight runs after compressed-domain change detection. Neither
// this pass nor its coefficient probes reconstruct pixels. JPEG regions retain
// their exact source coefficients/tables; only proven constants become Fill.
func (c *client) prepareCompressedTight(frame *frameData, next *compressedJPEGState, rects []rectangle) []rectangle {
	if next == nil || len(rects) == 0 {
		return rects
	}
	if next.restart != nil {
		var checker *jpegcodec.RestartSolidChecker
		if !next.restart.HasRecordedColors() {
			checker = next.restart.NewSolidChecker()
		}
		if checker == nil && !next.restart.HasRecordedColors() {
			return rects
		}
		blockSize := 64
		if next.restart.HasRecordedColors() {
			blockSize = 16 // Isolate every metadata-only MCU; never rebuild it for Tight.
		}
		planned, fills := prepareTightRegionsSized(rects, blockSize, func(r rectangle) (uint32, bool) {
			region := image.Rect(r.x, r.y, r.x+r.w, r.y+r.h)
			if color, solid, known := next.restart.RecordedSolidColor(region); known {
				return color, solid
			}
			return checker.Color(region)
		})
		if len(fills) == 0 {
			return rects // Preserve existing JPEG coalescing when nothing is solid.
		}
		payloads := make(map[rectangle][]byte)
		for _, r := range planned {
			if _, fill := fills[r]; fill {
				continue
			}
			data := frame.jpegRects[r]
			if data == nil {
				var err error
				data, err = next.restart.Crop(image.Rect(r.x, r.y, r.x+r.w, r.y+r.h))
				if err != nil {
					return rects // No partial state on an unsupported crop.
				}
			}
			payloads[r] = data
		}
		frame.tightFills, frame.jpegRects = fills, payloads
		return planned
	}
	// External MJPEG crops are already 64px tiles (or the whole-frame fallback).
	// Parse only dirty payloads; progressive/unknown color layouts stay JPEG.
	fills := make(map[rectangle]uint32)
	for _, r := range rects {
		data, err := c.jpegForRect(frame, r)
		if err != nil {
			continue
		}
		if color, solid := jpegcodec.SolidJPEGColor(data); solid {
			fills[r] = color
		}
	}
	frame.tightFills = fills
	return rects
}
