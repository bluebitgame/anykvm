package vnc

import (
	"encoding/binary"
	"math/bits"
)

// pixelFormat describes how pixel values are represented on the wire, as
// negotiated via ServerInit / SetPixelFormat. The server converts its native
// BGRA source pixels into this format for every encoding.
type pixelFormat struct {
	bpp        int // bytes per pixel (1, 2 or 4)
	depth      int
	bigEndian  bool
	trueColor  bool
	redMax     uint32
	greenMax   uint32
	blueMax    uint32
	redShift   uint
	greenShift uint
	blueShift  uint
}

// defaultPixelFormat is the server's native format (BGR888, 32bpp little-endian),
// matching pixelFormatBGR advertised in ServerInit.
func defaultPixelFormat() pixelFormat {
	return pixelFormat{
		bpp:        4,
		depth:      24,
		bigEndian:  false,
		trueColor:  true,
		redMax:     255,
		greenMax:   255,
		blueMax:    255,
		redShift:   16,
		greenShift: 8,
		blueShift:  0,
	}
}

// parsePixelFormat parses the 16-byte pixel-format structure from a
// SetPixelFormat message. If the client requests a colour-map (non true-colour)
// or an unsupported bpp, the server's default true-colour format is kept.
func parsePixelFormat(b []byte) pixelFormat {
	bpp := int(b[0]) / 8
	trueColor := b[3] != 0
	if !trueColor || (bpp != 1 && bpp != 2 && bpp != 4) {
		return defaultPixelFormat()
	}
	return pixelFormat{
		bpp:        bpp,
		depth:      int(b[1]),
		bigEndian:  b[2] != 0,
		trueColor:  true,
		redMax:     uint32(binary.BigEndian.Uint16(b[4:6])),
		greenMax:   uint32(binary.BigEndian.Uint16(b[6:8])),
		blueMax:    uint32(binary.BigEndian.Uint16(b[8:10])),
		redShift:   uint(b[10]),
		greenShift: uint(b[11]),
		blueShift:  uint(b[12]),
	}
}

// isNativeBGRA reports whether the format matches the server's native BGRA
// layout, allowing frame pixels to be sent without conversion.
func (pf pixelFormat) isNativeBGRA() bool {
	return pf.bpp == 4 && !pf.bigEndian && pf.trueColor &&
		pf.redMax == 255 && pf.greenMax == 255 && pf.blueMax == 255 &&
		pf.redShift == 16 && pf.greenShift == 8 && pf.blueShift == 0
}

// value packs 8-bit r,g,b intensities into the format's pixel value.
func (pf pixelFormat) value(r, g, b uint8) uint32 {
	var v uint32
	v |= scale(r, pf.redMax) << pf.redShift
	v |= scale(g, pf.greenMax) << pf.greenShift
	v |= scale(b, pf.blueMax) << pf.blueShift
	return v
}

// putPixel writes a full pixel (bpp bytes) into dst in the negotiated endianness.
func (pf pixelFormat) putPixel(dst []byte, r, g, b uint8) {
	v := pf.value(r, g, b)
	switch pf.bpp {
	case 1:
		dst[0] = byte(v)
	case 2:
		if pf.bigEndian {
			binary.BigEndian.PutUint16(dst, uint16(v))
		} else {
			binary.LittleEndian.PutUint16(dst, uint16(v))
		}
	default: // 4
		if pf.bigEndian {
			binary.BigEndian.PutUint32(dst, v)
		} else {
			binary.LittleEndian.PutUint32(dst, v)
		}
	}
}

// convertFrame converts native BGRA pixel data into a buffer of this format's
// pixels. For the native BGRA format the input is returned unchanged.
func (pf pixelFormat) convertFrame(bgra []byte) []byte {
	if pf.isNativeBGRA() {
		return bgra
	}
	n := len(bgra) / 4
	out := make([]byte, n*pf.bpp)
	for i := 0; i < n; i++ {
		s := i * 4
		pf.putPixel(out[i*pf.bpp:], bgra[s+2], bgra[s+1], bgra[s+0])
	}
	return out
}

func (pf pixelFormat) convertBGRAToPixel(bgra []byte, w, h int) []byte {
	if w <= 0 || h <= 0 || len(bgra) < w*h*4 {
		return nil
	}
	n := w * h
	out := make([]byte, n*pf.bpp)
	for i := 0; i < n; i++ {
		s := i * 4
		b := int(bgra[s+0])
		g := int(bgra[s+1])
		r := int(bgra[s+2])
		a := int(bgra[s+3])
		if a > 0 && a < 255 {
			r = r * 255 / a
			g = g * 255 / a
			b = b * 255 / a
		}
		pf.putPixel(out[i*pf.bpp:], uint8(r), uint8(g), uint8(b))
	}
	return out
}

// cpixelBytes returns the number of bytes used for a CPIXEL (ZRLE compressed
// pixel). Per the RFB spec this is 3 when bpp is 32, depth <= 24 and all colour
// bits fit within three bytes; otherwise it equals bpp.
func (pf pixelFormat) cpixelBytes() int {
	if pf.bpp == 4 && pf.trueColor && pf.depth <= 24 {
		if _, ok := pf.cpixel3High(); ok {
			return 3
		}
	}
	return pf.bpp
}

// cpixel3High reports whether the used colour bits sit in the most significant
// three bytes (returning true) or the least significant three bytes (false).
// The second result is false when the colours do not fit in three bytes.
func (pf pixelFormat) cpixel3High() (high bool, ok bool) {
	minShift := pf.redShift
	if pf.greenShift < minShift {
		minShift = pf.greenShift
	}
	if pf.blueShift < minShift {
		minShift = pf.blueShift
	}

	top := func(shift uint, max uint32) uint {
		return shift + uint(bits.Len32(max))
	}
	maxBit := top(pf.redShift, pf.redMax)
	if b := top(pf.greenShift, pf.greenMax); b > maxBit {
		maxBit = b
	}
	if b := top(pf.blueShift, pf.blueMax); b > maxBit {
		maxBit = b
	}

	if maxBit <= 24 {
		return false, true // fits in least significant 3 bytes
	}
	if minShift >= 8 {
		return true, true // fits in most significant 3 bytes
	}
	return false, false
}

// putCPIXEL writes a CPIXEL for the given colour into buf-style writer via dst
// slice, returning the number of bytes written.
func (pf pixelFormat) putCPIXEL(dst []byte, r, g, b uint8) int {
	cb := pf.cpixelBytes()
	if cb == 3 {
		var full [4]byte
		v := pf.value(r, g, b)
		if pf.bigEndian {
			binary.BigEndian.PutUint32(full[:], v)
		} else {
			binary.LittleEndian.PutUint32(full[:], v)
		}
		high, _ := pf.cpixel3High()
		// On little-endian the least significant 3 bytes are full[0:3] and the
		// most significant are full[1:4]; big-endian is the reverse.
		if pf.bigEndian {
			if high {
				copy(dst, full[0:3])
			} else {
				copy(dst, full[1:4])
			}
		} else {
			if high {
				copy(dst, full[1:4])
			} else {
				copy(dst, full[0:3])
			}
		}
		return 3
	}
	pf.putPixel(dst, r, g, b)
	return cb
}

// scale maps an 8-bit intensity onto a channel whose maximum is max.
func scale(v uint8, max uint32) uint32 {
	if max == 255 {
		return uint32(v)
	}
	return (uint32(v)*max + 127) / 255
}
