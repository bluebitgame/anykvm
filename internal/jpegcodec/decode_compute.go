package jpegcodec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var ErrComputeJPEG = errors.New("JPEG compute decoder unavailable for this image")

// The host parses markers and restart boundaries only. Huffman decoding,
// dequantization, IDCT, scaling and BT.709 NV12 conversion run in the shader.
// Metadata uses uint32 words and is shared by D3D11 and Vulkan storage buffers.
const decodeComponentBase = 16
const decodeQuantBase = 40
const decodeHuffmanBase = 232
const decodeHuffmanWords = 304
const decodeSegmentBase = decodeHuffmanBase + 6*decodeHuffmanWords
const decodeStorageLimit = 128 << 20

type computeJPEG struct {
	metadata                        []uint32
	entropy                         []byte
	width, height, blocks, segments int
}
type jpegDecodeComponent struct{ id, h, v, q, dc, ac int }
type jpegDecodeHuffman struct {
	words [decodeHuffmanWords]uint32
	valid bool
}

func parseComputeJPEG(data []byte, ow, oh int) (*computeJPEG, error) {
	bad := func() (*computeJPEG, error) { return nil, ErrComputeJPEG }
	if len(data) < 4 || len(data) > decodeStorageLimit || data[0] != 255 || data[1] != 0xd8 || ow < 2 || oh < 2 || ow > 8192 || oh > 8192 || (ow|oh)&1 != 0 {
		return bad()
	}
	var quant [4][64]uint32
	var haveQuant [4]bool
	var tables [2][4]jpegDecodeHuffman
	var components []jpegDecodeComponent
	w, h, mh, mv, restart := 0, 0, 1, 1, 0
	p := 2
	for p < len(data) {
		if data[p] != 255 {
			return bad()
		}
		for p < len(data) && data[p] == 255 {
			p++
		}
		if p >= len(data) {
			return bad()
		}
		marker := data[p]
		p++
		if p+2 > len(data) {
			return bad()
		}
		n := int(binary.BigEndian.Uint16(data[p:]))
		if n < 2 || n > len(data)-p {
			return bad()
		}
		b := data[p+2 : p+n]
		p += n
		switch marker {
		case 0xdb:
			for len(b) > 0 {
				if len(b) < 65 || b[0]>>4 != 0 || b[0]&15 > 3 {
					return bad()
				}
				id := int(b[0] & 15)
				for i := 0; i < 64; i++ {
					if b[i+1] == 0 {
						return bad()
					}
					quant[id][jpegZigzag[i]] = uint32(b[i+1])
				}
				haveQuant[id] = true
				b = b[65:]
			}
		case 0xc4:
			for len(b) > 0 {
				if len(b) < 17 || b[0]>>4 > 1 || b[0]&15 > 3 {
					return bad()
				}
				class, id := int(b[0]>>4), int(b[0]&15)
				t := jpegDecodeHuffman{valid: true}
				code, count := 0, 0
				for i := 0; i < 16; i++ {
					amount := int(b[1+i])
					if code+amount >= 1<<(i+1) {
						return bad()
					}
					t.words[i] = uint32(code)
					t.words[16+i] = uint32(code + amount)
					t.words[32+i] = uint32(count)
					count += amount
					code = (code + amount) << 1
				}
				if count == 0 || count > 256 || len(b) < 17+count {
					return bad()
				}
				for i, v := range b[17 : 17+count] {
					if class == 0 && v > 11 {
						return bad()
					}
					if class == 1 && v&15 > 10 {
						return bad()
					}
					t.words[48+i] = uint32(v)
				}
				tables[class][id] = t
				b = b[17+count:]
			}
		case 0xc0:
			if len(components) != 0 || len(b) < 6 || b[0] != 8 {
				return bad()
			}
			h = int(binary.BigEndian.Uint16(b[1:]))
			w = int(binary.BigEndian.Uint16(b[3:]))
			nc := int(b[5])
			if (nc != 1 && nc != 3) || len(b) != 6+3*nc || w < 1 || h < 1 || w > 8192 || h > 8192 {
				return bad()
			}
			for i := 0; i < nc; i++ {
				c := jpegDecodeComponent{id: int(b[6+i*3]), h: int(b[7+i*3] >> 4), v: int(b[7+i*3] & 15), q: int(b[8+i*3])}
				if c.h < 1 || c.h > 2 || c.v < 1 || c.v > 2 || c.q > 3 {
					return bad()
				}
				for _, old := range components {
					if old.id == c.id {
						return bad()
					}
				}
				components = append(components, c)
				mh = max(mh, c.h)
				mv = max(mv, c.v)
			}
			if nc == 3 && components[0].id == 'R' && components[1].id == 'G' && components[2].id == 'B' {
				return bad()
			}
			if nc == 1 {
				if mh != 1 || mv != 1 {
					return bad()
				}
			} else if components[0].h != mh || components[0].v != mv || components[1].h != 1 || components[1].v != 1 || components[2].h != 1 || components[2].v != 1 {
				return bad()
			}
		case 0xdd:
			if len(b) != 2 {
				return bad()
			}
			restart = int(binary.BigEndian.Uint16(b))
		case 0xee:
			// Adobe transform 0 is RGB/CMYK, not YCbCr.
			if len(b) >= 12 && string(b[:5]) == "Adobe" && b[11] != 1 {
				return bad()
			}
		case 0xda:
			nc := len(components)
			if nc == 0 || len(b) != 1+nc*2+3 || int(b[0]) != nc || b[len(b)-3] != 0 || b[len(b)-2] != 63 || b[len(b)-1] != 0 {
				return bad()
			}
			for i := range components {
				if int(b[1+i*2]) != components[i].id {
					return bad()
				}
				components[i].dc = int(b[2+i*2] >> 4)
				components[i].ac = int(b[2+i*2] & 15)
				if components[i].dc > 3 || components[i].ac > 3 {
					return bad()
				}
			}
			j := &computeJPEG{width: w, height: h, metadata: make([]uint32, decodeSegmentBase)}
			cols, rows := (w+8*mh-1)/(8*mh), (h+8*mv-1)/(8*mv)
			dw, dh := ow, oh
			if w*dh > h*dw {
				dh = max(2, (h*dw/w)&^1)
			} else {
				dw = max(2, (w*dh/h)&^1)
			}
			copy(j.metadata, []uint32{uint32(w), uint32(h), uint32(ow), uint32(oh), uint32(mh), uint32(mv), uint32(cols), uint32(rows), 0, 0, uint32(dw), uint32(dh), uint32(((ow - dw) / 2) &^ 1), uint32(((oh - dh) / 2) &^ 1), uint32(nc), 0})
			for i, c := range components {
				if !haveQuant[c.q] || !tables[0][c.dc].valid || !tables[1][c.ac].valid {
					return bad()
				}
				copy(j.metadata[decodeComponentBase+i*8:], []uint32{uint32(c.h), uint32(c.v), uint32(j.blocks)})
				j.blocks += c.h * c.v
				copy(j.metadata[decodeQuantBase+i*64:], quant[c.q][:])
				copy(j.metadata[decodeHuffmanBase+(2*i)*decodeHuffmanWords:], tables[0][c.dc].words[:])
				copy(j.metadata[decodeHuffmanBase+(2*i+1)*decodeHuffmanWords:], tables[1][c.ac].words[:])
			}
			j.metadata[8] = uint32(j.blocks)
			if cols*rows*j.blocks*64*4 > decodeStorageLimit {
				return bad()
			}
			// Unstuff entropy bytes without interpreting bits. Each restart segment has
			// independent DC predictors and a bounded MCU count for a GPU worker.
			first, start, expected := 0, 0, byte(0xd0)
			for p < len(data) {
				v := data[p]
				p++
				if v != 255 {
					j.entropy = append(j.entropy, v)
					continue
				}
				if p >= len(data) {
					return bad()
				}
				v = data[p]
				p++
				if v == 0 {
					j.entropy = append(j.entropy, 255)
					continue
				}
				for v == 255 && p < len(data) {
					v = data[p]
					p++
				}
				if v == 0xd9 || v >= 0xd0 && v <= 0xd7 {
					remaining := cols*rows - first
					count := remaining
					if v != 0xd9 {
						if restart == 0 || v != expected || remaining <= restart {
							return bad()
						}
						count = restart
						expected = 0xd0 + (expected-0xd0+1)%8
					} else if restart > 0 && remaining > restart {
						return bad()
					}
					if count <= 0 || count > 65536 || len(j.entropy) == start {
						return bad()
					}
					j.metadata = append(j.metadata, uint32(start), uint32(len(j.entropy)-start), uint32(first), uint32(count))
					j.segments++
					first += count
					start = len(j.entropy)
					if v == 0xd9 {
						j.metadata[9] = uint32(j.segments)
						j.entropy = append(j.entropy, make([]byte, (-len(j.entropy))&3)...)
						return j, nil
					}
				} else {
					return bad()
				}
			}
			return bad()
		default:
			if marker < 0xe0 || marker > 0xef {
				if marker != 0xfe {
					return bad()
				}
			}
		}
	}
	return bad()
}

// NV12Decoder owns a reusable GPU compute context. Calls must be serialized. Decode returns tightly packed
// limited-range BT.709 NV12 at the requested even dimensions. It never silently
// decodes on the CPU; callers can explicitly use DecodeBGRA as a fallback.
type NV12Decoder struct{ backend computeJPEGBackend }
type computeJPEGBackend interface {
	decode(*computeJPEG) ([]byte, error)
	Close()
}

func NewNV12Decoder() (*NV12Decoder, error) {
	b, err := newComputeJPEGBackend()
	if err != nil {
		return nil, err
	}
	return &NV12Decoder{backend: b}, nil
}
func (d *NV12Decoder) Decode(data []byte, width, height int) ([]byte, error) {
	j, err := parseComputeJPEG(data, width, height)
	if err != nil {
		return nil, err
	}
	if d == nil || d.backend == nil {
		return nil, fmt.Errorf("%w: decoder closed", ErrComputeJPEG)
	}
	return d.backend.decode(j)
}
func (d *NV12Decoder) Close() {
	if d != nil && d.backend != nil {
		d.backend.Close()
		d.backend = nil
	}
}
