//go:build cgo && darwin

package videoenc

/*
#cgo darwin LDFLAGS: -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework Foundation
#include "native.h"
#include <stdlib.h>
*/
import "C"
import (
	"anykvm/internal/framebuf"
	"fmt"
	"log"
	"runtime"
	"sync"
	"unsafe"
)

type nativeEncoder struct {
	mu      sync.Mutex
	ptr     *C.bk_encoder
	options Options
	name    string
	syncID  uint64
}

func openNative(o Options, backend, device string) (Encoder, error) {
	b, d := C.CString(backend), C.CString(device)
	defer C.free(unsafe.Pointer(b))
	defer C.free(unsafe.Pointer(d))
	var msg [512]C.char
	hevc := 0
	if o.Codec == "hevc" {
		hevc = 1
	}
	disableLowLatency := C.int(0)
	if o.DisableLowLatencyRateControl {
		disableLowLatency = 1
	}
	profileIndex := C.int(0)
	profile := o.H264Profile
	switch profile {
	case "", "constrained_baseline":
		profile = "constrained_baseline"
	case "baseline":
		profileIndex = 1
	case "main":
		profileIndex = 2
	case "high":
		profileIndex = 3
	default:
		return nil, fmt.Errorf("invalid H.264 profile %q", profile)
	}
	p := C.bk_encoder_open(b, d, C.int(o.Width), C.int(o.Height), C.int(o.Rate.Num), C.int(o.Rate.Den), C.int(o.Bitrate), C.int(hevc), disableLowLatency, profileIndex, &msg[0], C.size_t(len(msg)))
	if p == nil {
		return nil, fmt.Errorf("%s: %s", backend, C.GoString(&msg[0]))
	}
	if hevc == 0 {
		log.Printf("videoenc: VideoToolbox H.264 lowdelay=%t profile=%s", !o.DisableLowLatencyRateControl, profile)
	}
	return &nativeEncoder{ptr: p, options: o, name: backend}, nil
}
func (e *nativeEncoder) Name() string { return e.name }
func (e *nativeEncoder) CanSubmit() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return false, fmt.Errorf("encoder closed")
	}
	var msg [512]C.char
	result := C.bk_encoder_ready(e.ptr, &msg[0], C.size_t(len(msg)))
	if result < 0 {
		return false, fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
	}
	return result > 0, nil
}
func (e *nativeEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
		C.bk_encoder_close(e.ptr)
		e.ptr = nil
	}
}
func nativePixelFormat(data []byte, w, h int, pixfmt string) (C.int, error) {
	if w < 1 || h < 1 || w > 8192 || h > 8192 {
		return 0, fmt.Errorf("invalid input size %dx%d", w, h)
	}
	size, tag := w*h*4, 1
	switch pixfmt {
	case "bgra":
	case "uyvy422":
		if w%2 != 0 {
			return 0, fmt.Errorf("odd packed YUV width")
		}
		size, tag = w*h*2, 2
	case "yuyv422":
		if w%2 != 0 {
			return 0, fmt.Errorf("odd packed YUV width")
		}
		size, tag = w*h*2, 3
	case "nv12":
		if w%2 != 0 || h%2 != 0 {
			return 0, fmt.Errorf("odd native NV12 size")
		}
		size, tag = w*h*3/2, 4
	default:
		return 0, fmt.Errorf("unsupported native input %q", pixfmt)
	}
	if len(data) != size {
		return 0, fmt.Errorf("invalid %s frame: got %d, want %d", pixfmt, len(data), size)
	}
	return C.int(tag), nil
}
func (e *nativeEncoder) Submit(data []byte, w, h int, pixfmt string, id uint64, force bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submit(data, w, h, pixfmt, id, force)
}
func (e *nativeEncoder) submit(data []byte, w, h int, pixfmt string, id uint64, force bool) (bool, error) {
	if e.ptr == nil {
		return false, fmt.Errorf("encoder closed")
	}
	if pixfmt == "yuv420p" || pixfmt == "nv12" && (w%2 != 0 || h%2 != 0) {
		converted, err := pixelsNV12(data, w, h, pixfmt, e.options.Width, e.options.Height)
		if err != nil {
			return false, err
		}
		data, w, h, pixfmt = converted, e.options.Width, e.options.Height, "nv12"
	}
	tag, err := nativePixelFormat(data, w, h, pixfmt)
	if err != nil {
		return false, err
	}
	var msg [512]C.char
	idr := 0
	if force {
		idr = 1
	}
	result := C.bk_encoder_submit(e.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.int(w), C.int(h), tag, C.uint64_t(id), C.int(idr), &msg[0], C.size_t(len(msg)))
	// C copied the borrowed Go input into native storage before returning.
	runtime.KeepAlive(data)
	if result < 0 {
		return false, fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
	}
	return result > 0, nil
}
func (e *nativeEncoder) SubmitPixelBuffer(pixel unsafe.Pointer, id uint64, force bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return false, fmt.Errorf("encoder closed")
	}
	var msg [512]C.char
	idr := 0
	if force {
		idr = 1
	}
	result := C.bk_encoder_submit_pixel(e.ptr, pixel, C.uint64_t(id), C.int(idr), &msg[0], C.size_t(len(msg)))
	if result < 0 {
		return false, fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
	}
	return result > 0, nil
}
func (e *nativeEncoder) Receive() (*Packet, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.receive()
}
func (e *nativeEncoder) receive() (*Packet, error) {
	if e.ptr == nil {
		return nil, fmt.Errorf("encoder closed")
	}
	var lease unsafe.Pointer
	var out *C.uint8_t
	var size C.size_t
	var id C.uint64_t
	var dropped C.int
	var msg [512]C.char
	result := C.bk_encoder_receive(e.ptr, &lease, &out, &size, &id, &dropped, &msg[0], C.size_t(len(msg)))
	if lease != nil {
		defer func() {
			if lease != nil {
				C.bk_encoder_packet_release(lease)
			}
		}()
	}
	if result < 0 {
		return nil, fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
	}
	if result == 0 {
		return nil, nil
	}
	packet := &Packet{ID: uint64(id), Dropped: dropped != 0}
	if !packet.Dropped {
		if size == 0 || size > 32*1024*1024 || out == nil {
			return nil, fmt.Errorf("%s: invalid encoded access unit size %d", e.name, size)
		}
		packet.Data = unsafe.Slice((*byte)(unsafe.Pointer(out)), int(size))
		owned := lease
		packet.Buffer = framebuf.New(packet.Data, func() { C.bk_encoder_packet_release(owned) })
		lease = nil
	}
	return packet, nil
}
func (e *nativeEncoder) Encode(nv12 []byte, force bool) ([]byte, error) {
	return e.EncodePixels(nv12, e.options.Width, e.options.Height, "nv12", force)
}

// Compatibility for probes and callers needing one synchronous picture. Media
// streaming uses Submit/Receive and never flushes between individual pictures.
func (e *nativeEncoder) EncodePixels(data []byte, w, h int, pixfmt string, force bool) ([]byte, error) {
	buffer, err := e.EncodePixelsOwned(data, w, h, pixfmt, force)
	if err != nil || buffer == nil {
		return nil, err
	}
	defer buffer.Release()
	return append([]byte(nil), buffer.Bytes()...), nil
}
func (e *nativeEncoder) EncodePixelsOwned(data []byte, w, h int, pixfmt string, force bool) (*framebuf.Buffer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncID++
	accepted, err := e.submit(data, w, h, pixfmt, e.syncID, force)
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, fmt.Errorf("synchronous encode on a busy asynchronous session")
	}
	var msg [512]C.char
	if C.bk_encoder_flush(e.ptr, &msg[0], C.size_t(len(msg))) < 0 {
		return nil, fmt.Errorf("%s: %s", e.name, C.GoString(&msg[0]))
	}
	packet, err := e.receive()
	if err != nil {
		return nil, err
	}
	defer packet.Release()
	if packet == nil || packet.Dropped || packet.ID != e.syncID {
		return nil, fmt.Errorf("%s: no matching synchronous output", e.name)
	}
	buffer := packet.Buffer
	packet.Buffer = nil
	return buffer, nil
}
