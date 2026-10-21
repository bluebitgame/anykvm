package vnc

import (
	"encoding/binary"

	"anykvm/internal/capture"
)

func (c *client) prepareCursor(cs *capture.CursorState) (enc int32, data []byte, w, h, hotX, hotY uint16) {
	if cs == nil {
		return
	}
	switch {
	case c.supportsCursorWithAlpha():
		enc = encCursorWithAlpha
	case c.supportsCursor():
		enc = encCursor
	case c.hasEncoding(encXCursor):
		enc = encXCursor
	default:
		return
	}
	if cs.SystemDefault {
		cs = defaultCursorShape()
	}
	if cs.Width == 0 && cs.Height == 0 && len(cs.Image) == 0 {
		if enc == encCursorWithAlpha {
			data = binary.BigEndian.AppendUint32(nil, uint32(encRaw))
		}
		return // zero-size shape explicitly hides the pointer
	}
	if cs.Width < 1 || cs.Height < 1 || cs.Width > 256 || cs.Height > 256 || len(cs.Image) != cs.Width*cs.Height*4 {
		return 0, nil, 0, 0, 0, 0
	}
	width, height := c.cursorDimensions(cs)
	pixels := resampleCursor(cs, width, height, enc == encCursorWithAlpha)
	switch enc {
	case encCursorWithAlpha:
		// RFB Cursor With Alpha uses premultiplied RGBA regardless of the
		// negotiated framebuffer pixel format. Capture sources use BGRA.
		for i := 0; i < len(pixels); i += 4 {
			pixels[i], pixels[i+2] = pixels[i+2], pixels[i]
		}
		data = binary.BigEndian.AppendUint32(nil, uint32(encRaw))
		data = append(data, pixels...)
	case encXCursor:
		stride := (width + 7) / 8
		bitmap := make([]byte, stride*height)
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				i := (y*width + x) * 4
				if int(pixels[i])+int(pixels[i+1])+int(pixels[i+2]) >= 384 {
					bitmap[y*stride+x/8] |= 1 << uint(7-x%8)
				}
			}
		}
		data = append([]byte{255, 255, 255, 0, 0, 0}, bitmap...)
		data = append(data, buildCursorMask(pixels, width, height)...)
	default:
		pf := c.pixelFormatSnapshot()
		data = pf.convertFrame(pixels)
		data = append(data, buildCursorMask(pixels, width, height)...)
	}
	hotX = uint16(max(0, min(cs.Width-1, cs.HotX)) * width / cs.Width)
	hotY = uint16(max(0, min(cs.Height-1, cs.HotY)) * height / cs.Height)
	return enc, data, uint16(width), uint16(height), hotX, hotY
}

// cursorDimensions applies the desktop's uniform reduction and the RFB size cap.
func (c *client) cursorDimensions(cs *capture.CursorState) (width, height int) {
	width, height = cs.Width, cs.Height
	// Remote cursor rasters are in source desktop pixels. Follow the same
	// uniform reduction as the displayed desktop, then enforce the wire limit.
	if cs.SourceWidth > 0 && cs.SourceHeight > 0 && c.width > 0 && c.height > 0 {
		num, den := 1, 1
		if int(c.width) < cs.SourceWidth {
			num, den = int(c.width), cs.SourceWidth
		}
		if int(c.height)*den < cs.SourceHeight*num {
			num, den = int(c.height), cs.SourceHeight
		}
		width, height = max(1, width*num/den), max(1, height*num/den)
	}
	if size := max(width, height); size > 128 {
		width = max(1, width*128/size)
		height = max(1, height*128/size)
	}
	return width, height
}

// resampleCursor returns BGRA with the alpha convention required by the encoding.
func resampleCursor(cs *capture.CursorState, width, height int, premultiplied bool) []byte {
	pixels := make([]byte, width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src := ((y*cs.Height/height)*cs.Width + x*cs.Width/width) * 4
			dst := (y*width + x) * 4
			alpha := int(cs.Image[src+3])
			for channel := 0; channel < 3; channel++ {
				value := int(cs.Image[src+channel])
				if premultiplied {
					if !cs.Premultiplied {
						value = (value*alpha + 127) / 255
					}
				} else if cs.Premultiplied && alpha > 0 {
					value = min(255, (value*255+alpha/2)/alpha)
				}
				if alpha == 0 {
					value = 0
				}
				pixels[dst+channel] = byte(value)
			}
			pixels[dst+3] = byte(alpha)
		}
	}
	return pixels
}

// RFB has no system-default-pointer message; supply a small standard arrow.
func defaultCursorShape() *capture.CursorState {
	// X = black, . = white, space = transparent.
	rows := []string{
		"X           ",
		"XX          ",
		"X.X         ",
		"X..X        ",
		"X...X       ",
		"X....X      ",
		"X.....X     ",
		"X......X    ",
		"X.......X   ",
		"X........X  ",
		"X.....XXXXX ",
		"X..X..X     ",
		"X.X X..X    ",
		"XX  X..X    ",
		"X    X..X   ",
		"     X..X   ",
		"      XX    ",
	}
	pixels := make([]byte, 12*len(rows)*4)
	for y, row := range rows {
		for x, ch := range row {
			if x >= 12 {
				break
			}
			if ch == ' ' {
				continue
			}
			i := (y*12 + x) * 4
			pixels[i+3] = 255
			if ch == '.' {
				pixels[i], pixels[i+1], pixels[i+2] = 255, 255, 255
			}
		}
	}
	return &capture.CursorState{Image: pixels, Width: 12, Height: len(rows)}
}

func buildCursorMask(bgra []byte, w, h int) []byte {
	rowBytes := (w + 7) / 8
	mask := make([]byte, rowBytes*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			alpha := bgra[(y*w+x)*4+3]
			if alpha >= 64 {
				mask[y*rowBytes+x/8] |= 1 << (7 - (x % 8))
			}
		}
	}
	return mask
}
