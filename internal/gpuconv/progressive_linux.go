//go:build linux && cgo

package gpuconv

/*
#include "native_linux.h"
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
	context    *C.bk_vk
	ptr        *C.bk_vk_progressive
	owner      *Converter
	stages     [4][]byte
	surfaceSPV []byte
	closed     bool
}

func NewProgressiveCompute(_ string, stages [4][]byte) (*ProgressiveCompute, error) {
	for _, s := range stages {
		if len(s) == 0 || len(s)%4 != 0 {
			return nil, fmt.Errorf("invalid Progressive SPIR-V")
		}
	}
	var message [1024]C.char
	ptr := func(i int) unsafe.Pointer { return unsafe.Pointer(&stages[i][0]) }
	v := C.bk_vk_open(0, 0, ptr(0), C.size_t(len(stages[0])), C.BK_VK_PROGRESSIVE, &message[0], 1024)
	if v == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&message[0]))
	}
	p := C.bk_vk_progressive_new(v, ptr(1), C.size_t(len(stages[1])), ptr(2), C.size_t(len(stages[2])), ptr(3), C.size_t(len(stages[3])), &message[0], 1024)
	if p == nil {
		C.bk_vk_close(v)
		return nil, fmt.Errorf("Progressive compute: %s", C.GoString(&message[0]))
	}
	return &ProgressiveCompute{context: v, ptr: p, stages: stages}, nil
}
func (p *ProgressiveCompute) Run(input []byte, meta []uint32) ([]byte, [48]uint32, error) {
	if len(meta) == 16 && meta[6] != 0 {
		return nil, [48]uint32{}, fmt.Errorf("resident surface requires RunSurface")
	}
	return p.run(input, meta)
}
func (p *ProgressiveCompute) RunSurface(meta []uint32) ([]byte, [48]uint32, error) {
	if len(meta) != 16 || meta[6] == 0 {
		return nil, [48]uint32{}, fmt.Errorf("missing resident surface geometry")
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
	if p.owner != nil {
		p.owner.mu.Lock()
		defer p.owner.mu.Unlock()
	}
	var message [1024]C.char
	var output *C.uint8_t
	var size C.size_t
	status := C.bk_vk_progressive_run(p.ptr, (*C.uint8_t)(unsafe.Pointer(&input[0])), C.size_t(len(input)),
		(*C.uint32_t)(unsafe.Pointer(&meta[0])), &output, &size, (*C.uint32_t)(unsafe.Pointer(&lengths[0])), &message[0], 1024)
	defer C.free(unsafe.Pointer(output))
	runtime.KeepAlive(input)
	runtime.KeepAlive(meta)
	if status != 0 {
		return nil, lengths, fmt.Errorf("Progressive Vulkan: %s", C.GoString(&message[0]))
	}
	if output == nil || size == 0 || size > 48*32768 {
		return nil, lengths, fmt.Errorf("invalid Progressive GPU output")
	}
	return C.GoBytes(unsafe.Pointer(output), C.int(size)), lengths, nil
}
func (p *ProgressiveCompute) release() {
	if p.owner != nil {
		p.owner.mu.Lock()
		defer p.owner.mu.Unlock()
	}
	if p.ptr != nil {
		C.bk_vk_progressive_free(p.ptr)
		p.ptr = nil
	}
	if p.context != nil {
		C.bk_vk_close(p.context)
		p.context = nil
	}
	if p.owner != nil {
		p.owner.images--
		p.owner.finish()
		p.owner = nil
	}
}
func (p *ProgressiveCompute) Close() {
	if p != nil && !p.closed {
		p.release()
		p.closed = true
	}
}

func (p *ProgressiveCompute) ConfigureSurface(_ string, spirv []byte) error {
	if p == nil || p.closed || p.ptr == nil || len(spirv) == 0 || len(spirv)%4 != 0 || p.surfaceSPV != nil {
		return fmt.Errorf("invalid surface shader configuration")
	}
	var message [1024]C.char
	if C.bk_vk_progressive_surface_init(p.ptr, unsafe.Pointer(&spirv[0]), C.size_t(len(spirv)), &message[0], 1024) != 0 {
		return fmt.Errorf("Progressive surface shader: %s", C.GoString(&message[0]))
	}
	p.surfaceSPV = spirv
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
	if p == nil || p.closed || p.surfaceSPV == nil || s.Image == nil || s.Image.NV12 {
		return nil, false, fmt.Errorf("unsupported GPU surface")
	}
	i := s.Image
	c := i.owner
	if c == nil || i.Width != s.Width || i.Height != s.Height || s.Rotation != 0 {
		return nil, false, fmt.Errorf("invalid Vulkan snapshot")
	}
	changed := p.owner != c
	if changed {
		p.release()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if i.ptr == nil || c.ptr == nil {
		return nil, changed, fmt.Errorf("GPU snapshot is closed")
	}
	var message [1024]C.char
	if changed {
		pointer := func(j int) unsafe.Pointer { return unsafe.Pointer(&p.stages[j][0]) }
		v := C.bk_vk_progressive_context(i.ptr, pointer(0), C.size_t(len(p.stages[0])), &message[0], 1024)
		if v == nil {
			return nil, true, fmt.Errorf("GPU snapshot context: %s", C.GoString(&message[0]))
		}
		codec := C.bk_vk_progressive_new(v, pointer(1), C.size_t(len(p.stages[1])), pointer(2), C.size_t(len(p.stages[2])), pointer(3), C.size_t(len(p.stages[3])), &message[0], 1024)
		if codec == nil {
			C.bk_vk_close(v)
			return nil, true, fmt.Errorf("GPU snapshot pipelines: %s", C.GoString(&message[0]))
		}
		if C.bk_vk_progressive_surface_init(codec, unsafe.Pointer(&p.surfaceSPV[0]), C.size_t(len(p.surfaceSPV)), &message[0], 1024) != 0 {
			C.bk_vk_progressive_free(codec)
			C.bk_vk_close(v)
			return nil, true, fmt.Errorf("GPU snapshot surface shader: %s", C.GoString(&message[0]))
		}
		p.context, p.ptr, p.owner = v, codec, c
		c.images++ // Separate lease for the child context.
	}
	flags := make([]uint32, ((w+63)/64)*((h+63)/64))
	status := C.bk_vk_progressive_surface_prepare(p.ptr, i.ptr, (*C.uint32_t)(unsafe.Pointer(&parameters[0])), C.size_t(len(parameters)), (*C.uint32_t)(unsafe.Pointer(&flags[0])), &message[0], 1024)
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
	if p.owner != nil {
		p.owner.mu.Lock()
		defer p.owner.mu.Unlock()
	}
	var message [1024]C.char
	if C.bk_vk_progressive_surface_commit(p.ptr, &message[0], 1024) != 0 {
		return fmt.Errorf("GPU surface commit: %s", C.GoString(&message[0]))
	}
	return nil
}
