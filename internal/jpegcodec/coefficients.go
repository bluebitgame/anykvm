package jpegcodec

import "fmt"

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
func gpuHuffmanTables() (tables [1024]uint32) {
	for table, codes := range jpegCodes {
		for symbol, code := range codes {
			tables[table*256+symbol] = code.size<<16 | code.value
		}
	}
	return
}

func gpuJPEGFrame(width, height, quality int) ([128]uint32, [1024]uint32, []byte, error) {
	if width < 1 || height < 1 || width > 8192 || height > 8192 ||
		((width+15)/16)*((height+15)/16) > (128<<20)/3072 {
		return [128]uint32{}, [1024]uint32{}, nil, fmt.Errorf("GPU JPEG: frame exceeds bounded segment storage")
	}
	return quantization420(quality), gpuHuffmanTables(), jpegHeader420(width, height, quality, true), nil
}
