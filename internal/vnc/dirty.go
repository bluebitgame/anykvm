package vnc

import "anykvm/internal/pixelops"

// rectangle is a region of the framebuffer in pixel coordinates.
type rectangle struct {
	x, y, w, h int
}

// computeDirtyRects compares cur against prev (both BGRA, width*height*4 bytes)
// using shared 64x64 tiles and returns a set of rectangles covering all
// changed tiles. Adjacent changed tiles on the same tile-row are merged
// horizontally, then vertically-adjacent rectangles with identical x/width are
// merged to reduce rectangle count.
//
// tol is the per-pixel change tolerance: a pixel is considered unchanged when
// |dB|+|dG|+|dR| <= tol. tol=0 requires an exact color match; alpha is ignored.
//
// If prev is nil or its dimensions differ from cur, a single full-frame
// rectangle is returned (full refresh).
func computeDirtyRects(prev, cur []byte, width, height, tol int) []rectangle {
	const tileSize = pixelops.BlockSize
	if prev == nil || len(prev) != len(cur) || len(cur) != width*height*4 {
		return []rectangle{{0, 0, width, height}}
	}

	cols := (width + tileSize - 1) / tileSize
	rows := (height + tileSize - 1) / tileSize
	// dirty[r*cols+col] marks a changed tile.
	dirty := make([]bool, rows*cols)
	blocks := pixelops.DirtyBlocks(prev, cur, width, height, tol)
	for _, b := range blocks {
		dirty[(b.Y/tileSize)*cols+b.X/tileSize] = true
	}
	if len(blocks) == 0 {
		return nil
	}

	// Merge horizontally within each tile-row into runs.
	type run struct{ tx0, tx1, ty int } // tile columns [tx0,tx1), tile row ty
	var runs []run
	for ty := 0; ty < rows; ty++ {
		tx := 0
		for tx < cols {
			if !dirty[ty*cols+tx] {
				tx++
				continue
			}
			start := tx
			for tx < cols && dirty[ty*cols+tx] {
				tx++
			}
			runs = append(runs, run{start, tx, ty})
		}
	}

	// Convert tile runs to pixel rectangles, merging vertically-adjacent runs
	// that share the same tile-column span.
	var rects []rectangle
	used := make([]bool, len(runs))
	for i := range runs {
		if used[i] {
			continue
		}
		used[i] = true
		r := runs[i]
		tyEnd := r.ty
		for j := i + 1; j < len(runs); j++ {
			if used[j] {
				continue
			}
			if runs[j].tx0 == r.tx0 && runs[j].tx1 == r.tx1 && runs[j].ty == tyEnd+1 {
				used[j] = true
				tyEnd = runs[j].ty
			}
		}
		x0 := r.tx0 * tileSize
		x1 := r.tx1 * tileSize
		if x1 > width {
			x1 = width
		}
		y0 := r.ty * tileSize
		y1 := (tyEnd + 1) * tileSize
		if y1 > height {
			y1 = height
		}
		rects = append(rects, rectangle{x0, y0, x1 - x0, y1 - y0})
	}
	return rects
}

// cropBGRA extracts the pixels of rect r from a full BGRA frame into a tightly
// packed BGRA buffer (r.w*r.h*4 bytes).
func cropBGRA(src []byte, width int, r rectangle) []byte {
	if r.x == 0 && r.y == 0 && r.w == width && r.h == len(src)/(width*4) {
		return src
	}
	stride := width * 4
	rowBytes := r.w * 4
	out := make([]byte, r.w*r.h*4)
	for y := 0; y < r.h; y++ {
		srcOff := (r.y+y)*stride + r.x*4
		copy(out[y*rowBytes:(y+1)*rowBytes], src[srcOff:srcOff+rowBytes])
	}
	return out
}
