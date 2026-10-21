//go:build linux && cgo

package gpuconv

/*
#include "native_linux.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/dmabuf"
	"fmt"
	"sync"
	"unsafe"
)

// JPEGEncoder owns a compute-only device associated with the captured DRM
// source, including the split vc4 display/v3d renderer on Raspberry Pi 4/5.
// Coefficients remain device-local. Readback is either a standalone JPEG or
// compact records for changed MCUs, selected by the consumer.
type JPEGEncoder struct {
	mu           sync.Mutex
	ptr          *C.bk_vk
	major, minor uint32 // Capture source identity; the renderer may be a different DRM device.
}

// DRMDevice identifies the selected renderer, which can differ from the source.
func (e *JPEGEncoder) DRMDevice() (uint32, uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return 0, 0
	}
	var major, minor C.uint32_t
	C.bk_vk_drm_device(e.ptr, &major, &minor)
	return uint32(major), uint32(minor)
}

// Stages: transform, entropy, local scan, pack, compare, work preparation,
// group scan, offset addition, entropy selection and entropy preparation.
func NewJPEGEncoder(major, minor uint32, stages [10][]byte) (*JPEGEncoder, error) {
	if major == 0 {
		return nil, fmt.Errorf("invalid Vulkan JPEG DRM device")
	}
	for _, stage := range stages {
		if len(stage) == 0 || len(stage)%4 != 0 {
			return nil, fmt.Errorf("invalid Vulkan JPEG shader")
		}
	}
	var message [512]C.char
	pointer := func(stage int) unsafe.Pointer { return unsafe.Pointer(&stages[stage][0]) }
	ptr := C.bk_vk_open(C.uint32_t(major), C.uint32_t(minor), pointer(0), C.size_t(len(stages[0])), C.BK_VK_JPEG, &message[0], 512)
	if ptr == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&message[0]))
	}
	if C.bk_vk_jpeg_init(ptr, pointer(1), C.size_t(len(stages[1])), pointer(2), C.size_t(len(stages[2])), pointer(3), C.size_t(len(stages[3])), pointer(4), C.size_t(len(stages[4])), pointer(5), C.size_t(len(stages[5])),
		pointer(8), C.size_t(len(stages[8])), pointer(9), C.size_t(len(stages[9])),
		pointer(6), C.size_t(len(stages[6])), pointer(7), C.size_t(len(stages[7])), &message[0], 512) < 0 {
		C.bk_vk_close(ptr)
		return nil, fmt.Errorf("Vulkan JPEG pipelines: %s", C.GoString(&message[0]))
	}
	return &JPEGEncoder{ptr: ptr, major: major, minor: minor}, nil
}

func (e *JPEGEncoder) EncodeSource(frame dmabuf.Frame, width, height int, quantization *[128]uint32, huffman *[3072]uint32, header []byte) ([]byte, error) {
	return e.encodeSource(frame, width, height, quantization, huffman, header, false)
}

func (e *JPEGEncoder) EncodeRegions(frame dmabuf.Frame, width, height int, quantization *[128]uint32, huffman *[3072]uint32, header []byte) ([]byte, error) {
	return e.encodeSource(frame, width, height, quantization, huffman, header, true)
}

func (e *JPEGEncoder) encodeSource(frame dmabuf.Frame, width, height int, quantization *[128]uint32, huffman *[3072]uint32, header []byte, regions bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("Vulkan JPEG encoder closed")
	}
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	if frame.DeviceMajor != e.major || frame.DeviceMinor != e.minor {
		return nil, fmt.Errorf("Vulkan JPEG source does not match configured capture DRM device")
	}
	if quantization == nil || huffman == nil || width < 1 || height < 1 || width > 8192 || height > 8192 || len(header) < 2 || len(header) > 1024 ||
		((width+15)/16)*((height+15)/16) > (128<<20)/3072 {
		return nil, fmt.Errorf("invalid Vulkan JPEG frame or segment storage")
	}
	input := nativeSource(frame)
	var message [512]C.char
	var jpeg *C.uint8_t
	var bytes C.size_t
	mode := C.int(0)
	if regions {
		mode = 1
	}
	status := C.bk_vk_jpeg_encode_mode(e.ptr, &input, C.int(width), C.int(height),
		(*C.uint32_t)(unsafe.Pointer(&quantization[0])), (*C.uint32_t)(unsafe.Pointer(&huffman[0])),
		(*C.uint8_t)(unsafe.Pointer(&header[0])), C.size_t(len(header)), mode, &jpeg, &bytes, &message[0], 512)
	defer C.free(unsafe.Pointer(jpeg))
	if status < 0 {
		return nil, fmt.Errorf("Vulkan JPEG: %s", C.GoString(&message[0]))
	}
	if jpeg == nil || bytes < 2 || bytes > 128<<20 {
		return nil, fmt.Errorf("invalid Vulkan JPEG output size")
	}
	return C.GoBytes(unsafe.Pointer(jpeg), C.int(bytes)), nil
}

func (e *JPEGEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.bk_vk_close(e.ptr)
		e.ptr = nil
	}
}
