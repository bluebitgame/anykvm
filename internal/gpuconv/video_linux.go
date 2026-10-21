//go:build linux && cgo

package gpuconv

/*
#include <stdlib.h>
#include "native_linux.h"
*/
import "C"

import (
	"anykvm/internal/dmabuf"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// VideoEncoder owns one synchronous, bounded native Vulkan Video session.
// A successful call completes one H.264/HEVC access unit or AV1 temporal unit.
// Raw pixels stay on the GPU when EncodeSource is used. EncodeNV12 is the explicit CPU input path.
type VideoEncoder struct {
	mu        sync.Mutex
	context   *C.bk_vk
	ptr       *C.bk_vk_encoder
	failed    error
	hasSource bool
}

// SupportsVideoCodec trusts the driver's extension/profile/format declarations.
// No video session or synthetic picture is created for capability negotiation.
func SupportsVideoCodec(codec string) bool {
	mode := C.int(C.BK_VK_QUERY_HEVC)
	if codec == "av1" {
		mode = C.BK_VK_QUERY_AV1
	} else if codec != "hevc" {
		return false
	}
	var msg [512]C.char
	context := C.bk_vk_open(0, 0, nil, 0, mode, &msg[0], 512)
	if context == nil {
		return false
	}
	defer C.bk_vk_close(context)
	return C.bk_vk_encoder_supports(context) != 0
}

func NewVideoEncoder(major, minor uint32, w, h, num, den, bitrate int) (*VideoEncoder, error) {
	return NewVideoEncoderCodec(major, minor, w, h, num, den, bitrate, "h264")
}

func NewVideoEncoderCodec(major, minor uint32, w, h, num, den, bitrate int, codec string) (*VideoEncoder, error) {
	mode := C.int(C.BK_VK_VIDEO)
	switch codec {
	case "h264":
	case "hevc":
		mode = C.BK_VK_VIDEO_HEVC
	case "av1":
		mode = C.BK_VK_VIDEO_AV1
	default:
		return nil, fmt.Errorf("unsupported Vulkan Video codec %q", codec)
	}
	if w < 2 || h < 2 || w > 8192 || h > 8192 || w%2 != 0 || h%2 != 0 || num < 1 || den < 1 || bitrate < 1 || bitrate > 1000000 {
		return nil, fmt.Errorf("invalid Vulkan Video options")
	}
	var msg [512]C.char
	context := C.bk_vk_open(C.uint32_t(major), C.uint32_t(minor), unsafe.Pointer(&shader[0]), C.size_t(len(shader)), mode, &msg[0], 512)
	if context == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&msg[0]))
	}
	ptr := C.bk_vk_encoder_new(context, C.int(w), C.int(h), C.int(num), C.int(den), C.int(bitrate), &msg[0], 512)
	if ptr == nil {
		C.bk_vk_close(context)
		return nil, fmt.Errorf("Vulkan Video %s: %s", codec, C.GoString(&msg[0]))
	}
	return &VideoEncoder{context: context, ptr: ptr}, nil
}
func (e *VideoEncoder) encode(source *C.bk_vk_source, pixels []byte, force bool) ([]byte, error) {
	if e.ptr == nil {
		return nil, fmt.Errorf("Vulkan Video encoder closed")
	}
	if e.failed != nil {
		return nil, e.failed
	}
	var data *C.uint8_t
	var size C.size_t
	var msg [512]C.char
	var input *C.uint8_t
	if len(pixels) > 0 {
		input = (*C.uint8_t)(unsafe.Pointer(&pixels[0]))
	}
	key := 0
	if force {
		key = 1
	}
	status := C.bk_vk_encoder_encode(e.ptr, source, input, C.size_t(len(pixels)), C.int(key), &data, &size, &msg[0], 512)
	runtime.KeepAlive(pixels)
	defer C.free(unsafe.Pointer(data))
	if status > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&msg[0]))
	}
	if status < 0 {
		e.failed = fmt.Errorf("Vulkan Video: %s", C.GoString(&msg[0]))
		return nil, e.failed
	}
	if data == nil || size == 0 || size > 128*1024*1024 {
		e.failed = fmt.Errorf("Vulkan Video returned an invalid access unit")
		return nil, e.failed
	}
	return C.GoBytes(unsafe.Pointer(data), C.int(size)), nil
}
func (e *VideoEncoder) EncodeSource(f dmabuf.Frame, force bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	validate := f.Validate
	if f.FourCC == dmabuf.NV12 {
		validate = f.ValidateNV12
	}
	if err := validate(); err != nil {
		return nil, err
	}
	source := nativeSource(f)
	data, err := e.encode(&source, nil, force)
	if err == nil {
		e.hasSource = f.FourCC != dmabuf.NV12
	}
	return data, err
}
func (e *VideoEncoder) EncodeNV12(pixels []byte, force bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hasSource = false
	if len(pixels) == 0 {
		return nil, fmt.Errorf("empty Vulkan NV12 pixels")
	}
	return e.encode(nil, pixels, force)
}
func (e *VideoEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.bk_vk_encoder_free(e.ptr)
		e.ptr = nil
		C.bk_vk_close(e.context)
		e.context = nil
	}
}

// EncodeLast re-encodes the owned GPU NV12 image of the last RGB source. It
// supports an IDR on an idle desktop after the SPICE producer was acknowledged.
// It neither retains a borrowed DMA-BUF nor uploads or reads back pixels.
func (e *VideoEncoder) EncodeLast(force bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasSource {
		return nil, fmt.Errorf("no cached GPU RGB source")
	}
	return e.encode(nil, nil, force)
}
