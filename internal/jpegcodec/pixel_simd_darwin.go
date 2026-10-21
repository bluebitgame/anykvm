//go:build darwin && cgo && (amd64 || anykvm_jpeg_simd)

package jpegcodec

/*
#cgo LDFLAGS: -framework CoreVideo -framework CoreImage -framework CoreGraphics -framework Foundation
#include "pixel_simd_darwin.h"
static void *lock_bgra(void *pixel) { return bk_jpeg_pixel_lock_bgra(pixel); }
static void unlock_bgra(void *pixel) { bk_jpeg_pixel_unlock_bgra(pixel); }
static size_t pixel_width(void *pixel) { return CVPixelBufferGetWidth(pixel); }
static size_t pixel_height(void *pixel) { return CVPixelBufferGetHeight(pixel); }
static size_t pixel_stride(void *pixel) { return CVPixelBufferGetBytesPerRow(pixel); }
static void *pixel_base(void *pixel) { return CVPixelBufferGetBaseAddress(pixel); }
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"fmt"
	"sync"
	"unsafe"
)

var pixelBGRAScratch = sync.Pool{New: func() any { return new([]byte) }}

// EncodePixelBufferOwned borrows the native pixels until encoding completes.
// The returned JPEG has independent storage and must be released by the caller.
func EncodePixelBufferOwned(pixel unsafe.Pointer, quality int) (*framebuf.Buffer, error) {
	if pixel == nil {
		return nil, fmt.Errorf("anyjpeg: nil pixel buffer")
	}
	bgra := C.lock_bgra(pixel)
	if bgra == nil {
		return nil, fmt.Errorf("anyjpeg: cannot access BGRA pixel buffer")
	}
	defer C.unlock_bgra(bgra)
	width, height := int(C.pixel_width(bgra)), int(C.pixel_height(bgra))
	stride := int(C.pixel_stride(bgra))
	pixels := unsafe.Slice((*byte)(C.pixel_base(bgra)), stride*(height-1)+width*4)
	if quality <= 0 {
		quality = 80
	}
	return encodePixelRows(pixels, width, height, stride, quality)
}

func encodePixelRows(pixels []byte, width, height, stride, quality int) (*framebuf.Buffer, error) {
	rowBytes := width * 4
	if stride == rowBytes {
		return EncodeBGRAOwned(pixels, width, height, quality)
	}
	// CVPixelBuffer rows may have alignment padding. Reuse the packing buffer;
	// tightly packed BGRA above goes straight to anyjpeg without a pixel copy.
	scratch := pixelBGRAScratch.Get().(*[]byte)
	defer pixelBGRAScratch.Put(scratch)
	if cap(*scratch) < rowBytes*height {
		*scratch = make([]byte, rowBytes*height)
	}
	packed := (*scratch)[:rowBytes*height]
	for y := 0; y < height; y++ {
		copy(packed[y*rowBytes:(y+1)*rowBytes], pixels[y*stride:y*stride+rowBytes])
	}
	return EncodeBGRAOwned(packed, width, height, quality)
}
