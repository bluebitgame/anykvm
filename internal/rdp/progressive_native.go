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
	rects   [progressiveWindowTiles]C.anykvm_rfx_rect
	ends    [C.ANYKVM_RFX_MAX_PACKETS]C.size_t
	lengths [C.ANYKVM_RFX_MAX_PACKETS]C.size_t
	out     *C.uint8_t
	size    C.size_t
	message [512]C.char
}

func newNativeProgressive(mode string, quantization int) (nativeProgressiveCodec, error) {
	codec, err := newSerialNativeProgressive(mode, quantization)
	if err != nil || mode == "native-scalar" {
		return codec, err
	}
	quantization, _ = normalizeProgressiveQuantization(quantization)
	return newParallelProgressive(codec.(*cProgressive), quantization, min(4, runtime.NumCPU()), func() (progressiveTileCodec, error) {
		next, err := newSerialNativeProgressive(mode, quantization)
		if err != nil {
			return nil, err
		}
		return next.(*cProgressive), nil
	}), nil
}

func newSerialNativeProgressive(mode string, quantization int) (nativeProgressiveCodec, error) {
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
	return e.run(pixels, w, h, rects, frame, first, format, extraBits, upgrade, cache, false)
}

func (e *cProgressive) encodeTiles(pixels []byte, w, h int, rects []bitmapRect, format progressivePixelFormat, extraBits int, upgrade bool, cache []int32) ([]byte, error) {
	return e.run(pixels, w, h, rects, 0, false, format, extraBits, upgrade, cache, true)
}

func (e *cProgressive) run(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, extraBits int, upgrade bool, cache []int32, tilesOnly bool) ([]byte, error) {
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
	var ok C.int
	if tilesOnly {
		ok = C.bk_rfx_run_tiles(e.ptr, input, C.size_t(len(pixels)), C.int(format),
			C.uint32_t(w), C.uint32_t(h), &e.rects[0], C.size_t(len(rects)), pass, C.uint(extraBits), coefficients, C.size_t(len(cache)),
			&e.out, &e.size, &e.message[0], C.size_t(len(e.message)))
	} else {
		ok = C.bk_rfx_run_pass(e.ptr, input, C.size_t(len(pixels)), C.int(format),
			C.uint32_t(w), C.uint32_t(h), &e.rects[0], C.size_t(len(rects)), C.uint32_t(frame), initial,
			pass, C.uint(extraBits), coefficients, C.size_t(len(cache)),
			&e.out, &e.size, &e.message[0], C.size_t(len(e.message)))
	}
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

func (e *cProgressive) encodePackets(work progressiveTileWork) {
	if len(work.packets) == 0 || len(work.packets) > len(e.ends) {
		for i := range work.packets {
			work.packets[i].err = fmt.Errorf("native Progressive: invalid packet count %d", len(work.packets))
		}
		return
	}
	count := 0
	for i, packet := range work.packets {
		for _, r := range packet.rects {
			if count == len(e.rects) {
				work.packets[0].err = fmt.Errorf("native Progressive: packet group exceeds rectangle limit")
				return
			}
			e.rects[count] = C.anykvm_rfx_rect{x: C.uint16_t(r.x), y: C.uint16_t(r.y), width: C.uint16_t(r.w), height: C.uint16_t(r.h)}
			count++
		}
		e.ends[i] = C.size_t(count)
	}
	var coefficients *C.int32_t
	pass := C.int(C.ANYKVM_RFX_SIMPLE)
	if work.bits != 0 {
		pass = C.ANYKVM_RFX_FIRST
		coefficients = (*C.int32_t)(unsafe.Pointer(&work.cache[0]))
	}
	initial := C.int(0)
	if work.first {
		initial = 1
	}
	ok := C.bk_rfx_run_packets(e.ptr, (*C.uint8_t)(unsafe.Pointer(&work.pixels[0])), C.size_t(len(work.pixels)), C.int(work.format),
		C.uint32_t(work.w), C.uint32_t(work.h), &e.rects[0], C.size_t(count), &e.ends[0], C.size_t(len(work.packets)),
		C.uint32_t(work.frame), initial, pass, C.uint(work.bits), coefficients, C.size_t(len(work.cache)),
		&e.out, &e.size, &e.lengths[0], &e.message[0], C.size_t(len(e.message)))
	runtime.KeepAlive(work.pixels)
	runtime.KeepAlive(work.cache)
	runtime.KeepAlive(e)
	if ok == 0 {
		work.packets[0].err = fmt.Errorf("native Progressive: %s", C.GoString(&e.message[0]))
		return
	}
	if e.out == nil || e.size < 1 || e.size > C.size_t(len(work.packets))*(256<<10) {
		work.packets[0].err = fmt.Errorf("native Progressive: invalid packet group size %d", e.size)
		return
	}
	// Packets share a Go-owned allocation, independent of the reused C buffer.
	data := C.GoBytes(unsafe.Pointer(e.out), C.int(e.size))
	offset := 0
	for i := range work.packets {
		size := int(e.lengths[i])
		if size < 1 || size > 256<<10 || size > len(data)-offset {
			work.packets[0].err = fmt.Errorf("native Progressive: invalid packet size %d", size)
			return
		}
		work.packets[i].data = data[offset : offset+size : offset+size]
		offset += size
	}
	if offset != len(data) {
		work.packets[0].err = fmt.Errorf("native Progressive: incomplete packet group")
	}
}
