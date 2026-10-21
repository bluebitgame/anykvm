//go:build darwin && cgo

package jpegcodec

/*
#cgo LDFLAGS: -framework ImageIO -framework CoreGraphics -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include "native.h"
CFDataRef bk_jpeg_encode_pixel(void *pixel, int quality);
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"fmt"
	"runtime"
	"unsafe"
)

func fallbackBackend() string { return "macOS ImageIO" }

func encodeFallbackOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	if buffer, err := encodeImageIOOwned(pixels, width, height, quality); err == nil {
		return buffer, nil
	}
	return encodeGoOwned(pixels, width, height, quality)
}

func decodeFallback(data []byte, width, height int) ([]byte, error) {
	if pixels, err := decodeImageIO(data, width, height); err == nil {
		return pixels, nil
	}
	return decodeGo(data, width, height)
}

func encodeImageIOOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	data := C.bk_jpeg_encode((*C.uint8_t)(unsafe.Pointer(&pixels[0])), C.size_t(len(pixels)), C.int(width), C.int(height), C.int(quality))
	// Native objects borrowing pixels are released before bk_jpeg_encode returns.
	runtime.KeepAlive(pixels)
	return imageIOBuffer(data)
}

func encodeImageIOPixelOwned(pixel unsafe.Pointer, quality int) (*framebuf.Buffer, error) {
	return imageIOBuffer(C.bk_jpeg_encode_pixel(pixel, C.int(quality)))
}

func imageIOBuffer(data C.CFDataRef) (*framebuf.Buffer, error) {
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

func decodeImageIO(data []byte, width, height int) ([]byte, error) {
	pixels := make([]byte, width*height*4)
	ok := C.bk_jpeg_decode((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), (*C.uint8_t)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height))
	runtime.KeepAlive(data)
	if ok == 0 {
		return nil, fmt.Errorf("ImageIO: JPEG decode failed")
	}
	return pixels, nil
}

func borrowCFData(data C.CFDataRef, size int) *framebuf.Buffer {
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(C.CFDataGetBytePtr(data))), size)
	return framebuf.New(bytes, func() { C.CFRelease(C.CFTypeRef(data)) })
}
