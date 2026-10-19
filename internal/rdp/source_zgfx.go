package rdp

import "fmt"

// ZGFX shares the MS-RDPEGFX token grammar with RDP8 Lite, with a larger
// 2,500,000-byte history and 65,535-byte segments. Functional token values
// follow MS-RDPEGFX 3.1.9.1.2.3; no external codec dependency is used.
const zgfxWindow = 2500000

type zgfxDecoder struct {
	history []byte
	cursor  int
}

var zgfxExtra = []liteToken{{6, 45, 15, 22176, false}, {7, 92, 18, 54944, false}, {7, 93, 20, 317088, false}, {8, 188, 20, 1365664, false}, {8, 189, 21, 2414240, false}}

func (d *zgfxDecoder) record(v byte) { d.history[d.cursor] = v; d.cursor = (d.cursor + 1) % zgfxWindow }
func zgfxToken(b *liteBits) (liteToken, int, error) {
	prefix := 0
	for width := 1; width <= 8; width++ {
		v, e := b.read(1)
		if e != nil {
			return liteToken{}, 0, e
		}
		prefix = prefix<<1 | v
		if width == 1 && prefix == 0 {
			v, e = b.read(8)
			return liteToken{literal: true}, v, e
		}
		for _, tokens := range [][]liteToken{liteTokens[:], zgfxExtra} {
			for _, t := range tokens {
				if t.width == width && t.prefix == prefix {
					v, e = b.read(t.extra)
					return t, t.base + v, e
				}
			}
		}
	}
	return liteToken{}, 0, errProtocol
}
func (d *zgfxDecoder) segment(p []byte) ([]byte, error) {
	if len(p) < 1 || len(p) > 66536 || p[0]&15 != 4 {
		return nil, fmt.Errorf("invalid RDP ZGFX segment")
	}
	if d.history == nil {
		d.history = make([]byte, zgfxWindow)
	}
	compressed := p[0]&0x20 != 0
	p = p[1:]
	var out []byte
	if !compressed {
		if len(p) > 65535 {
			return nil, errProtocol
		}
		for _, v := range p {
			d.record(v)
		}
		return append([]byte(nil), p...), nil
	}
	if len(p) == 0 || p[len(p)-1] > 7 {
		return nil, errProtocol
	}
	bits := liteBits{data: p[:len(p)-1], end: (len(p)-1)*8 - int(p[len(p)-1])}
	if bits.end < 0 {
		return nil, errProtocol
	}
	for bits.pos < bits.end {
		token, value, e := zgfxToken(&bits)
		if e != nil {
			return nil, e
		}
		if token.literal {
			if len(out) >= 65535 {
				return nil, errProtocol
			}
			out = append(out, byte(value))
			d.record(byte(value))
			continue
		}
		if value == 0 {
			n, e := bits.read(15)
			if e != nil {
				return nil, e
			}
			bits.pos = (bits.pos + 7) &^ 7
			if n > 65535-len(out) || bits.pos > bits.end || n*8 > bits.end-bits.pos {
				return nil, errProtocol
			}
			for _, v := range bits.data[bits.pos/8 : bits.pos/8+n] {
				out = append(out, v)
				d.record(v)
			}
			bits.pos += n * 8
			continue
		}
		if value > zgfxWindow {
			return nil, errProtocol
		}
		first, e := bits.read(1)
		if e != nil {
			return nil, e
		}
		count := 3
		if first != 0 {
			width := 2
			for {
				v, e := bits.read(1)
				if e != nil {
					return nil, e
				}
				if v == 0 {
					extra, e := bits.read(width)
					if e != nil {
						return nil, e
					}
					count = (1 << width) + extra
					break
				}
				width++
				if width > 15 {
					return nil, errProtocol
				}
			}
		}
		if count > 65535-len(out) {
			return nil, errProtocol
		}
		for i := 0; i < count; i++ {
			v := d.history[(d.cursor-value+zgfxWindow)%zgfxWindow]
			out = append(out, v)
			d.record(v)
		}
	}
	return out, nil
}
func (d *zgfxDecoder) decode(p []byte) ([]byte, error) {
	if len(p) < 2 || len(p) > maxMessage+4096 {
		return nil, errProtocol
	}
	if p[0] == 0xe0 {
		return d.segment(p[1:])
	}
	if p[0] != 0xe1 || len(p) < 7 {
		return nil, errProtocol
	}
	count, total := int(le.Uint16(p[1:])), int(le.Uint32(p[3:]))
	if count == 0 || total > maxMessage {
		return nil, errProtocol
	}
	p = p[7:]
	out := make([]byte, 0, total)
	for i := 0; i < count; i++ {
		if len(p) < 4 {
			return nil, errProtocol
		}
		n := int(le.Uint32(p))
		p = p[4:]
		if n < 1 || n > len(p) {
			return nil, errProtocol
		}
		part, e := d.segment(p[:n])
		if e != nil {
			return nil, e
		}
		if len(part) > total-len(out) {
			return nil, errProtocol
		}
		out = append(out, part...)
		p = p[n:]
	}
	if len(p) != 0 || len(out) != total {
		return nil, errProtocol
	}
	return out, nil
}
