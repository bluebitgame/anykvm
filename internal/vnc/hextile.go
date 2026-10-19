package vnc

import (
	"bytes"
)

const (
	hextileTileSize            = 16
	hextileRaw                 = 0x01
	hextileBackgroundSpecified = 0x02
	hextileForegroundSpecified = 0x04
	hextileAnySubrects         = 0x08
	hextileSubrectsColoured    = 0x10
)

// encodeHextile encodes pixel data using Hextile encoding.
// bpp is the bytes per pixel from the negotiated pixel format.
func encodeHextile(pixels []byte, width, height, bpp int) []byte {
	var buf bytes.Buffer
	stride := width * bpp

	for ty := 0; ty < height; ty += hextileTileSize {
		th := hextileTileSize
		if ty+th > height {
			th = height - ty
		}
		for tx := 0; tx < width; tx += hextileTileSize {
			tw := hextileTileSize
			if tx+tw > width {
				tw = width - tx
			}
			encodeHextileTile(&buf, pixels, stride, tx, ty, tw, th, bpp)
		}
	}

	return buf.Bytes()
}

func encodeHextileTile(w *bytes.Buffer, pixels []byte, stride, tx, ty, tw, th, bpp int) {
	firstOff := ty*stride + tx*bpp
	firstPixel := pixels[firstOff : firstOff+bpp]
	solid := true

	for y := 0; y < th && solid; y++ {
		rowOff := (ty+y)*stride + tx*bpp
		for x := 0; x < tw; x++ {
			off := rowOff + x*bpp
			for i := 0; i < bpp; i++ {
				if pixels[off+i] != firstPixel[i] {
					solid = false
					break
				}
			}
		}
	}

	if solid {
		w.WriteByte(hextileBackgroundSpecified)
		w.Write(firstPixel)
		return
	}

	w.WriteByte(hextileRaw)
	for y := 0; y < th; y++ {
		rowOff := (ty+y)*stride + tx*bpp
		w.Write(pixels[rowOff : rowOff+tw*bpp])
	}
}
