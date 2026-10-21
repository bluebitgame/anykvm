//go:build darwin && cgo && !amd64 && !anykvm_jpeg_simd

package jpegcodec

/*
#cgo LDFLAGS: -framework ImageIO -framework CoreGraphics -framework CoreFoundation -framework CoreVideo -framework CoreImage
#include <CoreFoundation/CoreFoundation.h>
CFDataRef bk_jpeg_encode_pixel(void *pixel, int quality);
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"fmt"
	"unsafe"
)

func EncodePixelBufferOwned(pixel unsafe.Pointer, quality int) (*framebuf.Buffer, error) {
	if pixel == nil {
		return nil, fmt.Errorf("ImageIO: nil pixel buffer")
	}
	if quality <= 0 {
		quality = 80
	}
	data := C.bk_jpeg_encode_pixel(pixel, C.int(min(100, quality)))
	if data == 0 {
		return nil, fmt.Errorf("ImageIO: pixel buffer JPEG encode failed")
	}
	size := C.CFDataGetLength(data)
	if size < 1 || size > 1<<31-1 {
		C.CFRelease(C.CFTypeRef(data))
		return nil, fmt.Errorf("ImageIO: invalid JPEG output size")
	}
	return borrowCFData(data, int(size)), nil
}

func borrowCFData(data C.CFDataRef, size int) *framebuf.Buffer {
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(C.CFDataGetBytePtr(data))), size)
	return framebuf.New(bytes, func() { C.CFRelease(C.CFTypeRef(data)) })
}
