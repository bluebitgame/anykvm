//go:build windows && cgo

package gpuconv

/*
#cgo d3d11_precompiled CFLAGS: -DBK_PROGRESSIVE_PRECOMPILED
#cgo LDFLAGS: -ld3d11 -ldxgi -ldxguid
#include "progressive_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// ProgressiveCompute owns a per-session GPU cache. Run and Close must be
// serialized by the encoder owner, just like the native CPU encoder.
type ProgressiveCompute struct {
	ptr                   *C.bk_d3d_progressive
	source, surfaceSource string
	adapter               uint64
	adapterSet            bool
	closed                bool
}

func NewProgressiveCompute(source string, _ [4][]byte) (*ProgressiveCompute, error) {
	if source == "" {
		return nil, fmt.Errorf("missing Progressive compute shader")
	}
	var message [1024]C.char
	p := C.bk_d3d_progressive_new((*C.char)(unsafe.Pointer(unsafe.StringData(source))), C.size_t(len(source)), 0, 0, &message[0], 1024)
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&message[0]))
	}
	return &ProgressiveCompute{ptr: p, source: source}, nil
}
func (p *ProgressiveCompute) Run(input []byte, meta []uint32) ([]byte, [48]uint32, error) {
	if len(meta) == 16 && meta[6] != 0 {
		return nil, [48]uint32{}, fmt.Errorf("resident surface requires RunSurface")
	}
	return p.run(input, meta)
}
func (p *ProgressiveCompute) RunSurface(meta []uint32) ([]byte, [48]uint32, error) {
	if len(meta) != 16 || meta[6] == 0 {
		return nil, [48]uint32{}, fmt.Errorf("missing resident GPU geometry")
	}
	return p.run([]byte{0, 0, 0, 0}, meta)
}
func (p *ProgressiveCompute) run(input []byte, meta []uint32) ([]byte, [48]uint32, error) {
	var lengths [48]uint32
	if _, err := ProgressiveMetadata(input, meta); err != nil {
		return nil, lengths, err
	}
	if p == nil || p.ptr == nil {
		return nil, lengths, fmt.Errorf("Progressive compute closed")
	}
	var message [1024]C.char
	var output *C.uint8_t
	var size C.size_t
	status := C.bk_d3d_progressive_run(p.ptr, (*C.uint8_t)(unsafe.Pointer(&input[0])), C.size_t(len(input)),
		(*C.uint32_t)(unsafe.Pointer(&meta[0])), &output, &size, (*C.uint32_t)(unsafe.Pointer(&lengths[0])), &message[0], 1024)
	defer C.free(unsafe.Pointer(output))
	runtime.KeepAlive(input)
	runtime.KeepAlive(meta)
	if status != 0 {
		return nil, lengths, fmt.Errorf("Progressive D3D11: %s", C.GoString(&message[0]))
	}
	if output == nil || size == 0 || size > 48*32768 {
		return nil, lengths, fmt.Errorf("invalid Progressive GPU output")
	}
	return C.GoBytes(unsafe.Pointer(output), C.int(size)), lengths, nil
}
func (p *ProgressiveCompute) Close() {
	if p != nil && p.ptr != nil {
		C.bk_d3d_progressive_free(p.ptr)
		p.ptr = nil
	}
	if p != nil {
		p.closed = true
	}
}
func (p *ProgressiveCompute) ConfigureSurface(source string, _ []byte) error {
	if p == nil || p.closed || p.ptr == nil || source == "" || p.surfaceSource != "" {
		return fmt.Errorf("invalid surface shader configuration")
	}
	var message [1024]C.char
	if C.bk_d3d_progressive_surface_init(p.ptr, (*C.char)(unsafe.Pointer(unsafe.StringData(source))), C.size_t(len(source)), &message[0], 1024) != 0 {
		return fmt.Errorf("GPU surface shader: %s", C.GoString(&message[0]))
	}
	p.surfaceSource = source
	return nil
}
func (p *ProgressiveCompute) PrepareSurface(s *Surface, w, h int, force bool) ([]uint32, bool, error) {
	return p.PrepareSurfaceMoves(s, w, h, force, nil)
}
func (p *ProgressiveCompute) PrepareSurfaceMoves(s *Surface, w, h int, force bool, moves []SurfaceMove) ([]uint32, bool, error) {
	parameters, err := SurfaceMoveParameters(s, w, h, force, moves)
	if err != nil {
		return nil, false, err
	}
	if p == nil || p.closed || s.SharedTexture == 0 || p.surfaceSource == "" {
		return nil, false, fmt.Errorf("unsupported GPU snapshot")
	}
	changed := !p.adapterSet || p.adapter != s.AdapterLUID
	var message [1024]C.char
	if changed {
		native := C.bk_d3d_progressive_new((*C.char)(unsafe.Pointer(unsafe.StringData(p.source))), C.size_t(len(p.source)), C.uint64_t(s.AdapterLUID), 1, &message[0], 1024)
		if native == nil {
			return nil, true, fmt.Errorf("capture GPU selection: %s", C.GoString(&message[0]))
		}
		if C.bk_d3d_progressive_surface_init(native, (*C.char)(unsafe.Pointer(unsafe.StringData(p.surfaceSource))), C.size_t(len(p.surfaceSource)), &message[0], 1024) != 0 {
			C.bk_d3d_progressive_free(native)
			return nil, true, fmt.Errorf("capture GPU surface shader: %s", C.GoString(&message[0]))
		}
		C.bk_d3d_progressive_free(p.ptr)
		p.ptr = native
		p.adapter = s.AdapterLUID
		p.adapterSet = true
	}
	flags := make([]uint32, ((w+63)/64)*((h+63)/64))
	status := C.bk_d3d_progressive_surface_prepare(p.ptr, C.uintptr_t(s.SharedTexture), (*C.uint32_t)(unsafe.Pointer(&parameters[0])), C.size_t(len(parameters)), (*C.uint32_t)(unsafe.Pointer(&flags[0])), &message[0], 1024)
	runtime.KeepAlive(s)
	if status != 0 {
		return nil, changed, fmt.Errorf("GPU surface preparation: %s", C.GoString(&message[0]))
	}
	return flags, changed, nil
}
func (p *ProgressiveCompute) CommitSurface() error {
	if p == nil || p.closed || p.ptr == nil {
		return fmt.Errorf("GPU surface encoder closed")
	}
	var message [1024]C.char
	if C.bk_d3d_progressive_surface_commit(p.ptr, &message[0], 1024) != 0 {
		return fmt.Errorf("GPU surface commit: %s", C.GoString(&message[0]))
	}
	return nil
}
