//go:build ffmpeg && cgo

package avmedia

/*
#include "native.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"unsafe"
)

type MediaSource struct {
	mu, life sync.Mutex
	ptr      *C.bm_media
	duration float64
	cancel   context.CancelFunc
	done     chan struct{}
}

func URLMediaAvailable() error {
	if err := Available(); err != nil {
		return err
	}
	if C.bm_media_available() == 0 {
		return fmt.Errorf("FFmpeg runtime lacks HTTP/HTTPS, MP4/HLS or media bitstream filters")
	}
	return nil
}

func openMedia(ctx context.Context, raw string) (*MediaSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Available(); err != nil {
		return nil, err
	}
	m := &MediaSource{ptr: C.bm_media_alloc(), done: make(chan struct{})}
	if m.ptr == nil {
		return nil, fmt.Errorf("allocate media source")
	}
	watch, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		defer close(m.done)
		<-watch.Done()
		m.life.Lock()
		if m.ptr != nil {
			C.bm_media_interrupt(m.ptr)
		}
		m.life.Unlock()
	}()
	u := C.CString(raw)
	defer C.free(unsafe.Pointer(u))
	var msg [512]C.char
	result := C.bm_media_open(m.ptr, u, &msg[0], 512)
	if result < 0 || ctx.Err() != nil {
		m.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("libav: %s", C.GoString(&msg[0]))
	}
	m.duration = float64(C.bm_media_duration(m.ptr)) / 1e6
	return m, nil
}
func (m *MediaSource) lock() bool {
	m.life.Lock()
	m.mu.Lock()
	m.life.Unlock()
	if m.ptr == nil {
		m.mu.Unlock()
		return false
	}
	return true
}
func (m *MediaSource) Close() {
	m.life.Lock()
	if m.ptr != nil {
		C.bm_media_interrupt(m.ptr)
		m.mu.Lock()
		C.bm_media_close(m.ptr)
		m.ptr = nil
		m.mu.Unlock()
	}
	m.life.Unlock()
	m.cancel()
	<-m.done
}
func (m *MediaSource) Duration() float64 { return m.duration }
func (m *MediaSource) Seek(seconds float64) error {
	if !validMediaPosition(seconds) {
		return fmt.Errorf("invalid media position")
	}
	if !m.lock() {
		return io.ErrClosedPipe
	}
	defer m.mu.Unlock()
	var msg [512]C.char
	if C.bm_media_seek(m.ptr, C.int64_t(seconds*1e6), &msg[0], 512) < 0 {
		return fmt.Errorf("libav: %s", C.GoString(&msg[0]))
	}
	return nil
}
func (m *MediaSource) Read() (*MediaPacket, error) {
	if !m.lock() {
		return nil, io.ErrClosedPipe
	}
	defer m.mu.Unlock()
	var out, config *C.uint8_t
	var size, w, h, audio, configSize, rate, channels C.int
	var pts, dts C.int64_t
	var msg [512]C.char
	result := C.bm_media_read(m.ptr, &out, &size, &w, &h, &audio, &pts, &dts, &config, &configSize, &rate, &channels, &msg[0], 512)
	defer C.free(unsafe.Pointer(config))
	buffer, err := outputOwned(result, out, size, &msg[0])
	if err != nil || buffer == nil {
		return nil, err
	}
	codec := "h264"
	if audio != 0 {
		codec = "mp4a.40.2"
	}
	if int64(pts) == math.MinInt64 {
		pts = dts
	}
	if int64(dts) == math.MinInt64 {
		dts = pts
	}
	if int64(pts) == math.MinInt64 {
		buffer.Release()
		return nil, fmt.Errorf("media packet missing timestamps")
	}
	return &MediaPacket{Frame: Frame{Buffer: buffer, Data: buffer.Bytes(), Width: int(w), Height: int(h), Codec: codec}, Audio: audio != 0, Timestamp: int64(pts), DecodeTimestamp: int64(dts), Config: C.GoBytes(unsafe.Pointer(config), configSize), SampleRate: int(rate), Channels: int(channels)}, nil
}
