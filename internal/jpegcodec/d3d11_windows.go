//go:build windows && cgo

package jpegcodec

/*
#cgo LDFLAGS: -ld3d11 -ldxgi -ldxguid
#include "d3d11_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// D3D11Encoder synchronously produces a complete JPEG on the capture GPU.
// Calls, including Close, must be serialized by the capture owner.
type D3D11Encoder struct {
	ptr        *C.bk_jpeg_gpu
	parameters jpegFrameParameters
}

func NewD3D11Encoder(adapterLUID uint64) (*D3D11Encoder, error) {
	var message [1024]C.char
	p := C.bk_jpeg_gpu_open(C.uint64_t(adapterLUID), (*C.char)(unsafe.Pointer(unsafe.StringData(jpegComputeShader))),
		C.size_t(len(jpegComputeShader)), &message[0], C.size_t(len(message)))
	if p == nil {
		return nil, fmt.Errorf("%s", C.GoString(&message[0]))
	}
	return &D3D11Encoder{ptr: p}, nil
}

// EncodeTexture borrows a keyed-mutex shared BGRA/RGBA texture through completion.
// It scales/rotates directly on the GPU, then reads only the final compressed JPEG.
func (e *D3D11Encoder) EncodeTexture(handle uintptr, rotation, width, height, quality int) ([]byte, error) {
	if e == nil || e.ptr == nil {
		return nil, fmt.Errorf("D3D11 JPEG encoder is closed")
	}
	if handle == 0 || width < 1 || height < 1 || width > 8192 || height > 8192 ||
		(rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270) {
		return nil, fmt.Errorf("D3D11 JPEG: invalid texture geometry")
	}
	if err := e.parameters.prepare(width, height, quality); err != nil {
		return nil, err
	}
	header := e.parameters.header
	var jpeg *C.uint8_t
	var jpegBytes C.size_t
	var message [1024]C.char
	ok := C.bk_jpeg_gpu_encode(e.ptr, C.uintptr_t(handle), C.int(rotation), C.int(width), C.int(height),
		(*C.uint32_t)(unsafe.Pointer(&e.parameters.quantization[0])), (*C.uint32_t)(unsafe.Pointer(&jpegPackedEntropyCodes[0])),
		(*C.uint8_t)(unsafe.Pointer(&header[0])), C.size_t(len(header)), &jpeg, &jpegBytes, &message[0], C.size_t(len(message)))
	defer C.free(unsafe.Pointer(jpeg))
	runtime.KeepAlive(e)
	if ok == 0 {
		return nil, fmt.Errorf("%s", C.GoString(&message[0]))
	}
	if jpeg == nil || jpegBytes < 2 || jpegBytes > 128<<20 {
		return nil, fmt.Errorf("invalid GPU JPEG output size")
	}
	return C.GoBytes(unsafe.Pointer(jpeg), C.int(jpegBytes)), nil
}

func (e *D3D11Encoder) Close() {
	if e != nil && e.ptr != nil {
		C.bk_jpeg_gpu_close(e.ptr)
		e.ptr = nil
	}
}
