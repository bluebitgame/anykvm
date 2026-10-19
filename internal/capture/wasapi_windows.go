//go:build windows && cgo

package capture

/*
#cgo windows LDFLAGS: -lole32 -luuid -lavrt
#include "wasapi_windows.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"unsafe"
)

type wasapiAudioDevice struct {
	mu       sync.Mutex
	selector string
	ptr      *C.bk_wasapi
	scratch  []byte
}

func newWASAPIAudioDevice(selector string) AudioDevice { return &wasapiAudioDevice{selector: selector} }
func (d *wasapiAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		return fmt.Errorf("WASAPI loopback already open")
	}
	name := C.CString(d.selector)
	defer C.free(unsafe.Pointer(name))
	var msg [512]C.char
	d.ptr = C.bk_wasapi_open(name, &msg[0], C.size_t(len(msg)))
	if d.ptr == nil {
		return fmt.Errorf("WASAPI loopback: %s", C.GoString(&msg[0]))
	}
	d.scratch = make([]byte, AudioSampleRate/50*AudioChannels*AudioBitsPerSample/8)
	nameLabel := d.selector
	if nameLabel == "" {
		nameLabel = "default multimedia playback device"
	}
	log.Printf("capture: WASAPI loopback %q -> 48000Hz stereo S16LE", nameLabel)
	return nil
}
func (d *wasapiAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr == nil {
		return nil, ErrStreamStopped
	}
	data := d.scratch
	var epoch C.uint64_t
	size := int(C.bk_wasapi_read(d.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &epoch))
	if size == 0 {
		return nil, nil
	}
	return &AudioFrame{PCM: append([]byte(nil), data[:size]...), SampleRate: AudioSampleRate, Channels: AudioChannels, Epoch: uint64(epoch)}, nil
}
func (d *wasapiAudioDevice) AudioAvailable() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ptr != nil && C.bk_wasapi_available(d.ptr) != 0
}
func (d *wasapiAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ptr != nil {
		C.bk_wasapi_close(d.ptr)
		d.ptr = nil
	}
	return nil
}
func ListWASAPILoopbackDevices() []string {
	var msg [512]C.char
	names := C.bk_wasapi_devices(&msg[0], C.size_t(len(msg)))
	if names == nil {
		log.Printf("capture: %s", C.GoString(&msg[0]))
		return []string{"wasapi"}
	}
	defer C.free(unsafe.Pointer(names))
	result := []string{"wasapi"}
	value := strings.TrimSuffix(C.GoString(names), "\n")
	if value != "" {
		result = append(result, strings.Split(value, "\n")...)
	}
	return result
}
