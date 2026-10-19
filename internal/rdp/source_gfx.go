package rdp

import (
	"bytes"
	"fmt"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

func sourceDVCHeader(cmd byte, ch uint32) []byte {
	width := byte(0)
	if ch > 255 {
		width = 1
	}
	if ch > 65535 {
		width = 2
	}
	return join([]byte{cmd<<4 | width}, u32(ch)[:1<<width])
}
func (s *sourceSession) sendSourceGFX(p []byte) error {
	// Client-to-server GFX PDUs are not wrapped in a ZGFX container.
	return s.sendVC(s.dvc, join(sourceDVCHeader(3, s.gfxChannelID), p))
}
func (s *sourceSession) handleSourceDVC(p []byte) error {
	if len(p) == 0 {
		return errProtocol
	}
	header := p[0]
	p = p[1:]
	cmd := header >> 4
	if cmd == 5 {
		if len(p) != 3 && len(p) != 11 {
			return errProtocol
		}
		v := le.Uint16(p[1:])
		if v < 1 {
			return errProtocol
		}
		s.dvcVersion = min(v, uint16(2))
		return s.sendVC(s.dvc, join([]byte{0x50, 0}, u16(int(s.dvcVersion))))
	}
	ch, p, e := dvcInteger(p, header&3)
	if e != nil {
		return e
	}
	if cmd == 1 {
		if s.dvcVersion == 0 {
			return errProtocol
		}
		if !bytes.Equal(p, []byte("Microsoft::Windows::RDS::Graphics\x00")) {
			return s.sendVC(s.dvc, join(sourceDVCHeader(1, ch), u32(0xc0000001)))
		}
		s.gfxChannelID = ch
		if e = s.sendVC(s.dvc, join(sourceDVCHeader(1, ch), u32(0))); e != nil {
			return e
		}
		// Version 8.1 advertises AVC420, avoiding the mandatory AVC444 modes of
		// later capabilities. THINCLIENT requests the AVC path from Windows.
		return s.sendSourceGFX(gfxPDU(0x12, join(u16(1), u32(0x80105), u32(4), u32(0x11))))
	}
	if ch != s.gfxChannelID {
		return errProtocol
	}
	switch cmd {
	case 2:
		total, rest, e := dvcInteger(p, (header>>2)&3)
		if e != nil {
			return e
		}
		data, e := s.dynamic.append(int(total), true, len(rest) == int(total), rest)
		if e != nil || data == nil {
			return e
		}
		return s.sourceGFXContainer(data)
	case 3:
		if s.dynamic.total == 0 {
			return s.sourceGFXContainer(p)
		}
		data, e := s.dynamic.append(s.dynamic.total, false, len(s.dynamic.data)+len(p) == s.dynamic.total, p)
		if e != nil || data == nil {
			return e
		}
		return s.sourceGFXContainer(data)
	case 4:
		return fmt.Errorf("RDP server closed graphics channel")
	default:
		return fmt.Errorf("unsupported RDP source DVC command %d", cmd)
	}
}
func (s *sourceSession) sourceGFXContainer(p []byte) error {
	data, e := s.zgfx.decode(p)
	if e != nil {
		return e
	}
	return s.handleSourceGFX(data)
}
func validSourceSize(w, h int) bool { return w > 0 && h > 0 && w <= 8192 && h <= 8192 && w*h <= 16<<20 }
func (s *sourceSession) handleSourceGFX(p []byte) error {
	for len(p) > 0 {
		if len(p) < 8 {
			return errProtocol
		}
		kind, n := le.Uint16(p), int(le.Uint32(p[4:]))
		if n < 8 || n > len(p) || le.Uint16(p[2:]) != 0 {
			return errProtocol
		}
		b := p[8:n]
		p = p[n:]
		switch kind {
		case 0x13:
			if len(b) != 12 || le.Uint32(b) != 0x80105 || le.Uint32(b[4:]) != 4 || le.Uint32(b[8:])&0x10 == 0 {
				return fmt.Errorf("RDP server did not select AVC420 capability version 8.1")
			}
			s.conn.SetReadDeadline(time.Time{})
			s.readyOnce.Do(func() { close(s.ready) })
		case 0xe:
			if len(b) != 332 {
				return errProtocol
			}
			w, h := int(le.Uint32(b)), int(le.Uint32(b[4:]))
			if !validSourceSize(w, h) {
				return errProtocol
			}
			s.inputMu.Lock()
			s.width, s.height = w, h
			s.inputMu.Unlock()
			s.mappedOK = false
		case 9:
			if len(b) != 7 || b[6] != 0x20 {
				return errProtocol
			}
			id, w, h := le.Uint16(b), int(le.Uint16(b[2:])), int(le.Uint16(b[4:]))
			if !validSourceSize(w, h) || s.surfaces[id] != nil || len(s.surfaces) >= 4 {
				return fmt.Errorf("unsupported RDP surface geometry/count")
			}
			s.surfaces[id] = &sourceSurface{w: w, h: h, pixels: make([]byte, w*h*4)}
		case 0xa:
			if len(b) != 2 {
				return errProtocol
			}
			id := le.Uint16(b)
			if sf := s.surfaces[id]; sf != nil {
				if sf.decoder != nil && sf.decoder != s.decoder {
					sf.decoder.Close()
				}
				delete(s.surfaces, id)
			}
			if s.mapped == id {
				s.mappedOK = false
			}
		case 0xf:
			if len(b) != 12 {
				return errProtocol
			}
			id := le.Uint16(b)
			if s.surfaces[id] == nil {
				return errProtocol
			}
			x, y := int(le.Uint32(b[4:])), int(le.Uint32(b[8:]))
			sf := s.surfaces[id]
			if x+sf.w > s.width || y+sf.h > s.height {
				return fmt.Errorf("RDP surface does not fit desktop")
			}
			s.mapped, s.originX, s.originY, s.mappedOK = id, x, y, true
		case 0xb:
			if len(b) != 8 || s.inFrame {
				return errProtocol
			}
			s.frameID = le.Uint32(b[4:])
			s.inFrame = true
			s.dirty = false
		case 0xc:
			if len(b) != 4 || !s.inFrame || le.Uint32(b) != s.frameID {
				return errProtocol
			}
			s.inFrame = false
			s.decodedFrames++
			if s.dirty {
				s.publishSourceFrame()
			}
			if e := s.sendSourceGFX(gfxPDU(0xd, join(u32(0), u32(s.frameID), u32(s.decodedFrames)))); e != nil {
				return e
			}
		case 1:
			if e := s.sourceAVC420(b); e != nil {
				return e
			}
		case 3: // DELETE_ENCODING_CONTEXT (AVC420 uses an implicit context).
			if len(b) != 6 {
				return errProtocol
			}
			sf := s.surfaces[le.Uint16(b)]
			if sf != nil && sf.decoder != nil {
				sf.decoder.Close()
				sf.decoder = nil
			}
		case 0x11: // CACHE_IMPORT_REPLY: no persistent cache was offered.
			if len(b) < 2 || len(b) != 2+int(le.Uint16(b))*2 {
				return errProtocol
			}
		default:
			return fmt.Errorf("RDP H.264 source does not support graphics command %#x", kind)
		}
	}
	return nil
}
func (s *sourceSession) sourceAVC420(b []byte) error {
	if len(b) < 21 || !s.inFrame {
		return errProtocol
	}
	sf := s.surfaces[le.Uint16(b)]
	if sf == nil || le.Uint16(b[2:]) != uint16(codecAVC420) || b[4] != 0x20 {
		return fmt.Errorf("RDP source only supports H.264/AVC420 surfaces")
	}
	left, top, right, bottom := int(le.Uint16(b[5:])), int(le.Uint16(b[7:])), int(le.Uint16(b[9:])), int(le.Uint16(b[11:]))
	size := int(le.Uint32(b[13:]))
	if size != len(b)-17 || right <= left || bottom <= top || right > sf.w || bottom > sf.h {
		return errProtocol
	}
	avc := b[17:]
	count := int(le.Uint32(avc))
	if count > (len(avc)-4)/10 {
		return errProtocol
	}
	offset := 4 + count*10
	type rect struct{ x, y, r, b int }
	rects := make([]rect, count)
	for i := range rects {
		r := avc[4+i*8:]
		v := rect{int(le.Uint16(r)), int(le.Uint16(r[2:])), int(le.Uint16(r[4:])), int(le.Uint16(r[6:]))}
		if v.x < left || v.y < top || v.r <= v.x || v.b <= v.y || v.r > right || v.b > bottom {
			return errProtocol
		}
		rects[i] = v
	}
	if sf.decoder == nil {
		if len(s.surfaces) == 1 && s.decoder != nil {
			sf.decoder = s.decoder
			s.decoder = nil
		} else {
			var e error
			sf.decoder, e = videodec.NewH264()
			if e != nil {
				return e
			}
		}
	}
	decoded, e := sf.decoder.Decode(avc[offset:])
	if e != nil || decoded == nil {
		return e
	}
	defer decoded.Release()
	if !validSourceSize(decoded.Width, decoded.Height) || len(decoded.Data) != decoded.Width*decoded.Height*4 {
		return fmt.Errorf("invalid decoded RDP H.264 frame")
	}
	// AVC420 pictures use surface coordinates. Only metadata regions are valid;
	// other decoder pixels may be padding or stale desktop content.
	if decoded.Width < right || decoded.Height < bottom {
		return fmt.Errorf("RDP H.264 picture does not cover update rectangles")
	}
	for _, r := range rects {
		for y := r.y; y < r.b; y++ {
			copy(sf.pixels[(y*sf.w+r.x)*4:(y*sf.w+r.r)*4], decoded.Data[(y*decoded.Width+r.x)*4:(y*decoded.Width+r.r)*4])
		}
	}
	s.dirty = true
	return nil
}
func (s *sourceSession) publishSourceFrame() {
	sf := s.surfaces[s.mapped]
	if !s.mappedOK || sf == nil {
		return
	}
	pixels := make([]byte, s.width*s.height*4)
	for y := 0; y < sf.h; y++ {
		copy(pixels[((y+s.originY)*s.width+s.originX)*4:], sf.pixels[y*sf.w*4:(y+1)*sf.w*4])
	}
	f := &capture.Frame{Format: capture.FormatRaw, Data: pixels, Width: s.width, Height: s.height, SourceWidth: s.width, SourceHeight: s.height, CapturedAt: time.Now()}
	select {
	case s.frames <- f:
	default:
		select {
		case <-s.frames:
		default:
		}
		select {
		case s.frames <- f:
		case <-s.done:
		}
	}
}
