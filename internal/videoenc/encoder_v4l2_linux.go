//go:build linux && cgo

package videoenc

/*
#include "v4l2_linux.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/dmabuf"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

type piEncoder struct {
	mu             sync.Mutex
	ptr            *C.bk_pi_encoder
	options        Options
	header         []byte
	first          bool
	failed         error
	dma            *C.bk_pi_encoder
	path           string
	dmaUnavailable bool
	dmaHeader      []byte
	dmaFirst       bool
}

func openRaspberryPi(o Options) (Encoder, error) {
	if o.Codec != "h264" {
		return nil, ErrUnavailable
	}
	nodes, err := filepath.Glob("/dev/video*")
	if err != nil {
		return nil, err
	}
	for _, path := range nodes {
		device := C.CString(path)
		candidate := C.bk_pi_candidate(device) != 0
		C.free(unsafe.Pointer(device))
		if !candidate {
			continue
		}
		encoder, err := openRaspberryPiDevice(o, path)
		if err == nil {
			log.Printf("videoenc: Raspberry Pi native H.264 encoder %s", path)
			return encoder, nil
		}
		log.Printf("videoenc: Raspberry Pi encoder %s: %v", path, err)
	}
	return nil, ErrUnavailable
}

// Initialization and probes use only the kernel codec API, without libavcodec.
func openRaspberryPiDevice(o Options, path string) (Encoder, error) {
	if o.Codec != "h264" {
		return nil, ErrUnavailable
	}
	rate := o.Rate.Or(o.FPS)
	device := C.CString(path)
	defer C.free(unsafe.Pointer(device))
	var message [512]C.char
	ptr := C.bk_pi_open(device, C.int(o.Width), C.int(o.Height), C.int(rate.Num), C.int(rate.Den),
		C.int(o.Bitrate), &message[0], C.size_t(len(message)))
	if ptr == nil {
		return nil, fmt.Errorf("Raspberry Pi V4L2: %s", C.GoString(&message[0]))
	}
	e := &piEncoder{ptr: ptr, options: o, first: true, path: path, dmaFirst: true}
	pixels := make([]byte, o.Width*o.Height*3/2)
	for i := range pixels {
		pixels[i] = 128
		if i < o.Width*o.Height {
			pixels[i] = 16
		}
	}
	// Exercise continuous operation and a subsequent recovery request, so a
	// driver that opens but buffers pictures cannot hide the software fallback.
	for _, force := range []bool{true, false, true} {
		if _, err := e.Encode(pixels, force); err != nil {
			e.Close()
			return nil, fmt.Errorf("Raspberry Pi V4L2 startup probe: %w", err)
		}
	}
	e.first = true
	return e, nil
}

func (e *piEncoder) Name() string { return "h264_v4l2m2m_native" }

func (e *piEncoder) Encode(nv12 []byte, forceIDR bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("encoder closed")
	}
	if e.failed != nil {
		return nil, e.failed
	}
	if len(nv12) != e.options.Width*e.options.Height*3/2 {
		return nil, fmt.Errorf("invalid native NV12 frame size")
	}
	var output *C.uint8_t
	var size C.size_t
	var message [512]C.char
	force := C.int(0)
	if forceIDR || e.first {
		force = 1
	}
	result := C.bk_pi_encode(e.ptr, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.size_t(len(nv12)), force,
		&output, &size, &message[0], C.size_t(len(message)))
	runtime.KeepAlive(nv12)
	defer C.free(unsafe.Pointer(output))
	if result < 0 {
		e.failed = fmt.Errorf("%s: %s", e.Name(), C.GoString(&message[0]))
		return nil, e.failed
	}
	if output == nil || size == 0 || size > 32*1024*1024 {
		e.failed = fmt.Errorf("%s: invalid encoded access unit size %d", e.Name(), size)
		return nil, e.failed
	}
	packet, header, err := raspberryPiPacket(C.GoBytes(unsafe.Pointer(output), C.int(size)), e.header, force != 0)
	if err != nil {
		e.failed = fmt.Errorf("%s: %w", e.Name(), err)
		return nil, e.failed
	}
	e.header = header
	e.first = false
	return packet, nil
}

func (e *piEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dma != nil {
		C.bk_pi_close(e.dma)
		e.dma = nil
	}
	if e.ptr != nil {
		C.bk_pi_close(e.ptr)
		e.ptr = nil
	}
}

func (e *piEncoder) EncodeDMABUF(f dmabuf.Frame, key bool) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return nil, true, fmt.Errorf("encoder closed")
	}
	if e.failed != nil {
		return nil, true, e.failed
	}
	if e.dmaUnavailable || f.ValidateNV12() != nil || int(f.Width) != e.options.Width || int(f.Height) != e.options.Height ||
		f.PlaneOffsets[0] != 0 || uint64(f.PlaneOffsets[1]) != uint64(f.Pitch)*uint64(f.Height) {
		return nil, false, nil
	}
	var msg [512]C.char
	if e.dma == nil {
		path := C.CString(e.path)
		defer C.free(unsafe.Pointer(path))
		rate := e.options.Rate.Or(e.options.FPS)
		e.dma = C.bk_pi_open_dma(path, C.int(f.Width), C.int(f.Height), C.int(rate.Num), C.int(rate.Den), C.int(e.options.Bitrate), C.uint(f.Pitch), &msg[0], 512)
		if e.dma == nil {
			e.dmaUnavailable = true
			log.Printf("videoenc: V4L2 DMA-BUF unavailable: %s", C.GoString(&msg[0]))
			return nil, false, nil
		}
	}
	force := C.int(0)
	if key || e.dmaFirst {
		force = 1
	}
	var out *C.uint8_t
	var size C.size_t
	result := C.bk_pi_encode_dma(e.dma, C.int(f.FD), C.size_t(f.Size), force, &out, &size, &msg[0], 512)
	defer C.free(unsafe.Pointer(out))
	if result > 0 {
		C.bk_pi_close(e.dma)
		e.dma = nil
		e.dmaUnavailable = true
		return nil, false, nil
	}
	if result < 0 {
		C.bk_pi_close(e.dma)
		e.dma = nil
		e.failed = fmt.Errorf("V4L2 DMA-BUF: %s", C.GoString(&msg[0]))
		return nil, true, e.failed
	}
	if out == nil || size == 0 || size > 32*1024*1024 {
		e.failed = fmt.Errorf("invalid V4L2 DMA-BUF output")
		return nil, true, e.failed
	}
	data, header, err := raspberryPiPacket(C.GoBytes(unsafe.Pointer(out), C.int(size)), e.dmaHeader, force != 0)
	if err != nil {
		e.failed = err
		return nil, true, err
	}
	e.dmaHeader = header
	e.dmaFirst = false
	return data, true, nil
}
