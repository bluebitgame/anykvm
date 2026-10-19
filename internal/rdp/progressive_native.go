//go:build cgo && (darwin || linux || windows)

package rdp

/*
#cgo CFLAGS: -O3
#cgo amd64 CFLAGS: -march=x86-64
#include "progressive_native.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

type cProgressive struct {
	ptr     unsafe.Pointer
	label   string
	rects   [progressiveBatchRects]C.anykvm_rfx_rect
	out     *C.uint8_t
	size    C.size_t
	message [512]C.char
}

func newNativeProgressive(mode string, quantization int) (nativeProgressiveCodec, error) {
	quantization, err := normalizeProgressiveQuantization(quantization)
	if err != nil {
		return nil, err
	}
	if (mode == "native-sse2" || mode == "native-avx2") && runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("%w: %s requires AMD64", errProgressiveSIMDUnavailable, mode)
	}
	var message [512]C.char
	ptr := C.bk_rfx_open(&message[0], C.size_t(len(message)))
	if ptr == nil {
		return nil, fmt.Errorf("native Progressive: %s", C.GoString(&message[0]))
	}
	if C.bk_rfx_set_quantization(ptr, C.int(quantization)) == 0 {
		C.bk_rfx_close(ptr)
		return nil, fmt.Errorf("native Progressive: invalid quantization %d", quantization)
	}
	forced := C.int(-1)
	switch mode {
	case "native-scalar":
		forced = C.ANYKVM_RFX_SIMD_SCALAR
	case "native-sse2":
		forced = C.ANYKVM_RFX_SIMD_BASELINE
	case "native-avx2":
		forced = C.ANYKVM_RFX_SIMD_AVX2
	}
	if forced >= 0 && C.bk_rfx_set_simd(ptr, forced) == 0 {
		C.bk_rfx_close(ptr)
		return nil, fmt.Errorf("%w: %s", errProgressiveSIMDUnavailable, mode)
	}
	return &cProgressive{ptr: ptr, label: C.GoString(C.bk_rfx_backend(ptr))}, nil
}

func (e *cProgressive) name() string { return e.label }

func (e *cProgressive) close() {
	if e.ptr != nil {
		C.bk_rfx_close(e.ptr)
		e.ptr = nil
	}
}

func (e *cProgressive) encode(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat) ([]byte, error) {
	return e.encodePass(pixels, w, h, rects, frame, first, format, 0, false, nil)
}

func (e *cProgressive) encodePass(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, extraBits int, upgrade bool, cache []int32) ([]byte, error) {
	for i, r := range rects {
		e.rects[i] = C.anykvm_rfx_rect{x: C.uint16_t(r.x), y: C.uint16_t(r.y), width: C.uint16_t(r.w), height: C.uint16_t(r.h)}
	}
	initial := C.int(0)
	if first {
		initial = 1
	}
	// Input is consumed synchronously; C retains no Go pointers. Output belongs
	// to this context and must be copied before another encode or close.
	var input *C.uint8_t
	var coefficients *C.int32_t
	pass := C.int(C.ANYKVM_RFX_SIMPLE)
	if len(pixels) != 0 {
		input = (*C.uint8_t)(unsafe.Pointer(&pixels[0]))
	}
	if extraBits != 0 {
		pass = C.ANYKVM_RFX_FIRST
		coefficients = (*C.int32_t)(unsafe.Pointer(&cache[0]))
	}
	if upgrade {
		pass = C.ANYKVM_RFX_UPGRADE
	}
	ok := C.bk_rfx_run_pass(e.ptr, input, C.size_t(len(pixels)), C.int(format),
		C.uint32_t(w), C.uint32_t(h), &e.rects[0], C.size_t(len(rects)), C.uint32_t(frame), initial,
		pass, C.uint(extraBits), coefficients, C.size_t(len(cache)),
		&e.out, &e.size, &e.message[0], C.size_t(len(e.message)))
	runtime.KeepAlive(pixels)
	runtime.KeepAlive(cache)
	runtime.KeepAlive(e)
	if ok == 0 {
		return nil, fmt.Errorf("native Progressive: %s", C.GoString(&e.message[0]))
	}
	if e.out == nil || e.size < 1 || e.size > 256<<10 {
		return nil, fmt.Errorf("native Progressive: invalid batch size %d", e.size)
	}
	return C.GoBytes(unsafe.Pointer(e.out), C.int(e.size)), nil
}
