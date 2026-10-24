//go:build linux && cgo && spice

package spice

/*
#cgo pkg-config: spice-client-glib-2.0 egl gbm libdrm
#include <stdlib.h>
#include "native_linux.h"
*/
import "C"

import (
	"fmt"
	"net"
	"unsafe"

	"anykvm/internal/capture"
	"anykvm/internal/dmabuf"
)

type glibSession struct {
	ptr    *C.bk_spice
	remote bool
}

func newDevice(raw string, o Options) capture.Device {
	d := &Device{
		raw: raw, opts: o,
		openSession: openGLib,
	}
	e, _ := parseEndpoint(raw)
	if e.remote() {
		return wrapRemote(d)
	}
	return d
}

func openGLib(address, password, node string) (nativeSession, error) {
	ticket := C.CString(password)
	defer C.free(unsafe.Pointer(ticket))
	var msg [512]C.char
	var ptr *C.bk_spice
	remote := node == ""
	if remote {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		hostname, service := C.CString(host), C.CString(port)
		defer C.free(unsafe.Pointer(hostname))
		defer C.free(unsafe.Pointer(service))
		ptr = C.bk_spice_open_remote(hostname, service, ticket, &msg[0], 512)
	} else {
		socketPath, renderNode := C.CString(address), C.CString(node)
		defer C.free(unsafe.Pointer(socketPath))
		defer C.free(unsafe.Pointer(renderNode))
		ptr = C.bk_spice_open(socketPath, ticket, renderNode, &msg[0], 512)
	}
	if ptr == nil {
		return nil, fmt.Errorf("%s", C.GoString(&msg[0]))
	}
	return &glibSession{ptr: ptr, remote: remote}, nil
}

func (s *glibSession) next() (*frameLease, error) {
	if s.remote {
		return s.nextBitmap()
	}
	var f C.bk_spice_frame
	var msg [512]C.char
	status := C.bk_spice_next(s.ptr, &f, &msg[0], 512)
	if status < 0 {
		return nil, fmt.Errorf("%s", C.GoString(&msg[0]))
	}
	if status == 0 {
		return nil, nil
	}
	source := dmabuf.Frame{
		FD:    int(f.fd),
		Width: uint32(f.width), Height: uint32(f.height),
		FourCC: uint32(f.format), Modifier: uint64(f.modifier),
		Pitch: uint32(f.pitch), Offset: uint32(f.offset), PlaneCount: uint32(f.planes),
		CropWidth: uint32(f.width), CropHeight: uint32(f.height),
		DeviceMajor: uint32(f.device_major), DeviceMinor: uint32(f.device_minor),
		FlipY: f.flip_y != 0,
	}
	for i := 0; i < 4; i++ {
		source.PlanePitches[i] = uint32(f.pitches[i])
		source.PlaneOffsets[i] = uint32(f.offsets[i])
	}
	return &frameLease{frame: source, done: func() { C.bk_spice_done(s.ptr) }}, nil
}

func (s *glibSession) input(kind, a, b, buttons int) error {
	if C.bk_spice_input(s.ptr, C.int(kind), C.int(a), C.int(b), C.int(buttons)) < 0 {
		return fmt.Errorf("SPICE input channel or requested pointer mode is unavailable (absolute input needs a guest tablet)")
	}
	return nil
}

func (s *glibSession) close() {
	C.bk_spice_close(s.ptr)
	s.ptr = nil
}

func (s *glibSession) setAudioEnabled(enabled bool) {
	value := 0
	if enabled {
		value = 1
	}
	C.bk_spice_audio_enable(s.ptr, C.int(value))
}

func (s *glibSession) nextAudio() (*audioPacket, error) {
	var packet C.bk_spice_audio
	var msg [512]C.char
	status := C.bk_spice_audio_next(s.ptr, &packet, &msg[0], 512)
	if status < 0 {
		return nil, fmt.Errorf("SPICE audio: %s", C.GoString(&msg[0]))
	}
	if status == 0 {
		return nil, nil
	}
	defer C.bk_spice_audio_release(&packet)
	if packet.size == 0 || packet.size > maxAudioPacketBytes {
		return nil, fmt.Errorf("invalid SPICE audio payload size")
	}
	return &audioPacket{
		pcm:  C.GoBytes(unsafe.Pointer(packet.data), C.int(packet.size)),
		rate: int(packet.rate), channels: int(packet.channels),
		epoch: uint64(packet.epoch), sample: uint64(packet.sample),
		volume: [2]uint16{uint16(packet.volume[0]), uint16(packet.volume[1])},
		mute:   packet.muted != 0,
	}, nil
}

func (s *glibSession) audioAvailable() bool { return C.bk_spice_audio_available(s.ptr) != 0 }

func (s *glibSession) cursor() (*cursorSnapshot, error) {
	var state C.bk_spice_cursor
	C.bk_spice_cursor_get(s.ptr, &state)
	defer C.bk_spice_cursor_release(&state)
	snapshot := &cursorSnapshot{
		width: int(state.width), height: int(state.height),
		hotX: int(state.hot_x), hotY: int(state.hot_y),
		known: state.known != 0, visible: state.visible != 0,
		systemDefault: state.system_default != 0,
		premultiplied: state.premultiplied != 0,
		serial:        uint64(state.serial),
	}
	if state.rgba != nil {
		if state.width == 0 || state.height == 0 || state.width > maxCursorDimension || state.height > maxCursorDimension {
			return nil, fmt.Errorf("invalid SPICE cursor dimensions")
		}
		snapshot.rgba = C.GoBytes(unsafe.Pointer(state.rgba), C.int(state.width*state.height*4))
	}
	return snapshot, nil
}

func (s *glibSession) nextBitmap() (*frameLease, error) {
	var bitmap C.bk_spice_bitmap
	var msg [512]C.char
	status := C.bk_spice_bitmap_next(s.ptr, &bitmap, &msg[0], 512)
	if status < 0 {
		return nil, fmt.Errorf("%s", C.GoString(&msg[0]))
	}
	if status == C.BK_SPICE_BITMAP_RESET {
		return &frameLease{reset: true, done: func() {}}, nil
	}
	if status == C.BK_SPICE_BITMAP_IDLE {
		return nil, nil
	}
	defer C.bk_spice_bitmap_release(&bitmap)
	if bitmap.width < 1 || bitmap.height < 1 || bitmap.width > 8192 || bitmap.height > 8192 ||
		bitmap.size != C.size_t(bitmap.width)*C.size_t(bitmap.height)*4 || bitmap.data == nil {
		return nil, fmt.Errorf("invalid SPICE bitmap snapshot")
	}
	return &frameLease{
		bitmap: &bitmapFrame{
			pixels: C.GoBytes(unsafe.Pointer(bitmap.data), C.int(bitmap.size)),
			width:  int(bitmap.width), height: int(bitmap.height),
		},
		done: func() {},
	}, nil
}
