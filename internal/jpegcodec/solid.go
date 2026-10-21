package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
)

// SolidJPEGColor proves a baseline YCbCr/grayscale JPEG is constant using only
// Huffman symbols and DC values. It allocates no coefficient or pixel image and
// performs no IDCT. Unsupported color metadata/scans conservatively return false.
// The result is RGB packed into the low 24 bits, after JPEG quantization.
func SolidJPEGColor(data []byte) (uint32, bool) {
	j := solidJPEGMetadata(data)
	if j == nil {
		return 0, false
	}
	var expected [3]int
	first := true
	for segment := 0; segment < j.segments; segment++ {
		base := decodeSegmentBase + segment*4
		start, size, count := int(j.metadata[base]), int(j.metadata[base+1]), int(j.metadata[base+3])
		reader := solidBits{data: j.entropy[start : start+size]}
		var predictors [3]int
		for i := 0; i < count; i++ {
			if !solidMCU(j.metadata, &reader, &predictors, &expected, first) {
				return 0, false
			}
			first = false
		}
		if !reader.padding() {
			return 0, false
		}
	}
	return solidRGB(j.metadata, expected), !first
}

// RestartSolidChecker reuses header tables across independent-MCU regions.
// It borrows an immutable RestartJPEG, including GPU snapshots which have no
// packed full-frame JPEG. Calls read only the selected entropy segments.
type RestartSolidChecker struct {
	source *RestartJPEG
	meta   []uint32
}

func (j *RestartJPEG) NewSolidChecker() *RestartSolidChecker {
	// Parse one tiny MCU to reuse the existing, validated table parser without
	// packing or copying the full compressed desktop.
	sample, err := j.Crop(image.Rect(0, 0, min(16, j.Width), min(16, j.Height)))
	if err != nil {
		return nil
	}
	parsed := solidJPEGMetadata(sample)
	if parsed == nil {
		return nil
	}
	return &RestartSolidChecker{j, parsed.metadata}
}

func (c *RestartSolidChecker) Color(r image.Rectangle) (uint32, bool) {
	if c == nil || !c.source.validRect(r) {
		return 0, false
	}
	var expected [3]int
	first := true
	cols := (c.source.Width + 15) / 16
	for y := r.Min.Y / 16; y < (r.Max.Y+15)/16; y++ {
		for x := r.Min.X / 16; x < (r.Max.X+15)/16; x++ {
			reader := solidBits{data: c.source.segments[y*cols+x], stuffed: true}
			var predictors [3]int // Every MCU starts a new restart interval.
			if !solidMCU(c.meta, &reader, &predictors, &expected, first) || !reader.padding() {
				return 0, false
			}
			first = false
		}
	}
	return solidRGB(c.meta, expected), !first
}

func solidJPEGMetadata(data []byte) *computeJPEG {
	// Avoid Fill for RGB JPEG, ICC/color profiles, EXIF transforms, or unknown
	// component conventions. Forwarding the original JPEG preserves semantics.
	if len(data) < 4 || data[0] != 255 || data[1] != 0xd8 {
		return nil
	}
	for p := 2; ; {
		if p+4 > len(data) || data[p] != 255 {
			return nil
		}
		marker := data[p+1]
		n := int(binary.BigEndian.Uint16(data[p+2:]))
		if n < 2 || n > len(data)-p-2 {
			return nil
		}
		b := data[p+4 : p+2+n]
		if marker == 0xc0 {
			if len(b) < 6 || (b[5] != 1 && b[5] != 3) || len(b) != 6+3*int(b[5]) {
				return nil
			}
			for i := 0; i < int(b[5]); i++ {
				if b[6+3*i] != byte(i+1) {
					return nil
				}
			}
		}
		if marker >= 0xe0 && marker <= 0xef {
			if marker == 0xe0 && bytes.HasPrefix(b, []byte("JFIF\x00")) {
				// Standard YCbCr/grayscale interpretation.
			} else if marker == 0xee && len(b) == 12 && string(b[:5]) == "Adobe" && b[11] == 1 {
				// Explicit YCbCr, also understood by parseComputeJPEG.
			} else {
				return nil
			}
		}
		if marker == 0xda {
			parsed, err := parseComputeJPEG(data, 2, 2)
			if err != nil {
				return nil
			}
			return parsed
		}
		p += n + 2
	}
}

// A bounded bit reader, optionally unstuffing MCU bytes in place. The generic
// baseline parser already unstuffs its segments; GPU snapshots remain borrowed.
type solidBits struct {
	data    []byte
	pos     int
	value   byte
	left    uint
	stuffed bool
	bad     bool
}

func (r *solidBits) read(n uint) int {
	value := 0
	for i := uint(0); i < n; i++ {
		if r.left == 0 {
			if r.pos >= len(r.data) {
				r.bad = true
				return 0
			}
			r.value = r.data[r.pos]
			r.pos++
			if r.stuffed && r.value == 255 {
				if r.pos >= len(r.data) || r.data[r.pos] != 0 {
					r.bad = true
					return 0
				}
				r.pos++
			}
			r.left = 8
		}
		r.left--
		value = value<<1 | int((r.value>>r.left)&1)
	}
	return value
}

func (r *solidBits) symbol(table []uint32) int {
	code := 0
	for length := 0; length < 16 && !r.bad; length++ {
		code = code<<1 | r.read(1)
		first, limit := int(table[length]), int(table[16+length])
		if code >= first && code < limit {
			return int(table[48+int(table[32+length])+code-first])
		}
	}
	r.bad = true
	return 0
}

func (r *solidBits) padding() bool {
	bits := r.left
	return !r.bad && r.pos == len(r.data) && r.read(bits) == (1<<bits)-1
}

func solidMCU(meta []uint32, reader *solidBits, predictors, expected *[3]int, first bool) bool {
	for component := 0; component < int(meta[14]); component++ {
		base := decodeComponentBase + component*8
		blocks := int(meta[base] * meta[base+1])
		dc := meta[decodeHuffmanBase+component*2*decodeHuffmanWords:]
		ac := dc[decodeHuffmanWords:]
		for block := 0; block < blocks; block++ {
			size := reader.symbol(dc)
			if reader.bad || size > 11 {
				return false
			}
			diff := reader.read(uint(size))
			if size > 0 && diff < 1<<(size-1) {
				diff -= (1 << size) - 1
			}
			predictors[component] += diff
			value := predictors[component]
			if reader.bad || value < -2048 || value > 2047 {
				return false
			}
			// Reject extreme dequantized values outside the safe 8-bit IDCT
			// range. Malformed JPEGs can otherwise wrap a decoder's range-limit
			// lookup instead of producing the ordinary clamped sample below.
			dequantized := value * int(meta[decodeQuantBase+component*64])
			if dequantized < -2048 || dequantized > 2047 {
				return false
			}
			if first && block == 0 {
				expected[component] = value
			} else if value != expected[component] {
				return false
			}
			for k := 1; k < 64; {
				symbol := reader.symbol(ac)
				if reader.bad {
					return false
				}
				if symbol == 0 { // EOB: all remaining AC coefficients are zero.
					break
				}
				if symbol != 0xf0 || k+16 > 64 {
					return false // A nonzero AC or invalid zero run.
				}
				k += 16
			}
		}
	}
	return !reader.bad
}

func solidRGB(meta []uint32, dc [3]int) uint32 {
	var samples [3]uint8
	for c := 0; c < int(meta[14]); c++ {
		q := int(meta[decodeQuantBase+c*64])
		samples[c] = uint8(min(255, max(0, 128+((dc[c]*q+4)>>3))))
	}
	if meta[14] == 1 {
		return uint32(samples[0]) * 0x010101
	}
	r, g, b := color.YCbCrToRGB(samples[0], samples[1], samples[2])
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
}
