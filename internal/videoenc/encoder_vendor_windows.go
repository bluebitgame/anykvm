//go:build windows && cgo && !ffmpeg

package videoenc

/*
#cgo windows LDFLAGS: -ld3d11 -ldxgi -ldxguid -ladvapi32 -lcfgmgr32
#include "vendor_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/videotiming"
	"fmt"
	"log"
	"runtime"
	"sync"
	"unsafe"
)

type vendorEncoder struct {
	mu                                        sync.Mutex
	ptr                                       *C.bk_vendor_encoder
	options                                   Options
	name                                      string
	header                                    []byte
	first                                     bool
	failed                                    error
	gpuLogged, fallbackLogged, gpuUnavailable bool
}

// Vendor SDK headers are compiled in; their driver runtimes are loaded lazily.
// No libavcodec initialization takes place on any of these paths.
func openWindowsVendor(o Options, name string) (Encoder, error) {
	if o.Codec != "h264" && o.Codec != "av1" {
		return nil, ErrUnavailable
	}
	vendor := C.int(-1)
	for i, suffix := range []string{"nvenc", "amf", "qsv"} {
		if name == o.Codec+"_"+suffix {
			vendor = C.int(i)
			break
		}
	}
	if vendor < 0 {
		return nil, fmt.Errorf("unknown native Windows encoder %q", name)
	}
	codec := C.int(0)
	if o.Codec == "av1" {
		codec = 2
	}
	rate := o.Rate.Or(o.FPS)
	adapterSet := C.int(0)
	if o.WindowsAdapter.Set {
		adapterSet = 1
	}
	timing := C.int(0)
	if videotiming.Enabled {
		timing = 1
	}
	var msg [512]C.char
	ptr := C.bk_vendor_open(vendor, C.int(o.Width), C.int(o.Height), C.int(rate.Num), C.int(rate.Den),
		C.int(o.Bitrate), codec, adapterSet, C.uint64_t(o.WindowsAdapter.LUID), timing, &msg[0], C.size_t(len(msg)))
	if ptr == nil {
		return nil, fmt.Errorf("%s native: %s", name, C.GoString(&msg[0]))
	}
	var size C.size_t
	probe := C.bk_vendor_probe(ptr, &size)
	packet, err := mediaFoundationPacket(C.GoBytes(unsafe.Pointer(probe), C.int(size)), nil, o.Codec)
	if err == nil && !mediaFoundationRecovery(packet, o.Codec) {
		err = fmt.Errorf("encoder did not produce a recovery frame")
	}
	if err != nil {
		C.bk_vendor_close(ptr)
		return nil, fmt.Errorf("%s native probe: %w", name, err)
	}
	e := &vendorEncoder{ptr: ptr, options: o, name: name + "_native", first: true}
	if o.Codec == "h264" {
		units, _ := mediaFoundationUnits(packet, 4)
		for _, unit := range units {
			if tag := unit[0] & 31; tag == 7 || tag == 8 {
				e.header = append(e.header, mfStartCode...)
				e.header = append(e.header, unit...)
			}
		}
	}
	return e, nil
}

func (e *vendorEncoder) Name() string { return e.name }

func (e *vendorEncoder) LastNativeTiming() videotiming.Native {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return videotiming.Native{}
	}
	var t C.bk_vendor_timing
	C.bk_vendor_last_timing(e.ptr, &t)
	return videotiming.Native{ConversionStart: int64(t.conversion_start), ConversionEnd: int64(t.conversion_end), EncodeSubmit: int64(t.encode_submit), EncodeComplete: int64(t.encode_complete)}
}

func (e *vendorEncoder) Encode(nv12 []byte, forceIDR bool) ([]byte, error) {
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
	var msg [512]C.char
	force := C.int(0)
	if forceIDR || e.first {
		force = 1
	}
	result := C.bk_vendor_encode(e.ptr, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.size_t(len(nv12)), force,
		&output, &size, &msg[0], C.size_t(len(msg)))
	runtime.KeepAlive(nv12)
	if result < 0 {
		e.failed = fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
		return nil, e.failed
	}
	defer C.free(unsafe.Pointer(output))
	return e.packet(output, size, force != 0)
}

func (e *vendorEncoder) EncodeD3D11Texture(handle uintptr, rotation int, forceIDR bool) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return nil, true, fmt.Errorf("encoder closed")
	}
	if e.failed != nil {
		return nil, true, e.failed
	}
	if e.gpuUnavailable {
		return nil, false, nil
	}
	var output *C.uint8_t
	var size C.size_t
	var msg [512]C.char
	force := C.int(0)
	if forceIDR || e.first {
		force = 1
	}
	result := C.bk_vendor_encode_texture(e.ptr, C.uintptr_t(handle), C.int(rotation), force, &output, &size, &msg[0], C.size_t(len(msg)))
	if result > 0 {
		e.gpuUnavailable = true
		if !e.fallbackLogged {
			log.Printf("videoenc: %s GPU texture unavailable (%s); using CPU readback", e.name, C.GoString(&msg[0]))
			e.fallbackLogged = true
		}
		return nil, false, nil
	}
	if result < 0 {
		e.failed = fmt.Errorf("%s GPU texture: %s", e.name, C.GoString(&msg[0]))
		return nil, true, e.failed
	}
	defer C.free(unsafe.Pointer(output))
	if !e.gpuLogged {
		log.Printf("videoenc: %s D3D11 shared texture -> GPU NV12 -> hardware encoder (no desktop pixel readback)", e.name)
		e.gpuLogged = true
	}
	data, err := e.packet(output, size, force != 0)
	return data, true, err
}

func (e *vendorEncoder) packet(output *C.uint8_t, size C.size_t, force bool) ([]byte, error) {
	if output == nil || size == 0 || size > 32*1024*1024 {
		e.failed = fmt.Errorf("%s: invalid native output size %d", e.name, size)
		return nil, e.failed
	}
	data, err := mediaFoundationPacket(C.GoBytes(unsafe.Pointer(output), C.int(size)), e.header, e.options.Codec)
	if err == nil && force && !mediaFoundationRecovery(data, e.options.Codec) {
		err = fmt.Errorf("%s: encoder did not honor the recovery-frame request", e.name)
	}
	if err != nil {
		e.failed = err
		return nil, err
	}
	e.first = false
	return data, nil
}

func (e *vendorEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.bk_vendor_close(e.ptr)
		e.ptr = nil
	}
}
