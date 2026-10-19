//go:build darwin && cgo

package videodec

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework VideoToolbox -framework CoreMedia -framework CoreVideo
#include "video_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"unsafe"

	"anykvm/internal/avmedia"
)

type SystemDecoder struct {
	mu         sync.Mutex
	ptr        *C.anykvm_airplay_video
	sps, pps   []byte
	configured bool
}

func newSystemVideoDecoder() (Decoder, error) {
	p := C.anykvm_airplay_video_new()
	if p == nil {
		return nil, fmt.Errorf("video: VideoToolbox allocation failed")
	}
	return &SystemDecoder{ptr: p}, nil
}

func (d *SystemDecoder) Decode(data []byte) (*avmedia.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, io.ErrClosedPipe
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("video: video packet too large")
	}
	var packet []byte
	sps, pps := d.sps, d.pps
	for _, nal := range SplitNALs(data) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 31 {
		case 7:
			sps = nal
		case 8:
			pps = nal
		default:
			packet = binary.BigEndian.AppendUint32(packet, uint32(len(nal)))
			packet = append(packet, nal...)
		}
	}
	if len(sps) == 0 || len(pps) == 0 {
		return nil, nil
	}
	if !d.configured || !bytes.Equal(sps, d.sps) || !bytes.Equal(pps, d.pps) {
		info, err := ParseSPS(sps)
		if err != nil {
			return nil, err
		}
		if info.Width > 8192 || info.Height > 8192 {
			return nil, fmt.Errorf("video: invalid video size")
		}
		status := C.anykvm_airplay_video_configure(d.ptr, (*C.uint8_t)(unsafe.Pointer(&sps[0])), C.size_t(len(sps)), (*C.uint8_t)(unsafe.Pointer(&pps[0])), C.size_t(len(pps)))
		if status != 0 {
			d.configured = false
			return nil, fmt.Errorf("video: VideoToolbox initialization: %d", int(status))
		}
		d.sps, d.pps, d.configured = append([]byte(nil), sps...), append([]byte(nil), pps...), true
	}
	if len(packet) == 0 {
		return nil, nil
	}
	var output *C.uint8_t
	var width, height C.int
	status := C.anykvm_airplay_video_decode(d.ptr, (*C.uint8_t)(unsafe.Pointer(&packet[0])), C.size_t(len(packet)), &output, &width, &height)
	if output != nil {
		defer C.free(unsafe.Pointer(output))
	}
	if status != 0 {
		return nil, fmt.Errorf("video: VideoToolbox decode: %d", int(status))
	}
	if output == nil {
		return nil, nil
	}
	return &avmedia.Frame{Data: C.GoBytes(unsafe.Pointer(output), width*height*4), Width: int(width), Height: int(height)}, nil
}

func (d *SystemDecoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		C.anykvm_airplay_video_close(d.ptr)
		d.ptr = nil
	}
}
