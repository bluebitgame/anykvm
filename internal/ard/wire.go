package ard

import (
	"bytes"
	"crypto/aes"
	"fmt"
	"io"
	"time"

	"anykvm/internal/capture"
)

func (s *session) handshake(username, password string) error {
	p, err := s.read(12)
	if err != nil {
		return fmt.Errorf("ard: read RFB version: %w", err)
	}
	major, minor := 0, 0
	if n, _ := fmt.Sscanf(string(p), "RFB %03d.%03d\n", &major, &minor); n != 2 || major != 3 || minor < 7 {
		return fmt.Errorf("%w: requires RFB 3.7+ or Apple 3.889", ErrUnsupported)
	}
	version := "RFB 003.008\n"
	if !s.standard {
		if minor != 889 {
			return fmt.Errorf("%w: server did not advertise Apple RFB 3.889", ErrUnsupported)
		}
		version = "RFB 003.889\n"
	}
	if err := s.write([]byte(version)); err != nil {
		return err
	}
	p, err = s.read(1)
	if err != nil {
		return err
	}
	count := int(p[0])
	if count == 0 {
		return fmt.Errorf("ard: server rejected connection")
	}
	types, err := s.read(count)
	if err != nil {
		return err
	}
	chosen := byte(0)
	for _, t := range types {
		if t == 33 {
			chosen = 33
			break
		}
		if t == 30 {
			chosen = 30
		}
	}
	// Do not silently substitute None or password-only VNC for account authentication.
	if chosen == 0 {
		return fmt.Errorf("ard: server offers no supported Apple account authentication (30/33)")
	}
	if chosen == 33 {
		err = s.authMac(username, password)
	} else {
		if err = s.write([]byte{30}); err == nil {
			err = s.authDH(username, password)
		}
	}
	if err != nil {
		return err
	}
	if err = s.authResult(); err != nil {
		return err
	}
	if err = s.write([]byte{1}); err != nil {
		return err
	}
	p, err = s.read(24)
	if err != nil {
		return err
	}
	s.w, s.h = int(be16(p)), int(be16(p[2:]))
	if !validSize(s.w, s.h) && !(s.w == 0 && s.h == 0) {
		return fmt.Errorf("%w: invalid desktop geometry", ErrUnsupported)
	}
	nameLen := int(be32(p[20:]))
	if nameLen > 1<<20 {
		return fmt.Errorf("ard: desktop name exceeds limit")
	}
	// Consume the Apple capability prefix and name without logging remote strings.
	if _, err = s.read(nameLen); err != nil {
		return err
	}
	s.fb = make([]byte, s.w*s.h*4)
	return nil
}

func (s *session) initialize() error {
	pf := make([]byte, 20)
	pf[4], pf[5], pf[7] = 32, 24, 1
	put16(pf[8:], 255)
	put16(pf[10:], 255)
	put16(pf[12:], 255)
	pf[14], pf[15] = 16, 8
	if err := s.write(pf); err != nil {
		return err
	}
	enc := []int32{1, 0, -223, -239}
	if !s.standard {
		enc = []int32{1010, 1, 0, -223, -239, 1103, 1105, 1107, 1109, 1110}
	}
	if err := s.write(encodingMessage(enc)); err != nil {
		return err
	}
	if !s.standard {
		if err := s.write(viewerInfo()); err != nil {
			return err
		}
		if err := s.write(mediaRequest()); err != nil {
			return err
		}
	}
	return s.requestUpdate(false)
}
func (s *session) requestUpdate(incremental bool) error {
	p := make([]byte, 10)
	p[0] = 3
	if incremental {
		p[1] = 1
	}
	s.mu.Lock()
	put16(p[6:], s.w)
	put16(p[8:], s.h)
	s.mu.Unlock()
	return s.write(p)
}

// encryptedReader turns bounded encrypted control records back into a byte
// stream. RFB messages may span records; control envelopes are processed between
// RFB messages, without treating arbitrary chunks as complete messages.
type encryptedReader struct {
	s       *session
	pending *bytes.Reader
}

func (r *encryptedReader) Read(p []byte) (int, error) {
	for r.pending == nil || r.pending.Len() == 0 {
		h, err := readExactly(r.s.conn, 2)
		if err != nil {
			return 0, err
		}
		n := int(be16(h))
		if n < 32 || n%16 != 0 {
			return 0, fmt.Errorf("ard: invalid encrypted record framing")
		}
		b, err := readExactly(r.s.conn, n)
		if err != nil {
			return 0, err
		}
		if r.s.control != nil {
			b, err = r.s.control.decrypt(b)
		} else {
			b, err = legacyDecrypt(r.s.key, b)
		}
		if err != nil {
			return 0, err
		}
		if !r.s.parsingRFB {
			if handled, err := r.s.controlEnvelope(b); err != nil {
				return 0, err
			} else if handled {
				continue
			}
			// The second native control variant retains a two-byte envelope.
			if len(b) >= 6 && b[2] == 0 && b[3] == 0 && be16(b[4:]) > 0 && be16(b[4:]) <= 4096 && be16(b[2:]) == 0 {
				b = b[2:]
			}
		}
		r.pending = bytes.NewReader(b)
	}
	return r.pending.Read(p)
}

func (s *session) receive() {
	defer s.workers.Done()
	for {
		s.parsingRFB = false
		p, err := s.read(1)
		if err != nil {
			s.stop(err)
			return
		}
		s.parsingRFB = true
		switch p[0] {
		case 0:
			err = s.framebufferUpdate()
		case 2:
		case 3:
			var h []byte
			h, err = s.read(7)
			if err == nil {
				_, err = s.read(int(be32(h[3:])))
			}
		default:
			err = fmt.Errorf("ard: unsupported RFB server message %d", p[0])
		}
		if err != nil {
			s.stop(err)
			return
		}
	}
}

func (s *session) framebufferUpdate() error {
	h, err := s.read(3)
	if err != nil {
		return err
	}
	count := int(be16(h[1:]))
	if count > 4096 {
		return fmt.Errorf("ard: too many rectangles")
	}
	dirty := false
	for i := 0; i < count; i++ {
		r, err := s.read(12)
		if err != nil {
			return err
		}
		x, y, w, h := int(be16(r)), int(be16(r[2:])), int(be16(r[4:])), int(be16(r[6:]))
		enc := int32(be32(r[8:]))
		switch enc {
		case -223:
			if !validSize(w, h) {
				return fmt.Errorf("ard: invalid desktop size")
			}
			s.mu.Lock()
			s.w, s.h = w, h
			s.fb = make([]byte, w*h*4)
			s.mu.Unlock()
			if !s.standard {
				if w*h > maxSinglePicturePixels {
					return fmt.Errorf("%w: compound HEVC desktop is unsupported", ErrUnsupported)
				}
			}
		case -239:
			if w > 128 || h > 128 || (w*h > 0 && (x >= w || y >= h)) {
				return fmt.Errorf("ard: invalid cursor geometry")
			}
			p, err := s.read(w*h*4 + ((w+7)/8)*h)
			if err != nil {
				return err
			}
			pixels := bytes.Clone(p[:w*h*4])
			for row := 0; row < h; row++ {
				for col := 0; col < w; col++ {
					alpha := byte(0)
					if p[w*h*4+row*((w+7)/8)+col/8]&(128>>uint(col%8)) != 0 {
						alpha = 255
					}
					pixels[(row*w+col)*4+3] = alpha
				}
			}
			s.mu.Lock()
			s.cursor = &capture.CursorState{Image: pixels, Width: w, Height: h, HotX: x, HotY: y, Changed: true}
			s.mu.Unlock()
		case 1103:
			if s.standard {
				return fmt.Errorf("ard: unsolicited media offer in fallback mode")
			}
			p, err := s.read(36)
			if err != nil {
				return err
			}
			if err = s.acceptOffer(p); err != nil {
				return err
			}
		case 1010, 1105, 1107, 1109, 1110:
			p, err := s.read(2)
			if err != nil {
				return err
			}
			body, err := s.read(int(be16(p)))
			if err != nil {
				return err
			}
			p = append(p, body...)
			if err = s.handleAux(enc, p); err != nil {
				return err
			}
		case 0, 1:
			if !s.standard {
				return fmt.Errorf("%w: upstream supplied ordinary pixels instead of negotiated HEVC", ErrUnsupported)
			}
			s.mu.Lock()
			fw, fh := s.w, s.h
			s.mu.Unlock()
			if w == 0 || h == 0 || x+w > fw || y+h > fh {
				return fmt.Errorf("ard: rectangle outside framebuffer")
			}
			if enc == 0 {
				p, err := s.read(w * h * 4)
				if err != nil {
					return err
				}
				for off := 3; off < len(p); off += 4 {
					p[off] = 255
				}
				for row := 0; row < h; row++ {
					copy(s.fb[((y+row)*fw+x)*4:], p[row*w*4:(row+1)*w*4])
				}
			} else {
				p, err := s.read(4)
				if err != nil {
					return err
				}
				sx, sy := int(be16(p)), int(be16(p[2:]))
				if sx+w > fw || sy+h > fh {
					return fmt.Errorf("ard: CopyRect source outside framebuffer")
				}
				copyRect(s.fb, fw, x, y, w, h, sx, sy)
			}
			dirty = true
		default:
			return fmt.Errorf("ard: unsupported rectangle encoding %d", enc)
		}
	}
	if dirty {
		s.mu.Lock()
		f := &capture.Frame{Format: capture.FormatRaw, Data: bytes.Clone(s.fb), Width: s.w, Height: s.h, CapturedAt: time.Now()}
		s.mu.Unlock()
		s.emit(f)
	}
	if s.standard {
		return s.requestUpdate(true)
	}
	return nil
}
func copyRect(fb []byte, stride, x, y, w, h, sx, sy int) {
	start, end, step := 0, h, 1
	if y > sy {
		start, end, step = h-1, -1, -1
	}
	for row := start; row != end; row += step {
		copy(fb[((y+row)*stride+x)*4:((y+row)*stride+x+w)*4], fb[((sy+row)*stride+sx)*4:((sy+row)*stride+sx+w)*4])
	}
}
func (s *session) acceptOffer(p []byte) error {
	if len(p) != 36 || s.accepted {
		return fmt.Errorf("ard: unsupported media offer transition")
	}
	if be32(p) == 1 {
		b, err := aes.NewCipher(s.key)
		if err != nil {
			return err
		}
		key, iv := make([]byte, 16), make([]byte, 16)
		b.Decrypt(key, p[4:20])
		b.Decrypt(iv, p[20:36])
		c, err := newControlCipher(key, iv)
		if err != nil {
			return err
		}
		s.control = c
	} else {
		if err := s.bindPorts(p); err != nil {
			return err
		}
	}
	answer := []byte{0x12, 0, 0, 2, p[2], p[3], 0, 0}
	if err := s.write(answer); err != nil {
		return err
	}
	s.writeMu.Lock()
	s.accepted = true
	s.writeMu.Unlock()
	s.reader = &encryptedReader{s: s}
	if err := s.write(encodingMessage([]int32{1010, -223, -239, 1105, 1107, 1109, 1110})); err != nil {
		return err
	}
	return s.write(viewerInfo())
}
func (s *session) handleAux(enc int32, p []byte) error {
	switch enc {
	case 1010:
		if s.standard {
			return fmt.Errorf("ard: unsolicited HEVC control in fallback mode")
		}
		if len(p) < 6 {
			return fmt.Errorf("ard: truncated media message")
		}
		kind := be16(p[4:])
		if kind == 1 {
			if !s.accepted {
				return fmt.Errorf("ard: media message before keyed encryption offer")
			}
			if err := s.bindPorts(p[2:]); err != nil {
				return err
			}
			s.configured = false
			return s.configureMedia()
		}
		if kind == 2 {
			return s.write(viewerInfo())
		}
		return fmt.Errorf("ard: unsupported AVC media message %d", kind)
	case 1105:
		if len(p) < 22 {
			return fmt.Errorf("ard: truncated DisplayInfo2")
		}
		count := int(be16(p[20:]))
		if count < 1 || count > 16 || len(p) < 22+count*56 {
			return fmt.Errorf("ard: invalid DisplayInfo2 layout")
		}
		if !s.standard && count != 1 {
			return fmt.Errorf("%w: multiple upstream displays require compound HEVC support", ErrUnsupported)
		}
		id := be32(p[38:])
		bounds := p[50:58]
		w, h := int(be16(bounds[6:]))-int(be16(bounds[2:])), int(be16(bounds[4:]))-int(be16(bounds))
		if !validSize(w, h) {
			return fmt.Errorf("ard: invalid DisplayInfo2 dimensions")
		}
		if !s.standard && w*h > maxSinglePicturePixels {
			return fmt.Errorf("%w: compound HEVC desktop is unsupported", ErrUnsupported)
		}
		s.mu.Lock()
		if s.w != w || s.h != h {
			s.w, s.h = w, h
			s.fb = make([]byte, w*h*4)
		}
		s.mu.Unlock()
		if !s.displaySelected {
			p := make([]byte, 8)
			p[0] = 13
			put32(p[4:], id)
			s.displaySelected = true
			return s.write(p)
		}
	case 1109, 1110:
		if s.standard {
			return fmt.Errorf("ard: unsolicited media control in fallback mode")
		}
		auto := make([]byte, 16)
		auto[0] = 9
		put16(auto[2:], 1)
		interval := 0
		if s.fps > 0 {
			interval = max(1, 1000/s.fps)
		}
		put32(auto[4:], uint32(interval))
		s.mu.Lock()
		put16(auto[12:], s.w)
		put16(auto[14:], s.h)
		s.mu.Unlock()
		if err := s.write(auto); err != nil {
			return err
		}
		if enc == 1110 {
			return s.configureMedia()
		}
	case 1107: // Stream statistics/control; bounded and fully consumed.
	case 1104: // Native media-control envelope, distinct from an RFB cursor record.
		if !s.reconfigurationRequested {
			s.reconfigurationRequested = true
			return s.write(mediaRequest())
		}
	}
	return nil
}
func (s *session) controlEnvelope(p []byte) (bool, error) {
	if len(p) == 0 {
		return true, nil
	}
	// Ordinary RFB is read as a stream, including continuation records.
	if p[0] == 0 || p[0] == 2 || p[0] == 3 {
		return false, nil
	}
	for _, offset := range []int{14, 16} {
		if len(p) < offset+4 {
			continue
		}
		enc := int32(be16(p[offset:]))
		n := int(be16(p[offset+2:]))
		if n < 2 || n > len(p)-offset-2 {
			continue
		}
		if enc == 1010 || enc == 1104 || enc == 1105 || enc == 1107 || enc == 1109 || enc == 1110 {
			return true, s.handleAux(enc, p[offset+2:offset+2+n])
		}
	}
	return false, nil
}

var _ io.Reader = (*encryptedReader)(nil)
