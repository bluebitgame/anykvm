package jpegcodec

import (
	"fmt"
	"math/bits"
)

// quantization420 is shared by the GPU and JPEG headers, in natural order.
func quantization420(quality int) [128]uint32 {
	quality = min(100, max(1, quality))
	scale := 200 - quality*2
	if quality < 50 {
		scale = 5000 / quality
	}
	var q [128]uint32
	for table := range jpegBaseQuant {
		for i, base := range jpegBaseQuant[table] {
			q[table*64+jpegZigzag[i]] = uint32(min(255, max(1, (int(base)*scale+50)/100)))
		}
	}
	return q
}

type jpegCode struct{ value, size uint32 }

var jpegCodes = func() (lut [4][256]jpegCode) {
	for table, spec := range jpegHuffmanSpecs {
		code, index := uint32(0), 0
		for length, count := range spec.count {
			for range count {
				lut[table][spec.value[index]] = jpegCode{code, uint32(length + 1)}
				code++
				index++
			}
			code <<= 1
		}
	}
	return
}()

// JPEG markers are static frame metadata; entropy is generated on the GPU.
func jpegHeader420(width, height, quality int, restart bool) []byte {
	data := make([]byte, 0, 640)
	data = append(data, 0xff, 0xd8) // SOI.
	segment := func(marker byte, payload []byte) {
		n := len(payload) + 2
		data = append(data, 0xff, marker, byte(n>>8), byte(n))
		data = append(data, payload...)
	}
	q := quantization420(quality)
	dqt := make([]byte, 0, 130)
	for table := range 2 {
		dqt = append(dqt, byte(table))
		for _, natural := range jpegZigzag {
			dqt = append(dqt, byte(q[table*64+natural]))
		}
	}
	segment(0xdb, dqt)
	segment(0xc0, []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), 3,
		1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1})
	var dht []byte
	for i, spec := range jpegHuffmanSpecs {
		dht = append(dht, byte((i%2)<<4|i/2))
		dht = append(dht, spec.count[:]...)
		dht = append(dht, spec.value...)
	}
	segment(0xc4, dht)
	if restart {
		segment(0xdd, []byte{0, 1})
	}
	segment(0xda, []byte{3, 1, 0, 2, 0x11, 3, 0x11, 0, 63, 0})
	return data
}

// Codes use low 16 bits for the canonical code and high 16 bits for its length.
var jpegPackedHuffmanCodes = func() (tables [1024]uint32) {
	for table, codes := range jpegCodes {
		for symbol, code := range codes {
			tables[table*256+symbol] = code.size<<16 | code.value
		}
	}
	return
}()

// Standard codes followed by combined AC code/amplitude entries for -31..31.
// Each small-AC entry packs bit count:5 | output bits:27. The signed value
// occupies the low six index bits; zero and -32 are unused.
var jpegPackedEntropyCodes = func() (tables [3072]uint32) {
	copy(tables[:], jpegPackedHuffmanCodes[:])
	for table := 0; table < 2; table++ {
		for run := 0; run < 16; run++ {
			for value := -31; value <= 31; value++ {
				if value == 0 {
					continue
				}
				magnitude := value
				if magnitude < 0 {
					magnitude = -magnitude
				}
				size := uint32(bits.Len32(uint32(magnitude)))
				code := jpegCodes[table*2+1][run*16+int(size)]
				amplitude := value
				if amplitude < 0 {
					amplitude--
				}
				encoded := code.value<<size | uint32(amplitude)&((1<<size)-1)
				tables[1024+table*1024+run*64+(value&63)] = (code.size+size)<<27 | encoded
			}
		}
	}
	return
}()

// Each encoder retains only its current frame configuration.
type jpegFrameParameters struct {
	width, height, quality int
	quantization           [128]uint32
	header                 []byte
}

func (p *jpegFrameParameters) prepare(width, height, quality int) error {
	if width < 1 || height < 1 || width > 8192 || height > 8192 ||
		((width+15)/16)*((height+15)/16) > (128<<20)/3072 {
		return fmt.Errorf("GPU JPEG: frame exceeds bounded segment storage")
	}
	quality = min(100, max(1, quality))
	if p.width == width && p.height == height && p.quality == quality {
		return nil
	}
	p.quantization = quantization420(quality)
	p.header = jpegHeader420(width, height, quality, true)
	p.width, p.height, p.quality = width, height, quality
	return nil
}
