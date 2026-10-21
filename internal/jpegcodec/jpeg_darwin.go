//go:build darwin && cgo && !amd64 && !anykvm_jpeg_simd

package jpegcodec

/*
#cgo LDFLAGS: -framework ImageIO -framework CoreGraphics -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include "native.h"
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"fmt"
	"runtime"
	"unsafe"
)

func Backend() string { return "macOS ImageIO" }

func encodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	b, err := encodeBGRAOwned(pixels, width, height, quality)
	if err != nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}
func encodeBGRAOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	data := C.bk_jpeg_encode((*C.uint8_t)(unsafe.Pointer(&pixels[0])), C.size_t(len(pixels)), C.int(width), C.int(height), C.int(quality))
	// Native objects borrowing pixels are released before bk_jpeg_encode returns.
	runtime.KeepAlive(pixels)
	if data == 0 {
		return nil, fmt.Errorf("ImageIO: JPEG encode failed")
	}
	size := C.CFDataGetLength(data)
	if size < 1 || size > 1<<31-1 {
		C.CFRelease(C.CFTypeRef(data))
		return nil, fmt.Errorf("ImageIO: invalid JPEG output size")
	}
	return borrowCFData(data, int(size)), nil
}

func decodeBGRA(data []byte, width, height int) ([]byte, error) {
	pixels := make([]byte, width*height*4)
	ok := C.bk_jpeg_decode((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), (*C.uint8_t)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height))
	runtime.KeepAlive(data)
	if ok == 0 {
		return nil, fmt.Errorf("ImageIO: JPEG decode failed")
	}
	return pixels, nil
}
