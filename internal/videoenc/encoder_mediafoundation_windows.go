//go:build windows && cgo

package videoenc

/*
#cgo windows LDFLAGS: -lmfuuid -luuid -lole32 -loleaut32
#include "mediafoundation_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"anykvm/internal/av1"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

type mediaFoundationEncoder struct {
	mu                sync.Mutex
	ptr               *C.bk_mf_encoder
	options           Options
	name              string
	failed            error
	first             bool
	sampleUnavailable bool
	header            []byte
}

func openMediaFoundation(o Options, hardware bool) (Encoder, error) {
	codec := C.int(0)
	if o.Codec == "hevc" {
		codec = 1
	} else if o.Codec == "av1" {
		codec = 2
	}
	useHardware := C.int(0)
	if hardware {
		useHardware = 1
	}
	rate := o.Rate.Or(o.FPS)
	var msg [512]C.char
	ptr := C.bk_mf_open(C.int(o.Width), C.int(o.Height), C.int(rate.Num), C.int(rate.Den), C.int(o.Bitrate), codec, useHardware, &msg[0], C.size_t(len(msg)))
	if ptr == nil {
		return nil, fmt.Errorf("Media Foundation: %s", C.GoString(&msg[0]))
	}
	var size, headerSize C.size_t
	probe := C.bk_mf_probe(ptr, &size)
	header := C.bk_mf_header(ptr, &headerSize)
	packet, err := mediaFoundationPacket(C.GoBytes(unsafe.Pointer(probe), C.int(size)), C.GoBytes(unsafe.Pointer(header), C.int(headerSize)), o.Codec)
	if err == nil && !mediaFoundationRecovery(packet, o.Codec) {
		err = fmt.Errorf("encoder did not produce a recovery frame")
	}
	if err != nil {
		C.bk_mf_close(ptr)
		return nil, fmt.Errorf("Media Foundation probe: %w", err)
	}
	e := &mediaFoundationEncoder{ptr: ptr, options: o, name: C.GoString(C.bk_mf_name(ptr)), first: true}
	// Some drivers put parameter sets only in the first output sample and never
	// expose MF_MT_MPEG_SEQUENCE_HEADER. Keep those sets before discarding the probe.
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

func (e *mediaFoundationEncoder) Name() string { return e.name }

func (e *mediaFoundationEncoder) Encode(nv12 []byte, forceIDR bool) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return nil, fmt.Errorf("encoder closed")
	}
	if e.failed != nil {
		return nil, e.failed
	}
	if len(nv12) != e.options.Width*e.options.Height*3/2 {
		return nil, fmt.Errorf("invalid Media Foundation NV12 frame size")
	}
	var output *C.uint8_t
	var size C.size_t
	var msg [512]C.char
	force := C.int(0)
	if forceIDR || e.first {
		force = 1
	}
	result := C.bk_mf_encode(e.ptr, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.size_t(len(nv12)), force, &output, &size, &msg[0], C.size_t(len(msg)))
	runtime.KeepAlive(nv12)
	if result < 0 {
		e.failed = fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
		return nil, e.failed
	}
	defer C.free(unsafe.Pointer(output))
	if output == nil || size == 0 || size > 32*1024*1024 {
		e.failed = fmt.Errorf("%s: invalid output size %d", e.name, size)
		return nil, e.failed
	}
	return e.packet(output, size, force != 0)
}

func (e *mediaFoundationEncoder) packet(output *C.uint8_t, size C.size_t, force bool) ([]byte, error) {
	var headerSize C.size_t
	header := C.bk_mf_header(e.ptr, &headerSize)
	parameters := C.GoBytes(unsafe.Pointer(header), C.int(headerSize))
	if len(parameters) == 0 {
		parameters = e.header
	}
	data, err := mediaFoundationPacket(C.GoBytes(unsafe.Pointer(output), C.int(size)), parameters, e.options.Codec)
	if err == nil && force {
		if !mediaFoundationRecovery(data, e.options.Codec) {
			err = fmt.Errorf("%s: encoder did not honor the recovery-frame request", e.name)
		}
	}
	if err != nil {
		e.failed = err
		return nil, err
	}
	e.first = false
	return data, nil
}

func mediaFoundationRecovery(data []byte, codec string) bool {
	if codec == "av1" {
		info, err := av1.Inspect(data)
		return err == nil && info.Recovery
	}
	units, err := mediaFoundationUnits(data, 4)
	if err != nil {
		return false
	}
	for _, unit := range units {
		if codec == "h264" && unit[0]&31 == 5 {
			return true
		}
		if codec == "hevc" {
			if tag := unit[0] >> 1 & 63; tag >= 16 && tag <= 23 {
				return true
			}
		}
	}
	return false
}

func (e *mediaFoundationEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.bk_mf_close(e.ptr)
		e.ptr = nil
	}
}

func (e *mediaFoundationEncoder) EncodeMediaFoundationSample(sample unsafe.Pointer, key bool) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sampleUnavailable {
		return nil, false, nil
	}
	if e.ptr == nil {
		return nil, true, fmt.Errorf("encoder closed")
	}
	if e.failed != nil {
		return nil, true, e.failed
	}
	var output *C.uint8_t
	var size C.size_t
	var msg [512]C.char
	force := C.int(0)
	if key || e.first {
		force = 1
	}
	result := C.bk_mf_encode_sample(e.ptr, sample, force, &output, &size, &msg[0], 512)
	if result > 0 {
		e.sampleUnavailable = true
		return nil, false, nil
	}
	if result < 0 {
		e.failed = fmt.Errorf("%s borrowed sample: %s", e.name, C.GoString(&msg[0]))
		return nil, true, e.failed
	}
	defer C.free(unsafe.Pointer(output))
	if output == nil || size == 0 || size > 32*1024*1024 {
		e.failed = fmt.Errorf("invalid borrowed-sample output")
		return nil, true, e.failed
	}
	data, err := e.packet(output, size, force != 0)
	return data, true, err
}
