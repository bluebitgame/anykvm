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

// D3D11Encoder supports standalone JPEG and compact changed-MCU readback.
// Calls, including Close, must be serialized by the capture owner.
type D3D11Encoder struct {
	ptr        *C.bk_jpeg_gpu
	parameters jpegFrameParameters
	regions    *RestartJPEG
	damage     DesktopDamageState
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
	if e != nil {
		e.regions = nil
		e.damage = DesktopDamageState{}
	}
	return e.encodeTexture(handle, rotation, width, height, quality, false, nil)
}

// EncodeRegions returns an immutable CPU snapshot after reading only changed MCU records.
func (e *D3D11Encoder) EncodeRegions(handle uintptr, rotation, width, height, quality int) (*RestartJPEG, error) {
	return e.EncodeRegionsWithDamage(handle, rotation, width, height, quality, nil)
}

// EncodeRegionsWithDamage limits comparison to candidates when DXGI acquisition
// continuity is known. Configuration changes always force full initialization.
func (e *D3D11Encoder) EncodeRegionsWithDamage(handle uintptr, rotation, width, height, quality int, damage *DesktopDamage) (*RestartJPEG, error) {
	packet, err := e.encodeTexture(handle, rotation, width, height, quality, true, damage)
	if err != nil {
		return nil, err
	}
	next, err := updateGPURegions(e.regions, e.parameters.header, packet, width, height)
	if err == nil {
		e.regions = next
	}
	return next, err
}

func (e *D3D11Encoder) encodeTexture(handle uintptr, rotation, width, height, quality int, regions bool, damage *DesktopDamage) ([]byte, error) {
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
	mode := C.int(0)
	if regions {
		mode = 1
	}

	var nativeDamage *C.bk_jpeg_damage
	var plan C.bk_jpeg_damage
	if regions {
		if tiles, valid := e.damage.CandidateTiles(damage, width, height, rotation); valid {
			plan.count = C.size_t(len(tiles))
			plan.source_width = C.uint32_t(damage.SourceWidth)
			plan.source_height = C.uint32_t(damage.SourceHeight)
			if len(tiles) > 0 {
				storage := C.malloc(C.size_t(len(tiles) * 4))
				if storage != nil {
					defer C.free(storage)
					copy(unsafe.Slice((*uint32)(storage), len(tiles)), tiles)
					plan.tiles = (*C.uint32_t)(storage)
					nativeDamage = &plan
				}
			} else {
				nativeDamage = &plan
			}
		}
	}
	ok := C.bk_jpeg_gpu_encode_damage(e.ptr, C.uintptr_t(handle), C.int(rotation), C.int(width), C.int(height),
		(*C.uint32_t)(unsafe.Pointer(&e.parameters.quantization[0])), (*C.uint32_t)(unsafe.Pointer(&jpegPackedEntropyCodes[0])),
		(*C.uint8_t)(unsafe.Pointer(&header[0])), C.size_t(len(header)), mode, nativeDamage, &jpeg, &jpegBytes, &message[0], C.size_t(len(message)))
	defer C.free(unsafe.Pointer(jpeg))
	runtime.KeepAlive(e)
	if ok == 0 {
		return nil, fmt.Errorf("%s", C.GoString(&message[0]))
	}
	if jpeg == nil || jpegBytes < 2 || jpegBytes > 128<<20 {
		return nil, fmt.Errorf("invalid GPU JPEG output size")
	}
	e.damage.Commit(damage)
	return C.GoBytes(unsafe.Pointer(jpeg), C.int(jpegBytes)), nil
}

func (e *D3D11Encoder) Close() {
	if e != nil && e.ptr != nil {
		C.bk_jpeg_gpu_close(e.ptr)
		e.ptr = nil
		e.regions = nil
		e.damage = DesktopDamageState{}
	}
}
