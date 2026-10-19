package vnc

import (
	"encoding/binary"
)

func decodeRawClient(fb []byte, fbW int, r rect, data []byte, pf pixelFormat) {
	if pf.isNativeBGRA() {
		for y := 0; y < int(r.height); y++ {
			soff := y * int(r.width) * 4
			doff := ((int(r.y)+y)*fbW + int(r.x)) * 4
			copy(fb[doff:], data[soff:soff+int(r.width)*4])
		}
		return
	}
	for y := 0; y < int(r.height); y++ {
		for x := 0; x < int(r.width); x++ {
			doff := ((int(r.y)+y)*fbW + int(r.x) + x) * 4
			soff := (y*int(r.width) + x) * pf.bpp
			if soff+pf.bpp > len(data) {
				continue
			}
			v := readPixelClient(data, soff, pf)
			r8, g8, b8 := pixelToRGB(v, pf)
			fb[doff+0] = b8
			fb[doff+1] = g8
			fb[doff+2] = r8
			fb[doff+3] = 255
		}
	}
}

func decodeCopyRectClient(fb []byte, fbW int, r rect, data []byte) {
	srcX := int(binary.BigEndian.Uint16(data[0:2]))
	srcY := int(binary.BigEndian.Uint16(data[2:4]))
	// Copy bottom-up for downward overlap so destination rows do not
	// overwrite source rows that still need to be copied.
	start, end, step := 0, int(r.height), 1
	if int(r.y) > srcY {
		start, end, step = int(r.height)-1, -1, -1
	}
	for y := start; y != end; y += step {
		srcOff := ((srcY+y)*fbW + srcX) * 4
		dstOff := ((int(r.y)+y)*fbW + int(r.x)) * 4
		copy(fb[dstOff:], fb[srcOff:srcOff+int(r.width)*4])
	}
}

func readPixelClient(b []byte, off int, pf pixelFormat) uint32 {
	n := pf.bpp
	if off+n > len(b) {
		return 0
	}
	if pf.bigEndian {
		switch n {
		case 1:
			return uint32(b[off])
		case 2:
			return uint32(binary.BigEndian.Uint16(b[off:]))
		case 4:
			return binary.BigEndian.Uint32(b[off:])
		}
	} else {
		switch n {
		case 1:
			return uint32(b[off])
		case 2:
			return uint32(binary.LittleEndian.Uint16(b[off:]))
		case 4:
			return binary.LittleEndian.Uint32(b[off:])
		}
	}
	return 0
}

func pixelToRGB(pixel uint32, pf pixelFormat) (r, g, b uint8) {
	rm := (pixel >> pf.redShift) & pf.redMax
	gm := (pixel >> pf.greenShift) & pf.greenMax
	bm := (pixel >> pf.blueShift) & pf.blueMax
	if pf.redMax != 255 {
		r = uint8(rm * 255 / pf.redMax)
	} else {
		r = uint8(rm)
	}
	if pf.greenMax != 255 {
		g = uint8(gm * 255 / pf.greenMax)
	} else {
		g = uint8(gm)
	}
	if pf.blueMax != 255 {
		b = uint8(bm * 255 / pf.blueMax)
	} else {
		b = uint8(bm)
	}
	return
}
