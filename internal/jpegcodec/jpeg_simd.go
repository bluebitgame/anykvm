//go:build cgo && (amd64 || 386 || arm64) && (linux || windows || (darwin && amd64) || anykvm_jpeg_simd)

package jpegcodec

/*
#cgo !windows LDFLAGS: -lm
#include "simd.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/cpu"
)

const (
	jpegSSE2 = int(C.BK_JPEG_SSE2)
	jpegAVX2 = int(C.BK_JPEG_AVX2)
	jpegNEON = int(C.BK_JPEG_NEON)
)

// x/sys checks OS support for saving YMM registers as well as CPU AVX2 support.
var jpegSIMDLevel = func() int {
	if runtime.GOARCH == "arm64" && cpu.ARM64.HasASIMD {
		return jpegNEON
	}
	if cpu.X86.HasAVX2 {
		return jpegAVX2
	}
	if cpu.X86.HasSSE2 {
		return jpegSSE2
	}
	return 0
}()

var cpuJPEGParameters = sync.Pool{New: func() any { return new(jpegFrameParameters) }}

type simdEncoder struct{ native *C.bk_jpeg_encoder }

// Keep one idle allocation across GCs. Frames still held by consumers retain
// their own encoder, so a later encode cannot overwrite an outstanding frame.
var idleSIMDEncoder = make(chan *simdEncoder, 1)

func acquireSIMDEncoder() *simdEncoder {
	select {
	case encoder := <-idleSIMDEncoder:
		return encoder
	default:
		encoder := &simdEncoder{native: C.bk_jpeg_encoder_new()}
		runtime.SetFinalizer(encoder, (*simdEncoder).close)
		return encoder
	}
}

func (e *simdEncoder) close() {
	C.bk_jpeg_encoder_free(e.native)
	e.native = nil
}

func (e *simdEncoder) release() {
	select {
	case idleSIMDEncoder <- e:
	default:
		runtime.SetFinalizer(e, nil)
		e.close()
	}
}

func Backend() string {
	switch jpegSIMDLevel {
	case jpegNEON:
		return "anyjpeg NEON"
	case jpegAVX2:
		return "anyjpeg AVX2"
	case jpegSSE2:
		return "anyjpeg SSE2"
	default:
		return "Go JPEG"
	}
}

func encodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	buffer, err := encodeBGRAOwned(pixels, width, height, quality)
	if err != nil {
		return nil, err
	}
	defer buffer.Release()
	return append([]byte(nil), buffer.Bytes()...), nil
}

func encodeBGRAOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	return encodeSIMDOwned(pixels, width, height, quality, jpegSIMDLevel)
}

func encodeSIMDOwned(pixels []byte, width, height, quality, level int) (*framebuf.Buffer, error) {
	parameters := cpuJPEGParameters.Get().(*jpegFrameParameters)
	defer cpuJPEGParameters.Put(parameters)
	if level == 0 || parameters.prepare(width, height, quality) != nil {
		data, err := encodeGo(pixels, width, height, quality)
		if err != nil {
			return nil, err
		}
		return framebuf.New(data, nil), nil
	}
	encoder := acquireSIMDEncoder()
	if encoder.native == nil {
		return nil, fmt.Errorf("SIMD JPEG encoder allocation failed")
	}
	var data *C.uint8_t
	var size C.size_t
	header := parameters.header
	ok := C.bk_jpeg_encoder_encode(encoder.native, (*C.uint8_t)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height), C.int(level),
		(*C.uint32_t)(unsafe.Pointer(&parameters.quantization[0])), (*C.uint32_t)(unsafe.Pointer(&jpegPackedEntropyCodes[0])),
		(*C.uint8_t)(unsafe.Pointer(&header[0])), C.size_t(len(header)), &data, &size)
	runtime.KeepAlive(pixels)
	runtime.KeepAlive(parameters)
	if ok == 0 {
		encoder.release()
		return nil, fmt.Errorf("SIMD JPEG encoding failed")
	}
	return framebuf.New(unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size)), encoder.release), nil
}

func decodeBGRA(data []byte, width, height int) ([]byte, error) {
	return decodeSIMD(data, width, height, jpegSIMDLevel)
}

func decodeSIMD(data []byte, width, height, level int) ([]byte, error) {
	if level == 0 {
		return decodeGo(data, width, height)
	}
	// The shared parser describes baseline scans; progressive, CMYK and unusual
	// sampling remain supported through Go's JPEG decoder.
	frame, err := parseComputeJPEG(data, max(2, (width+1)&^1), max(2, (height+1)&^1))
	if err != nil {
		return decodeGo(data, width, height)
	}
	if frame.width != width || frame.height != height {
		return nil, fmt.Errorf("JPEG dimensions changed")
	}
	pixels := make([]byte, width*height*4)
	ok := C.bk_jpeg_simd_decode((*C.uint32_t)(unsafe.Pointer(&frame.metadata[0])),
		(*C.uint8_t)(unsafe.Pointer(&frame.entropy[0])), C.int(level), (*C.uint8_t)(unsafe.Pointer(&pixels[0])))
	runtime.KeepAlive(frame)
	if ok == 0 {
		return nil, fmt.Errorf("invalid JPEG entropy")
	}
	return pixels, nil
}
