package rdp

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

const (
	maxPointerSize        = 32    // Baseline clients.
	maxLargePointerSize   = 96    // Negotiated Color/New Pointer Updates.
	pointerReassemblySize = 38055 // MS-RDPBCGR 2.2.7.2.7 minimum for 96x96.
)

func cursorPointer(cs *capture.CursorState, alpha bool) ([]byte, error) {
	return scaledCursorPointer(cs, alpha, maxPointerSize, 1, 1)
}

func scaledCursorPointer(cs *capture.CursorState, alpha bool, limit int, scaleX, scaleY float64) ([]byte, error) {
	if cs == nil {
		return arrowPointer(), nil
	}
	if cs.SystemDefault {
		return shareData(27, join(u16(1), u16(0), u32(0x7f00))), nil // SYSPTR_DEFAULT
	}
	if cs.Width == 0 && cs.Height == 0 && len(cs.Image) == 0 {
		return shareData(27, join(u16(1), u16(0), u32(0))), nil
	}
	if cs.Width < 1 || cs.Height < 1 || cs.Width > 256 || cs.Height > 256 || len(cs.Image) != cs.Width*cs.Height*4 {
		return nil, fmt.Errorf("invalid cursor image %dx%d", cs.Width, cs.Height)
	}
	if limit != maxLargePointerSize {
		limit = maxPointerSize
	}
	if scaleX <= 0 || scaleY <= 0 || math.IsNaN(scaleX) || math.IsNaN(scaleY) || math.IsInf(scaleX, 0) || math.IsInf(scaleY, 0) {
		return nil, fmt.Errorf("invalid cursor scale %g,%g", scaleX, scaleY)
	}
	// Never enlarge the captured bitmap. Allow fractional reductions, using
	// one scale for both axes and fitting the client's negotiated pointer limit.
	scale := min(1, scaleX, scaleY, float64(limit)/float64(max(cs.Width, cs.Height)))
	wf, hf := float64(cs.Width)*scale, float64(cs.Height)*scale
	w, h := min(limit, max(1, int(math.Round(wf)))), min(limit, max(1, int(math.Round(hf))))
	hotX := min(max(cs.HotX, 0), cs.Width-1) * w / cs.Width
	hotY := min(max(cs.HotY, 0), cs.Height-1) * h / cs.Height
	bpp, kind := 3, 6 // Color Pointer Update, 24-bpp BGR + AND mask.
	if alpha {
		bpp, kind = 4, 8 // New Pointer Update, 32-bpp BGRA.
	}
	xorStride, andStride := (w*bpp+1)&^1, ((w+7)/8+1)&^1
	xor, and := make([]byte, xorStride*h), make([]byte, andStride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixel := pointerPixel(cs, x, y, w, h)
			a := pixel[3]
			dst := (h-1-y)*xorStride + x*bpp
			if a == 0 || (!alpha && a < 128) {
				and[(h-1-y)*andStride+x/8] |= 0x80 >> (x % 8)
				continue // XOR must stay black for fully transparent pixels.
			}
			for channel := 0; channel < 3; channel++ {
				xor[dst+channel] = pixel[channel]
			}
			if alpha {
				xor[dst+3] = a
			}
		}
	}
	attributes := join(u16(0), u16(hotX), u16(hotY), u16(w), u16(h), u16(len(and)), u16(len(xor)), xor, and)
	if alpha {
		attributes = join(u16(32), attributes)
	}
	return shareData(27, join(u16(kind), u16(0), attributes)), nil
}

// Return straight BGRA for the wire. Reduction averages source coverage in
// premultiplied space, including alpha, then unpremultiplies only the result.
// Exact overlap weights handle fractional source footprints after rounding
// output dimensions, without dropping pixels or double-counting coverage.
func pointerPixel(cs *capture.CursorState, x, y, w, h int) [4]byte {
	if w == cs.Width && h == cs.Height {
		src := (y*w + x) * 4
		pixel := [4]byte{cs.Image[src], cs.Image[src+1], cs.Image[src+2], cs.Image[src+3]}
		if cs.Premultiplied && pixel[3] != 0 {
			for c := 0; c < 3; c++ {
				pixel[c] = byte(min(255, (int(pixel[c])*255+int(pixel[3])/2)/int(pixel[3])))
			}
		}
		return pixel
	}
	x0, x1 := x*cs.Width, (x+1)*cs.Width
	y0, y1 := y*cs.Height, (y+1)*cs.Height
	var colors [3]int64
	var alpha int64
	for sy := y0 / h; sy < (y1+h-1)/h; sy++ {
		wy := min(y1, (sy+1)*h) - max(y0, sy*h)
		for sx := x0 / w; sx < (x1+w-1)/w; sx++ {
			wx := min(x1, (sx+1)*w) - max(x0, sx*w)
			weight := int64(wx * wy)
			src := (sy*cs.Width + sx) * 4
			a := int64(cs.Image[src+3])
			alpha += a * weight
			if a == 0 {
				continue
			}
			for c := 0; c < 3; c++ {
				color := int64(cs.Image[src+c])
				if cs.Premultiplied {
					colors[c] += color * 255 * weight
				} else {
					colors[c] += color * a * weight
				}
			}
		}
	}
	area := int64(cs.Width * cs.Height)
	pixel := [4]byte{0, 0, 0, byte((alpha + area/2) / area)}
	if pixel[3] != 0 {
		for c := 0; c < 3; c++ {
			pixel[c] = byte(min(255, (colors[c]+alpha/2)/alpha))
		}
	}
	return pixel
}

func (c *client) capturePointer() ([]byte, error) {
	c.s.cursorMu.Lock()
	defer c.s.cursorMu.Unlock()
	select {
	case <-c.done:
		return nil, nil
	default:
	}
	if !c.s.cursorOpen {
		return nil, nil
	}
	var cs *capture.CursorState
	var err error
	if native, ok := c.s.cfg.CursorCapture.(capture.NativeCursorCapture); ok {
		cs, err = native.CaptureNative()
	} else {
		cs, err = c.s.cfg.CursorCapture.Capture()
	}
	if err != nil {
		return nil, err
	}
	sx, sy := c.cursorScale(cs)
	return scaledCursorPointer(cs, c.cursorAlpha, c.cursorLimit, sx, sy)
}

// Native macOS images use backing pixels. Follow the displayed content bounds,
// including capture downscaling and letterboxing, rather than applying client
// DPI again. Geometry is shared with input and read under the same mutex.
func (c *client) cursorScale(cs *capture.CursorState) (float64, float64) {
	if cs == nil || cs.SourceWidth < 1 || cs.SourceHeight < 1 {
		return 1, 1
	}
	c.input.mu.Lock()
	w, h, rect := c.input.w, c.input.h, c.input.contentRect
	c.input.mu.Unlock()
	// Geometry of the frame actually sent wins over a pending client layout.
	// The layout can exceed the source or precede capture reconfiguration.
	ow, oh := w, h
	if ow < 1 || oh < 1 {
		tw, th := c.requestedOutputSize(cs.SourceWidth, cs.SourceHeight)
		ow, oh = capture.BoundedOutputSize(cs.SourceWidth, cs.SourceHeight, tw, th)
	}
	if ow < 1 || oh < 1 {
		return 1, 1
	}
	if rect.Empty() || w != ow || h != oh {
		rect = fittedFrameRect(cs.SourceWidth, cs.SourceHeight, ow, oh, false)
	}
	return float64(rect.Dx()) / float64(cs.SourceWidth), float64(rect.Dy()) / float64(cs.SourceHeight)
}

func (c *client) sendPointer(p []byte) error {
	// Baseline updates and hidden/fallback cursors retain slow-path transport.
	off := 22
	kind := le.Uint16(p[18:])
	if kind != 6 && kind != 8 {
		return c.sendMCS(ioChannel, p)
	}
	if kind == 8 {
		off += 2
	}
	if le.Uint16(p[off+6:]) <= maxPointerSize && le.Uint16(p[off+8:]) <= maxPointerSize {
		return c.sendMCS(ioChannel, p)
	}
	code := byte(9) // FASTPATH_UPDATETYPE_COLOR
	if kind == 8 {
		code = 11 // FASTPATH_UPDATETYPE_POINTER
	}
	return c.write(pointerFastPath(code, p[22:]))
}

// One serialized write keeps all fragments of a pointer update together on
// the TLS stream. Each PDU stays below the protocol's 16,383-byte limit.
func pointerFastPath(code byte, data []byte) []byte {
	const chunkSize = 16000
	var packets []byte
	first := true
	for len(data) > 0 {
		n := min(chunkSize, len(data))
		fragment := byte(3) // NEXT
		if first {
			fragment = 2 // FIRST
		}
		if n == len(data) {
			fragment = 1 // LAST
			if first {
				fragment = 0 // SINGLE
			}
		}
		size := n + 6 // Three-byte PDU header, update header and uint16 size.
		packets = append(packets, 0, 0x80|byte(size>>8), byte(size), code|fragment<<4, byte(n), byte(n>>8))
		packets = append(packets, data[:n]...)
		data, first = data[n:], false
	}
	return packets
}

func (c *client) cursorLoop() {
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	last := arrowPointer() // Already sent after Font Map.
	loggedError := false
	for {
		p, err := c.capturePointer()
		if err != nil {
			if !loggedError {
				logging.Infof("rdp: cursor capture: %v", err)
				loggedError = true
			}
		} else {
			loggedError = false
			// Compare the actual wire image, independently of the capture's
			// Changed flag, so reconnects and other consumers cannot miss it.
			if p != nil && !bytes.Equal(last, p) {
				select {
				case <-c.done:
					return
				default:
				}
				if err := c.sendPointer(p); err != nil {
					c.close()
					return
				}
				last = p
			}
		}
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
	}
}

// Send a real 24-bpp arrow instead of asking a touch client to supply its own
// system cursor. MS-RDPBCGR 2.2.9.1.1.4.4 requires bottom-up, word-aligned masks.
func arrowPointer() []byte {
	const w, h = 16, 24
	const xorStride, andStride = w * 3, 2
	shape := [...]string{
		"X",
		"XX",
		"XOX",
		"XOOX",
		"XOOOX",
		"XOOOOX",
		"XOOOOOX",
		"XOOOOOOX",
		"XOOOOOOOX",
		"XOOOOOOOOX",
		"XOOOOOOOOOX",
		"XOOOOOOOOOOX",
		"XOOOOOOOOOOOX",
		"XOOOOOOXXXXXX",
		"XOOXOOOX",
		"XOX.XOOX",
		"XX..XOOX",
		"X....XOOX",
		".....XOOX",
		"......XX",
	}
	xor := make([]byte, xorStride*h)
	and := make([]byte, andStride*h)
	for n := range and {
		and[n] = 0xff // AND=1, XOR=0 leaves the background unchanged.
	}
	for y, row := range shape {
		for x, pixel := range row {
			if pixel == '.' {
				continue
			}
			offset := (h - 1 - y) * andStride
			and[offset+x/8] &^= 0x80 >> (x % 8)
			if pixel == 'O' {
				offset = (h-1-y)*xorStride + x*3
				xor[offset], xor[offset+1], xor[offset+2] = 0xff, 0xff, 0xff
			}
		}
	}
	// Color pointer, padding, cache index 0, hotspot (0,0), size, mask lengths.
	return shareData(27, join(u16(6), u16(0), u16(0), u16(0), u16(0),
		u16(w), u16(h), u16(len(and)), u16(len(xor)), xor, and))
}
