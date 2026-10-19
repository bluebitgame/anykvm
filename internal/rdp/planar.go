package rdp

import "anykvm/internal/pixelconv"

// Planar always uses CLL=1, 4:2:0 chroma and opaque pixels. Raw fallback keeps
// this color model; lossless RGB planes are not an alternative encoding path.
const planar420Header = 0x29

type planarEncoder struct {
	planes, delta, wire []byte
}

func (*planarEncoder) backend() string { return pixelconv.Backend() }

func planarBuffer(p []byte, n int) []byte {
	if cap(p) < n {
		return make([]byte, n)
	}
	return p[:n]
}

// encodeRect reads the source rectangle without allocating a tile copy. RDP
// updates lie on the 64px grid, so NV12 chroma starts at an even coordinate.
// The returned buffer is valid until the next call on this encoder.
func (e *planarEncoder) encodeRect(pixels []byte, w, h int, r bitmapRect, format progressivePixelFormat) []byte {
	if r.x < 0 || r.y < 0 || r.w < 1 || r.h < 1 || r.x+r.w > w || r.y+r.h > h ||
		len(pixels) != progressivePixelBytes(w, h, format) {
		panic("rdp: invalid Planar input rectangle")
	}
	e.planes = planarBuffer(e.planes, pixelconv.Size420(r.w, r.h))
	if format == progressiveNV12 {
		if r.x%2 != 0 || r.y%2 != 0 {
			panic("rdp: unaligned NV12 rectangle")
		}
		uvStride := 2 * ((w + 1) / 2)
		pixelconv.NV12ToYCoCg420(e.planes, pixels[r.y*w+r.x:w*h],
			pixels[w*h+(r.y/2)*uvStride+r.x:], w, uvStride, r.w, r.h)
	} else {
		pixelconv.BGRAToYCoCg420(e.planes, pixels[(r.y*w+r.x)*4:], w*4, r.w, r.h)
	}
	ySize, cw, ch := r.w*r.h, (r.w+1)/2, (r.h+1)/2
	e.delta = planarBuffer(e.delta, ySize)
	e.wire = append(e.wire[:0], planar420Header|0x10)
	for i, plane := range [][]byte{e.planes[:ySize], e.planes[ySize : ySize+cw*ch], e.planes[ySize+cw*ch:]} {
		pw, ph := r.w, r.h
		if i != 0 {
			pw, ph = cw, ch
		}
		copy(e.delta[:pw], plane[:pw])
		// One bulk SIMD call covers every row difference in a plane.
		pixelconv.Delta(e.delta[pw:len(plane)], plane[pw:], plane[:len(plane)-pw])
		for y := 0; y < ph; y++ {
			e.wire = appendPlanarRow(e.wire, e.delta[y*pw:(y+1)*pw])
		}
	}
	if len(e.wire) >= len(e.planes)+2 {
		e.wire = append(e.wire[:0], planar420Header)
		e.wire = append(e.wire, e.planes...)
		e.wire = append(e.wire, 0) // Required raw stream pad byte.
	}
	return e.wire
}
