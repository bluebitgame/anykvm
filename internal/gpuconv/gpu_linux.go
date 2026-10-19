//go:build linux && cgo

package gpuconv

/*
#cgo LDFLAGS: -ldl
#include "native_linux.h"
*/
import "C"

import (
	"anykvm/internal/dmabuf"
	_ "embed"
	"fmt"
	"sync"
	"unsafe"
)

//go:embed shaders/desktop.spv
var shader []byte

// Converter owns a Vulkan device matching the captured DRM primary device.
// Images keep the device alive after Close until their final lease is released.
type Converter struct {
	mu     sync.Mutex
	ptr    *C.bk_vk
	closed bool
	images int
}
type Image struct {
	owner         *Converter
	ptr           *C.bk_vk_image
	Width, Height int
	NV12          bool
}

func New(major, minor uint32) (*Converter, error) {
	var msg [512]C.char
	ptr := C.bk_vk_open(C.uint32_t(major), C.uint32_t(minor), unsafe.Pointer(&shader[0]), C.size_t(len(shader)), 0, &msg[0], 512)
	if ptr == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, C.GoString(&msg[0]))
	}
	return &Converter{ptr: ptr}, nil
}
func (c *Converter) Convert(f dmabuf.Frame, w, h int, nv12 bool) (*Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("Vulkan converter closed")
	}
	if f.FD < 0 || f.Width == 0 || f.Height == 0 || w < 2 || h < 2 || w > 8192 || h > 8192 || (nv12 && (w%2 != 0 || h%2 != 0)) {
		return nil, fmt.Errorf("invalid Vulkan conversion dimensions or DMA-BUF")
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	source := nativeSource(f)
	var msg [512]C.char
	format := 0
	if nv12 {
		format = 1
	}
	ptr := C.bk_vk_convert(c.ptr, &source, C.int(w), C.int(h), C.int(format), &msg[0], 512)
	if ptr == nil {
		return nil, fmt.Errorf("Vulkan conversion: %s", C.GoString(&msg[0]))
	}
	c.images++
	return &Image{owner: c, ptr: ptr, Width: w, Height: h, NV12: nv12}, nil
}

// Readback is explicit and only used by CPU consumers or encoder fallback.
func (i *Image) Readback() ([]byte, error) {
	c := i.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if i.ptr == nil {
		return nil, fmt.Errorf("Vulkan image closed")
	}
	var data unsafe.Pointer
	var size C.size_t
	var msg [512]C.char
	if C.bk_vk_readback(i.ptr, &data, &size, &msg[0], 512) < 0 {
		return nil, fmt.Errorf("Vulkan readback: %s", C.GoString(&msg[0]))
	}
	pitch := int(C.bk_vk_image_pitch(i.ptr))
	row := i.Width * 4
	rows := i.Height
	if i.NV12 {
		row = i.Width
		rows = rows * 3 / 2
	}
	if uint64(pitch)*uint64(rows) > uint64(size) {
		return nil, fmt.Errorf("invalid Vulkan readback size")
	}
	mapped := unsafe.Slice((*byte)(data), int(size))
	out := make([]byte, row*rows)
	for y := 0; y < rows; y++ {
		copy(out[y*row:(y+1)*row], mapped[y*pitch:y*pitch+row])
	}
	return out, nil
}
func (i *Image) Close() {
	c := i.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if i.ptr != nil {
		C.bk_vk_image_free(i.ptr)
		i.ptr = nil
		c.images--
		c.finish()
	}
}
func (c *Converter) finish() {
	if c.closed && c.images == 0 && c.ptr != nil {
		C.bk_vk_close(c.ptr)
		c.ptr = nil
	}
}
func (c *Converter) Close() { c.mu.Lock(); defer c.mu.Unlock(); c.closed = true; c.finish() }

func nativeSource(f dmabuf.Frame) C.bk_vk_source {
	source := C.bk_vk_source{fd: C.int(f.FD), width: C.uint32_t(f.Width), height: C.uint32_t(f.Height), fourcc: C.uint32_t(f.FourCC),
		pitch: C.uint32_t(f.Pitch), offset: C.uint32_t(f.Offset), modifier: C.uint64_t(f.Modifier), size: C.uint64_t(f.Size),
		x: C.uint32_t(f.X), y: C.uint32_t(f.Y), crop_width: C.uint32_t(f.CropWidth), crop_height: C.uint32_t(f.CropHeight), rotation: C.int(f.Rotation)}

	source.planes = C.uint32_t(f.PlaneCount)
	for i := 0; i < 4; i++ {
		source.pitches[i] = C.uint32_t(f.PlanePitches[i])
		source.offsets[i] = C.uint32_t(f.PlaneOffsets[i])
	}
	return source

}
