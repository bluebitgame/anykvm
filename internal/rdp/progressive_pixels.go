package rdp

import (
	"bytes"
	"fmt"

	"anykvm/internal/pixelops"
)

type progressivePixelFormat uint8

const (
	progressiveBGRA progressivePixelFormat = iota
	progressiveNV12                        // Packed Y then interleaved UV, limited-range BT.709.
)

func progressivePixelBytes(w, h int, format progressivePixelFormat) int {
	switch format {
	case progressiveBGRA:
		return w * h * 4
	case progressiveNV12:
		return w*h + 2*((w+1)/2)*((h+1)/2)
	default:
		return -1
	}
}

func progressiveInputPixels(data []byte, w, h int, format string) ([]byte, progressivePixelFormat, error) {
	if format == "nv12" {
		if w < 1 || h < 1 || w > 8192 || h > 8192 || len(data) != progressivePixelBytes(w, h, progressiveNV12) {
			return nil, 0, fmt.Errorf("invalid NV12 bitmap frame")
		}
		return data, progressiveNV12, nil
	}
	pixels, err := bitmapBGRA(data, w, h, format)
	return pixels, progressiveBGRA, err
}

func progressiveRectDirty(pixels, previous []byte, w, h int, r bitmapRect, format progressivePixelFormat) bool {
	if len(previous) != len(pixels) {
		return true
	}
	if format == progressiveBGRA {
		i := (r.y*w + r.x) * 4
		return !pixelops.EqualBGRBlock(pixels[i:], previous[i:], w*4, r.w, r.h)
	}
	for y := r.y; y < r.y+r.h; y++ {
		i := y*w + r.x
		if !bytes.Equal(pixels[i:i+r.w], previous[i:i+r.w]) {
			return true
		}
	}
	stride, columns := 2*((w+1)/2), 2*((r.w+1)/2)
	for y := r.y / 2; y < (r.y+r.h+1)/2; y++ {
		i := w*h + y*stride + r.x
		if !bytes.Equal(pixels[i:i+columns], previous[i:i+columns]) {
			return true
		}
	}
	return false
}

func dirtyProgressiveRectsFormat(pixels, previous []byte, w, h int, format progressivePixelFormat) []bitmapRect {
	if format == progressiveBGRA {
		return dirtyBitmapRects(pixels, previous, w, h)
	}
	if len(previous) != len(pixels) {
		// No previous picture: emit the entire 64px grid without scanning.
		return dirtyBitmapRectsSized(pixels, nil, w, h, 64)
	}
	var rects []bitmapRect
	for y := 0; y < h; y += pixelops.BlockSize {
		for x := 0; x < w; x += pixelops.BlockSize {
			r := bitmapRect{x, y, min(pixelops.BlockSize, w-x), min(pixelops.BlockSize, h-y)}
			if progressiveRectDirty(pixels, previous, w, h, r, format) {
				rects = append(rects, r)
			}
		}
	}
	return rects
}

// Preserve the existing NV12 -> BGRA -> ICT integer rounding and clipping,
// but consume Y/UV directly without allocating an intermediate RGB picture.
func progressiveICT(planes *[3][4096]int32, pixels []byte, w, h, x, y int, format progressivePixelFormat) {
	uvStride := 2 * ((w + 1) / 2)
	for yy := 0; yy < 64; yy++ {
		row := min(y+yy, h-1)
		for xx := 0; xx < 64; xx++ {
			column := min(x+xx, w-1)
			var b, g, r int32
			if format == progressiveNV12 {
				i := w*h + (row/2)*uvStride + (column/2)*2
				yv := 298 * (int32(pixels[row*w+column]) - 16)
				u, v := int32(pixels[i])-128, int32(pixels[i+1])-128
				b = min(255, max(0, (yv+541*u+128)>>8))
				g = min(255, max(0, (yv-55*u-136*v+128)>>8))
				r = min(255, max(0, (yv+459*v+128)>>8))
			} else {
				i := (row*w + column) * 4
				b, g, r = int32(pixels[i]), int32(pixels[i+1]), int32(pixels[i+2])
			}
			i := yy*64 + xx
			planes[0][i] = ((9798*r + 19235*g + 3735*b) >> 10) - 4096
			planes[1][i] = (-5535*r - 10868*g + 16403*b) >> 10
			planes[2][i] = (16377*r - 13714*g - 2663*b) >> 10
		}
	}
}
