// Package rdp implements TLS RDP with the MS-RDPEGFX graphics pipeline,
// including AVC420, ClearCodec, Planar YCoCg420 and Progressive bitmap output.
package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

const (
	ioChannel     = 1003
	userChannel   = 1002
	serverChannel = 1001
	shareID       = 0x10000
	maxMessage    = 16 << 20
	vcChunk       = 1600
)

var le = binary.LittleEndian
var be = binary.BigEndian
var errProtocol = errors.New("invalid RDP message")

func u16(v int) []byte        { p := make([]byte, 2); le.PutUint16(p, uint16(v)); return p }
func u32(v uint32) []byte     { p := make([]byte, 4); le.PutUint32(p, v); return p }
func join(p ...[]byte) []byte { return bytes.Join(p, nil) }

func perLength(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	return []byte{0x80 | byte(n>>8), byte(n)}
}

func takePer(p []byte) (int, []byte, error) {
	if len(p) == 0 {
		return 0, nil, errProtocol
	}
	if p[0]&0x80 == 0 {
		return int(p[0]), p[1:], nil
	}
	if len(p) < 2 {
		return 0, nil, errProtocol
	}
	return int(p[0]&0x7f)<<8 | int(p[1]), p[2:], nil
}

func berValue(tag []byte, p []byte) []byte {
	n := len(p)
	l := []byte{byte(n)}
	if n >= 128 {
		if n <= 255 {
			l = []byte{0x81, byte(n)}
		} else {
			l = []byte{0x82, byte(n >> 8), byte(n)}
		}
	}
	return join(tag, l, p)
}

func berInt(n int) []byte {
	p := []byte{byte(n)}
	if n > 127 {
		p = []byte{byte(n >> 8), byte(n)}
		if p[0]&0x80 != 0 {
			p = append([]byte{0}, p...)
		}
	}
	return berValue([]byte{2}, p)
}

// takeBER accepts definite lengths only, with a bounded enclosing buffer.
func takeBER(p []byte, tag []byte) ([]byte, []byte, error) {
	if len(p) < len(tag)+1 || !bytes.Equal(p[:len(tag)], tag) {
		return nil, nil, errProtocol
	}
	p = p[len(tag):]
	n, off := int(p[0]), 1
	if n&128 != 0 {
		count := n & 127
		if count < 1 || count > 3 || len(p) < 1+count {
			return nil, nil, errProtocol
		}
		n = 0
		for _, b := range p[1 : 1+count] {
			n = n<<8 | int(b)
		}
		off += count
	}
	if n > len(p)-off {
		return nil, nil, errProtocol
	}
	return p[off : off+n], p[off+n:], nil
}

// readPacket separates TPKT/X.224 data from fast-path input on the TLS stream.
func readPacket(r io.Reader) ([]byte, bool, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, false, err
	}
	fast := h[0] != 3
	n := int(h[1])
	header := 2
	if !fast {
		if h[1] != 0 {
			return nil, false, errProtocol
		}
		t := make([]byte, 2)
		if _, err := io.ReadFull(r, t); err != nil {
			return nil, false, err
		}
		n, header = int(be.Uint16(t)), 4
	} else if n&128 != 0 {
		var t [1]byte
		if _, err := io.ReadFull(r, t[:]); err != nil {
			return nil, true, err
		}
		n, header = (n&127)<<8|int(t[0]), 3
	}
	if n < header || n > 65535 {
		return nil, fast, errProtocol
	}
	p := make([]byte, n-header)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, fast, err
	}
	if fast {
		return append([]byte{h[0]}, p...), true, nil
	}
	return p, false, nil
}

func tpkt(p []byte) []byte {
	h := []byte{3, 0, 0, 0}
	be.PutUint16(h[2:], uint16(len(p)+4))
	return append(h, p...)
}

func parseMCS(p []byte) (uint16, []byte, error) {
	if len(p) < 10 || !bytes.Equal(p[:3], []byte{2, 0xf0, 0x80}) || p[3] != 0x64 || be.Uint16(p[4:6])+1001 != userChannel {
		return 0, nil, errProtocol
	}
	ch := be.Uint16(p[6:8])
	n, data, err := takePer(p[9:])
	if err != nil || n != len(data) {
		return 0, nil, errProtocol
	}
	return ch, data, nil
}

type clientData struct {
	width, height int
	scalePercent  int
	clientBuild   uint32
	channels      []string
	dvc           uint16
	clipboard     uint16
	drives        uint16
	audio         uint16
}

func parseConnect(p []byte) (clientData, error) {
	var c clientData
	if len(p) < 3 || !bytes.Equal(p[:3], []byte{2, 0xf0, 0x80}) {
		return c, errProtocol
	}
	value, rest, err := takeBER(p[3:], []byte{0x7f, 0x65})
	if err != nil || len(rest) != 0 {
		return c, errProtocol
	}
	for _, tag := range []byte{4, 4, 1, 0x30, 0x30, 0x30} {
		_, value, err = takeBER(value, []byte{tag})
		if err != nil {
			return c, err
		}
	}
	gcc, rest, err := takeBER(value, []byte{4})
	if err != nil || len(rest) != 0 {
		return c, errProtocol
	}
	idx := bytes.Index(gcc, []byte("Duca"))
	if idx < 0 {
		return c, errProtocol
	}
	n, blocks, err := takePer(gcc[idx+4:])
	if err != nil || n != len(blocks) {
		return c, errProtocol
	}
	gfx := false
	extendedScale := 0
	for len(blocks) > 0 {
		if len(blocks) < 4 {
			return c, errProtocol
		}
		kind, size := le.Uint16(blocks), int(le.Uint16(blocks[2:]))
		if size < 4 || size > len(blocks) {
			return c, errProtocol
		}
		b := blocks[4:size]
		switch kind {
		case 0xc001:
			if len(b) < 142 {
				return c, errors.New("RDP 8 graphics capabilities required")
			}
			c.width, c.height = int(le.Uint16(b[4:])), int(le.Uint16(b[6:]))
			c.clientBuild = le.Uint32(b[16:])
			gfx = le.Uint16(b[140:])&0x100 != 0
			// Optional DPI fields follow physical width/height and orientation.
			// Require both scale fields; incomplete or invalid DPI is ignored.
			// Payload offsets exclude the four-byte GCC user data header.
			if len(b) >= 230 {
				c.scalePercent = desktopScalePercent(le.Uint32(b[222:]), le.Uint32(b[226:]))
			}
		case 0xc003:
			if len(b) < 4 {
				return c, errProtocol
			}
			count := int(le.Uint32(b))
			if count > 31 || len(b) != 4+12*count {
				return c, errProtocol
			}
			for i := 0; i < count; i++ {
				name := string(bytes.TrimRight(b[4+i*12:12+i*12], "\x00"))
				c.channels = append(c.channels, name)
				if name == "drdynvc" {
					c.dvc = uint16(1004 + i)
				}
				if name == "cliprdr" {
					if c.clipboard != 0 {
						return c, errors.New("duplicate clipboard channel")
					}
					c.clipboard = uint16(1004 + i)
				}
				if name == "rdpdr" {
					if c.drives != 0 {
						return c, errors.New("duplicate drive channel")
					}
					c.drives = uint16(1004 + i)
				}
				if name == "rdpsnd" {
					if c.audio != 0 {
						return c, errors.New("duplicate audio channel")
					}
					c.audio = uint16(1004 + i)
				}
			}
		case 0xc008:
			// Single-monitor attributes can provide DPI even when CS_CORE
			// omits it. Data blocks may arrive in either order.
			if len(b) == 32 && le.Uint32(b) == 0 && le.Uint32(b[4:]) == 20 && le.Uint32(b[8:]) == 1 {
				extendedScale = desktopScalePercent(le.Uint32(b[24:]), le.Uint32(b[28:]))
			}
		}
		blocks = blocks[size:]
	}
	if extendedScale > 0 {
		c.scalePercent = extendedScale
	}
	if !gfx || c.dvc == 0 {
		return c, errors.New("RDPGFX client required")
	}
	if c.width < 2 || c.height < 2 || c.width > 8192 || c.height > 8192 {
		return c, errors.New("invalid desktop size")
	}
	return c, nil
}

func connectResponse(c clientData, requested uint32) []byte {
	core := join(u32(0x80004), u32(requested), u32(0))
	netdata := join(u16(ioChannel), u16(len(c.channels)))
	for i := range c.channels {
		netdata = append(netdata, u16(1004+i)...)
	}
	if len(c.channels)%2 != 0 {
		netdata = append(netdata, 0, 0)
	}
	blocks := join(capability(0xc01, core), capability(0xc02, make([]byte, 8)), capability(0xc03, netdata))
	gcc := join([]byte{0, 5, 0, 0x14, 0x7c, 0, 1, 0x2a, 0x14, 0x76, 0x0a, 1, 1, 0, 1, 0xc0, 0}, []byte("McDn"), perLength(len(blocks)), blocks)
	domain := []byte{}
	for _, n := range []int{34, 2, 0, 1, 0, 1, 65535, 2} {
		domain = append(domain, berInt(n)...)
	}
	return berValue([]byte{0x7f, 0x66}, join([]byte{0x0a, 1, 0}, berInt(0), berValue([]byte{0x30}, domain), berValue([]byte{4}, gcc)))
}

func capability(kind int, payload []byte) []byte {
	return join(u16(kind), u16(4+len(payload)), payload)
}
func shareControl(kind int, p []byte) []byte {
	return join(u16(6+len(p)), u16(0x10|kind), u16(serverChannel), p)
}
func shareData(kind byte, p []byte) []byte {
	h := join(u32(shareID), []byte{0, 1}, u16(12+len(p)), []byte{kind, 0, 0, 0})
	return shareControl(7, join(h, p))
}

func demandActive(w, h int) []byte {
	general := make([]byte, 20)
	le.PutUint16(general[4:], 0x200)
	le.PutUint16(general[10:], 0x405) // Include FASTPATH_OUTPUT_SUPPORTED for large cursors.
	bitmap := make([]byte, 24)
	le.PutUint16(bitmap, 32)
	le.PutUint16(bitmap[8:], uint16(w))
	le.PutUint16(bitmap[10:], uint16(h))
	le.PutUint16(bitmap[14:], 1)
	le.PutUint16(bitmap[16:], 1)
	bitmap[19] = 0x0e // Dynamic color fidelity, chroma subsampling, skip alpha.
	input := make([]byte, 84)
	le.PutUint16(input, 0x39) // Scancodes, Unicode, and fast-path input.
	order := make([]byte, 84)
	le.PutUint16(order[30:], 2) // Negotiate zero drawing orders.
	caps := join(capability(1, general), capability(2, bitmap), capability(3, order), capability(8, join(u16(1), u16(1), u16(1))), capability(13, input), capability(20, join(u32(0), u32(vcChunk))), capability(9, join(u16(serverChannel), u16(0))), capability(26, u32(pointerReassemblySize)), capability(27, u16(1)))
	desc := []byte("AnyKVM\x00")
	return shareControl(1, join(u32(shareID), u16(len(desc)), u16(4+len(caps)), desc, u16(9), u16(0), caps, u32(0)))
}

type clientInfo struct {
	domain, username, password string
}

func credentials(p []byte) (string, string, error) {
	info, err := parseClientInfo(p)
	return info.username, info.password, err
}

func parseClientInfo(p []byte) (clientInfo, error) {
	if len(p) < 22 {
		return clientInfo{}, fmt.Errorf("%w: truncated Client Info header", errProtocol)
	}
	// TLS selects ENCRYPTION_LEVEL_NONE. Require SEC_INFO_PKT, ignore unused
	// flagsHi, and tolerate SEC_ENCRYPT as specified by MS-RDPBCGR 3.3.5.3.11.
	if flags := le.Uint16(p); flags&0x40 == 0 {
		return clientInfo{}, fmt.Errorf("%w: Client Info security flags %#04x omit SEC_INFO_PKT", errProtocol, flags)
	}
	p = p[4:]
	if le.Uint32(p[4:])&0x10 == 0 {
		return clientInfo{}, errors.New("Unicode Client Info required")
	}
	off := 18
	var values [5]string
	for i := range values {
		n := int(le.Uint16(p[8+2*i:]))
		if n%2 != 0 || n > 4096 || off+n+2 > len(p) || le.Uint16(p[off+n:]) != 0 {
			return clientInfo{}, fmt.Errorf("%w: Client Info string %d has invalid length or terminator", errProtocol, i)
		}
		units := make([]uint16, n/2)
		for j := range units {
			units[j] = le.Uint16(p[off+j*2:])
		}
		values[i] = string(utf16.Decode(units))
		off += n + 2
	}
	return clientInfo{domain: values[0], username: values[1], password: values[2]}, nil
}

func confirmActive(p []byte) error {
	_, err := confirmActiveCapabilities(p)
	return err
}

func confirmActivePointers(p []byte) (bool, error) {
	caps, err := confirmActiveCapabilities(p)
	return caps.alphaPointer, err
}

type clientCapabilities struct {
	alphaPointer, longCredentials bool
	largePointer                  bool
	osMajor, osMinor              uint16
}

func confirmActiveCapabilities(p []byte) (clientCapabilities, error) {
	var caps clientCapabilities
	var fastPath bool
	var largeFlags uint16
	var reassembly uint32
	var generalSeen bool
	if len(p) < 20 || le.Uint16(p[2:])&15 != 3 || le.Uint32(p[6:]) != shareID {
		return caps, errProtocol
	}
	descLen, capsLen := int(le.Uint16(p[12:])), int(le.Uint16(p[14:]))
	off := 16 + descLen
	if capsLen < 4 || off+capsLen != len(p) {
		return caps, errProtocol
	}
	count := int(le.Uint16(p[off:]))
	p = p[off+4:]
	for i := 0; i < count; i++ {
		if len(p) < 4 {
			return caps, errProtocol
		}
		n := int(le.Uint16(p[2:]))
		if n < 4 || n > len(p) {
			return caps, errProtocol
		}
		switch le.Uint16(p) {
		case 1:
			if n < 24 || generalSeen {
				return caps, errProtocol
			}
			generalSeen = true
			caps.osMajor, caps.osMinor = le.Uint16(p[4:]), le.Uint16(p[6:])
			caps.longCredentials = le.Uint16(p[14:])&0x0004 != 0 // LONG_CREDENTIALS_SUPPORTED
			fastPath = le.Uint16(p[14:])&0x0001 != 0
		case 8:
			if n < 8 {
				return caps, errProtocol
			}
			// The optional pointerCacheSize enables New Pointer Updates.
			caps.alphaPointer = n >= 10 && le.Uint16(p[8:]) != 0
		case 26:
			if n < 8 {
				return caps, errProtocol
			}
			reassembly = le.Uint32(p[4:])
		case 27:
			if n < 6 {
				return caps, errProtocol
			}
			largeFlags = le.Uint16(p[4:])
		}
		p = p[n:]
	}
	if len(p) != 0 {
		return caps, fmt.Errorf("%w: capability length", errProtocol)
	}
	caps.largePointer = fastPath && largeFlags&1 != 0 && reassembly >= pointerReassemblySize
	return caps, nil
}
