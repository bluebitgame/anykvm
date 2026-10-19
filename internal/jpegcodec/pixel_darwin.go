//go:build darwin && cgo

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

// EncodePixelBuffer synchronously borrows a retained CVPixelBuffer. Only the
// compressed result crosses into Go; the caller retains ownership of pixel.
func EncodePixelBuffer(pixel unsafe.Pointer, quality int) ([]byte, error) {
	b, err := EncodePixelBufferOwned(pixel, quality)
	if err != nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}

// EncodePixelBufferOwned returns a read-only native JPEG lease. The caller must
// release it after its final consumer, including on cancellation or replacement.
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
