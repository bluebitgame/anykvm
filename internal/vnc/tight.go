package vnc

import "anykvm/internal/pixelops"

// prepareTightRects classifies only regions already selected by dirty detection.
// Keep temporal comparison in pixelops (SIMD, tolerance and early exits), and
// reuse its SIMD color-run scan here. Fusing both into a per-pixel Go loop would
// slow unchanged frames and make every other encoding pay for Tight analysis.
// Fill classification is exact even when the temporal comparison uses tolerance.
func prepareTightRects(pixels []byte, width int, dirty []rectangle) ([]rectangle, map[rectangle]uint32) {
	return prepareTightRegions(dirty, func(r rectangle) (uint32, bool) {
		return solidBGRRect(pixels, width, r)
	})
}

// Raw and coefficient-domain paths share the exact same splitting/merging
// policy. Only the test for an individual block differs.
func prepareTightRegions(dirty []rectangle, colorAt func(rectangle) (uint32, bool)) ([]rectangle, map[rectangle]uint32) {
	return prepareTightRegionsSized(dirty, pixelops.BlockSize, colorAt)
}

func prepareTightRegionsSized(dirty []rectangle, blockSize int, colorAt func(rectangle) (uint32, bool)) ([]rectangle, map[rectangle]uint32) {
	type style struct {
		color uint32
		fill  bool
	}
	type run struct {
		x, w int
		style
	}
	var rects []rectangle
	fills := make(map[rectangle]uint32)
	for _, region := range dirty {
		// Row maps let equal runs merge vertically in linear time. Rectangles
		// never extend outside the selected dirty region (including CopyRect cuts).
		previous := make(map[run]int)
		for y := region.y; y < region.y+region.h; y += blockSize {
			height := min(blockSize, region.y+region.h-y)
			var runs []run
			for x := region.x; x < region.x+region.w; x += blockSize {
				w := min(blockSize, region.x+region.w-x)
				color, solid := colorAt(rectangle{x, y, w, height})
				current := run{x, w, style{color, solid}}
				if len(runs) > 0 {
					last := &runs[len(runs)-1]
					if last.style == current.style && last.x+last.w == x && last.w+w <= 2048 {
						last.w += w
						continue
					}
				}
				runs = append(runs, current)
			}
			next := make(map[run]int, len(runs))
			for _, run := range runs {
				if i, ok := previous[run]; ok && (run.fill || rects[i].h+height <= 256) {
					old := rects[i]
					rects[i].h += height
					if run.fill {
						delete(fills, old)
						fills[rects[i]] = run.color
					}
					next[run] = i
					continue
				}
				r := rectangle{run.x, y, run.w, height}
				next[run] = len(rects)
				rects = append(rects, r)
				if run.fill {
					fills[r] = run.color
				}
			}
			previous = next
		}
	}
	return rects, fills
}

// solidBGRRect ignores alpha and row padding, just like dirty comparison.
// Non-solid blocks normally fail on the first row; solid rows use SIMD RunBGR.
func solidBGRRect(pixels []byte, width int, r rectangle) (uint32, bool) {
	offset := (r.y*width + r.x) * 4
	b, g, red := pixels[offset], pixels[offset+1], pixels[offset+2]
	for y := 0; y < r.h; y++ {
		start := offset + y*width*4
		row := pixels[start : start+r.w*4]
		if row[0] != b || row[1] != g || row[2] != red || pixelops.RunBGR(row) != r.w {
			return 0, false
		}
	}
	return uint32(red)<<16 | uint32(g)<<8 | uint32(b), true
}
