//go:build windows && cgo

package jpegcodec

/*
#include "d3d11_windows.h"
*/
import "C"
import (
	_ "embed"
	"fmt"
	"runtime"
	"unsafe"
)

//go:embed shaders/jpeg_decode.hlsl
var jpegDecodeShader string

type d3dJPEGDecoder struct{ ptr *C.bk_jpeg_decoder }

func newComputeJPEGBackend() (computeJPEGBackend, error) {
	var message [1024]C.char
	ptr := C.bk_jpeg_decoder_open((*C.char)(unsafe.Pointer(unsafe.StringData(jpegDecodeShader))), C.size_t(len(jpegDecodeShader)), &message[0], C.size_t(len(message)))
	if ptr == nil {
		return nil, fmt.Errorf("%w: %s", ErrComputeJPEG, C.GoString(&message[0]))
	}
	return &d3dJPEGDecoder{ptr: ptr}, nil
}
func (d *d3dJPEGDecoder) decode(j *computeJPEG) ([]byte, error) {
	out := make([]byte, int(j.metadata[2])*int(j.metadata[3])*3/2)
	var message [1024]C.char
	status := C.bk_jpeg_decoder_decode(d.ptr, (*C.uint8_t)(unsafe.Pointer(&j.entropy[0])), C.size_t(len(j.entropy)), (*C.uint32_t)(unsafe.Pointer(&j.metadata[0])), C.size_t(len(j.metadata)*4), (*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)), &message[0], C.size_t(len(message)))
	runtime.KeepAlive(j)
	if status != 0 {
		return nil, fmt.Errorf("JPEG compute: %s", C.GoString(&message[0]))
	}
	return out, nil
}
func (d *d3dJPEGDecoder) Close() { C.bk_jpeg_decoder_close(d.ptr); d.ptr = nil }
