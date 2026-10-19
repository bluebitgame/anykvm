package rdp

import "fmt"

const liteWindow = 8192

// rdp8LiteDecoder implements the DVC variant of MS-RDPEGFX 3.1.9.1.
// A separate decoder belongs to each dynamic channel and each connection.
type rdp8LiteDecoder struct {
	history [liteWindow]byte
	cursor  int
}

type liteToken struct {
	width, prefix, extra, base int
	literal                    bool
}

// These functional wire values come from MS-RDPEGFX 3.1.9.1.2.4.
// Distance ranges entirely beyond the DVC history window are invalid here.
var liteTokens = [...]liteToken{
	{5, 17, 5, 0, false}, {5, 18, 7, 32, false},
	{5, 19, 9, 160, false}, {5, 20, 10, 672, false},
	{5, 21, 12, 1696, false}, {6, 44, 14, 5792, false},
	{5, 24, 0, 0x00, true}, {5, 25, 0, 0x01, true},
	{6, 52, 0, 0x02, true}, {6, 53, 0, 0x03, true},
	{6, 54, 0, 0xff, true},
	{7, 110, 0, 0x04, true}, {7, 111, 0, 0x05, true},
	{7, 112, 0, 0x06, true}, {7, 113, 0, 0x07, true},
	{7, 114, 0, 0x08, true}, {7, 115, 0, 0x09, true},
	{7, 116, 0, 0x0a, true}, {7, 117, 0, 0x0b, true},
	{7, 118, 0, 0x3a, true}, {7, 119, 0, 0x3b, true},
	{7, 120, 0, 0x3c, true}, {7, 121, 0, 0x3d, true},
	{7, 122, 0, 0x3e, true}, {7, 123, 0, 0x3f, true},
	{7, 124, 0, 0x40, true}, {7, 125, 0, 0x80, true},
	{8, 252, 0, 0x0c, true}, {8, 253, 0, 0x38, true},
	{8, 254, 0, 0x39, true}, {8, 255, 0, 0x66, true},
}

type liteBits struct {
	data     []byte
	pos, end int
}

func (b *liteBits) read(n int) (int, error) {
	if n < 0 || n > 21 || n > b.end-b.pos {
		return 0, fmt.Errorf("%w: truncated RDP8 Lite token", errProtocol)
	}
	v := 0
	for i := 0; i < n; i++ {
		v = v<<1 | int((b.data[b.pos/8]>>uint(7-b.pos%8))&1)
		b.pos++
	}
	return v, nil
}

func (b *liteBits) token() (liteToken, int, error) {
	prefix := 0
	for width := 1; width <= 8; width++ {
		bit, err := b.read(1)
		if err != nil {
			return liteToken{}, 0, err
		}
		prefix = prefix<<1 | bit
		if width == 1 && prefix == 0 {
			v, err := b.read(8)
			if err != nil {
				return liteToken{}, 0, err
			}
			for _, t := range liteTokens {
				if t.literal && t.base == v {
					return liteToken{}, 0, fmt.Errorf("%w: reserved RDP8 Lite literal encoding", errProtocol)
				}
			}
			return liteToken{literal: true}, v, nil
		}
		for _, t := range liteTokens {
			if t.width == width && t.prefix == prefix {
				v, err := b.read(t.extra)
				return t, t.base + v, err
			}
		}
	}
	return liteToken{}, 0, fmt.Errorf("%w: reserved or out-of-window RDP8 Lite token", errProtocol)
}

func (b *liteBits) matchLength() (int, error) {
	bit, err := b.read(1)
	if err != nil || bit == 0 {
		return 3, err
	}
	width := 2
	for {
		bit, err = b.read(1)
		if err != nil {
			return 0, err
		}
		if bit == 0 {
			v, err := b.read(width)
			return (1 << uint(width)) + v, err
		}
		width++
		if width > 13 {
			return 0, fmt.Errorf("%w: RDP8 Lite match exceeds 8192 bytes", errProtocol)
		}
	}
}

func (d *rdp8LiteDecoder) record(v byte) {
	d.history[d.cursor] = v
	d.cursor = (d.cursor + 1) & (liteWindow - 1)
}

// decode commits history only after a complete valid segment. DVC compressed
// PDUs contain one segment, with an 8192-byte expansion and history limit.
func (d *rdp8LiteDecoder) decode(p []byte) ([]byte, error) {
	if len(p) < 2 || len(p) > vcChunk {
		return nil, fmt.Errorf("%w: RDP8 Lite container length %d", errProtocol, len(p))
	}
	expected := -1
	switch p[0] {
	case 0xe0:
		p = p[1:]
	case 0xe1:
		if len(p) < 12 || le.Uint16(p[1:]) != 1 ||
			le.Uint32(p[3:]) > liteWindow || int(le.Uint32(p[7:])) != len(p)-11 {
			return nil, fmt.Errorf("%w: invalid single-segment RDP8 Lite multipart container", errProtocol)
		}
		expected = int(le.Uint32(p[3:]))
		p = p[11:]
	default:
		return nil, fmt.Errorf("%w: RDP8 Lite descriptor %#x", errProtocol, p[0])
	}
	if p[0]&0xf != 6 {
		return nil, fmt.Errorf("%w: RDP8 Lite compression type %#x", errProtocol, p[0]&0xf)
	}
	compressed := p[0]&0x20 != 0
	p = p[1:]
	next := *d
	var out []byte
	if !compressed {
		if len(p) > liteWindow {
			return nil, fmt.Errorf("%w: RDP8 Lite raw segment exceeds 8192 bytes", errProtocol)
		}
		out = append([]byte(nil), p...)
		for _, v := range out {
			next.record(v)
		}
	} else {
		if len(p) == 0 || p[len(p)-1] > 7 || (len(p)-1)*8 < int(p[len(p)-1]) {
			return nil, fmt.Errorf("%w: invalid RDP8 Lite bit-stream trailer", errProtocol)
		}
		bits := liteBits{data: p[:len(p)-1], end: (len(p)-1)*8 - int(p[len(p)-1])}
		for bits.pos < bits.end {
			token, value, err := bits.token()
			if err != nil {
				return nil, err
			}
			if token.literal {
				if len(out) == liteWindow {
					return nil, fmt.Errorf("%w: RDP8 Lite output exceeds 8192 bytes", errProtocol)
				}
				out = append(out, byte(value))
				next.record(byte(value))
				continue
			}
			if value == 0 {
				count, err := bits.read(15)
				if err != nil {
					return nil, err
				}
				bits.pos = (bits.pos + 7) &^ 7
				if count > liteWindow-len(out) || bits.pos > bits.end || count*8 > bits.end-bits.pos {
					return nil, fmt.Errorf("%w: invalid RDP8 Lite unencoded run", errProtocol)
				}
				raw := bits.data[bits.pos/8 : bits.pos/8+count]
				out = append(out, raw...)
				for _, v := range raw {
					next.record(v)
				}
				bits.pos += count * 8
				continue
			}
			if value > liteWindow {
				return nil, fmt.Errorf("%w: RDP8 Lite match distance %d", errProtocol, value)
			}
			count, err := bits.matchLength()
			if err != nil {
				return nil, err
			}
			if count > liteWindow-len(out) {
				return nil, fmt.Errorf("%w: RDP8 Lite output exceeds 8192 bytes", errProtocol)
			}
			for i := 0; i < count; i++ {
				v := next.history[(next.cursor-value+liteWindow)&(liteWindow-1)]
				out = append(out, v)
				next.record(v)
			}
		}
	}
	if expected >= 0 && len(out) != expected {
		return nil, fmt.Errorf("%w: RDP8 Lite multipart output length %d, expected %d", errProtocol, len(out), expected)
	}
	*d = next
	return out, nil
}
