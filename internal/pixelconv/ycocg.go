// Package pixelconv converts desktop pixels into subsampled color planes.
// NV12 inputs use limited-range BT.709. Desktop alpha is ignored.
package pixelconv

type implementation struct {
	name    string
	convert func(dst, first, second []byte, firstStride, secondStride, width, height int, nv12 bool)
	delta   func(dst, row, previous []byte)
}

var scalar = implementation{"Go", convertGo, deltaGo}
var selected = scalar

func Backend() string { return selected.name }

// Size420 returns the size of tightly packed Y, Co, Cg planes. Each chroma
// plane has ceil(width/2) by ceil(height/2) samples, including partial edges.
func Size420(width, height int) int {
	if width < 1 || height < 1 || width > 8192 || height > 8192 {
		panic("pixelconv: invalid picture dimensions")
	}
	return width*height + 2*((width+1)/2)*((height+1)/2)
}

func validatePlane(p []byte, stride, rowBytes, rows int) {
	if stride < rowBytes || len(p) < rowBytes || rows-1 > (len(p)-rowBytes)/stride {
		panic("pixelconv: truncated plane")
	}
}

// BGRAToYCoCg420 writes Y, Co, Cg into dst without allocating. Source rows may
// have padding. Chroma uses CLL=1 and 2x2 averaging. Co has the historical RDP
// wire polarity (B-R), as required by deployed Planar decoders. Dst and src
// must not overlap. No buffer is retained after the synchronous conversion.
func BGRAToYCoCg420(dst, src []byte, stride, width, height int) []byte {
	n := Size420(width, height)
	validatePlane(src, stride, width*4, height)
	if len(dst) < n {
		panic("pixelconv: truncated destination")
	}
	dst = dst[:n]
	selected.convert(dst, src, nil, stride, 0, width, height, false)
	return dst
}

// NV12ToYCoCg420 reads Y and interleaved UV directly, with independent strides.
// Both inputs start at an even picture coordinate. RGB clipping and integer
// rounding are fused into the conversion; no intermediate BGRA picture is
// created. The output layout and non-overlap requirement match BGRAToYCoCg420.
func NV12ToYCoCg420(dst, y, uv []byte, yStride, uvStride, width, height int) []byte {
	n := Size420(width, height)
	validatePlane(y, yStride, width, height)
	validatePlane(uv, uvStride, 2*((width+1)/2), (height+1)/2)
	if len(dst) < n {
		panic("pixelconv: truncated destination")
	}
	dst = dst[:n]
	selected.convert(dst, y, uv, yStride, uvStride, width, height, true)
	return dst
}

// Delta applies RDP Planar's signed byte row difference and zigzag mapping.
// Dst may alias row; previous must not overlap dst. No allocation occurs.
func Delta(dst, row, previous []byte) {
	if len(dst) < len(row) || len(previous) < len(row) {
		panic("pixelconv: truncated delta row")
	}
	selected.delta(dst[:len(row)], row, previous[:len(row)])
}

func deltaGo(dst, row, previous []byte) {
	for i, value := range row {
		d := int(int8(value - previous[i]))
		if d < 0 {
			dst[i] = byte(-2*d - 1)
		} else {
			dst[i] = byte(2 * d)
		}
	}
}

func convertGo(dst, first, second []byte, firstStride, secondStride, width, height int, nv12 bool) {
	cw, ch := (width+1)/2, (height+1)/2
	yPlane, co, cg := dst[:width*height], dst[width*height:width*height+cw*ch], dst[width*height+cw*ch:]
	for y := 0; y < height; y += 2 {
		for x := 0; x < width; x += 2 {
			var sumCo, sumCg int
			for dy := 0; dy < 2; dy++ {
				yy := min(y+dy, height-1)
				for dx := 0; dx < 2; dx++ {
					xx := min(x+dx, width-1)
					var b, g, r int
					if nv12 {
						i := (yy/2)*secondStride + (xx/2)*2
						yv, u, v := 298*(int(first[yy*firstStride+xx])-16), int(second[i])-128, int(second[i+1])-128
						b = min(255, max(0, (yv+541*u+128)>>8))
						g = min(255, max(0, (yv-55*u-136*v+128)>>8))
						r = min(255, max(0, (yv+459*v+128)>>8))
					} else {
						i := yy*firstStride + xx*4
						b, g, r = int(first[i]), int(first[i+1]), int(first[i+2])
					}
					yPlane[yy*width+xx] = byte((r + 2*g + b) >> 2)
					sumCo += b - r
					sumCg += 2*g - r - b
				}
			}
			i := (y/2)*cw + x/2
			// Replicating odd edges makes their average identical to averaging
			// only the existing 1x1, 1x2 or 2x1 pixels. Use signed wide sums.
			co[i], cg[i] = byte(sumCo>>3), byte(sumCg>>4)
		}
	}
}
