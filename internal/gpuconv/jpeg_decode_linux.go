//go:build linux && cgo

package gpuconv

/*
#include "native_linux.h"
*/
import "C"
import (
	"fmt"
	"runtime"
	"unsafe"
)

type JPEGDecoder struct {
	context *C.bk_vk
	ptr     *C.bk_vk_jpeg_decoder
}

func NewJPEGDecoder(stages [3][]byte) (*JPEGDecoder, error) {
	for _, s := range stages {
		if len(s) == 0 || len(s)%4 != 0 {
			return nil, fmt.Errorf("invalid JPEG decode SPIR-V")
		}
	}
	var message [1024]C.char
	context := C.bk_vk_open(0, 0, unsafe.Pointer(&stages[0][0]), C.size_t(len(stages[0])), C.BK_VK_JPEG_DECODE, &message[0], C.size_t(len(message)))
	if context == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&message[0]))
	}
	ptr := C.bk_vk_jpeg_decoder_new(context, unsafe.Pointer(&stages[1][0]), C.size_t(len(stages[1])), unsafe.Pointer(&stages[2][0]), C.size_t(len(stages[2])), &message[0], C.size_t(len(message)))
	if ptr == nil {
		C.bk_vk_close(context)
		return nil, fmt.Errorf("JPEG decode: %s", C.GoString(&message[0]))
	}
	return &JPEGDecoder{context: context, ptr: ptr}, nil
}

// Decode is serialized by jpegcodec.NV12Decoder. It transfers only compressed
// input/metadata to the GPU and reads back NV12 for byte-oriented encoders.
func (d *JPEGDecoder) Decode(input []byte, metadata []uint32) ([]byte, error) {
	if d == nil || d.ptr == nil || len(input) < 4 || len(input)%4 != 0 || len(metadata) < 2056 || metadata[2] < 2 || metadata[3] < 2 || metadata[2] > 8192 || metadata[3] > 8192 {
		return nil, fmt.Errorf("invalid JPEG decode buffers")
	}
	out := make([]byte, int(metadata[2])*int(metadata[3])*3/2)
	var message [1024]C.char
	status := C.bk_vk_jpeg_decoder_decode(d.ptr, (*C.uint8_t)(unsafe.Pointer(&input[0])), C.size_t(len(input)), (*C.uint32_t)(unsafe.Pointer(&metadata[0])), C.size_t(len(metadata)*4), (*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &message[0], C.size_t(len(message)))
	runtime.KeepAlive(input)
	runtime.KeepAlive(metadata)
	if status != 0 {
		return nil, fmt.Errorf("JPEG decode: %s", C.GoString(&message[0]))
	}
	return out, nil
}
func (d *JPEGDecoder) Close() {
	if d != nil && d.ptr != nil {
		C.bk_vk_jpeg_decoder_free(d.ptr)
		C.bk_vk_close(d.context)
		d.ptr = nil
		d.context = nil
	}
}
