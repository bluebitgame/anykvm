//go:build darwin && cgo

package capture

/*
#include <stdint.h>
extern void *capture_usb_audio_open(void *session);
extern int capture_usb_audio_read(void *tap, uint8_t *out, int capacity);
extern void capture_usb_audio_close(void *tap);
*/
import "C"
import (
	"bytes"
	"fmt"
	"sync"
	"unsafe"
)

type usbAudioDevice struct {
	mu     sync.Mutex
	video  *darwinDevice
	tap    unsafe.Pointer
	buffer [3840]byte
}

func (d *darwinDevice) AudioDevice() AudioDevice { return &usbAudioDevice{video: d} }
func (d *usbAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tap != nil {
		return nil
	}
	d.video.mu.Lock()
	defer d.video.mu.Unlock()
	if d.video.session == nil {
		return ErrStreamStopped
	}
	d.tap = C.capture_usb_audio_open(d.video.session)
	if d.tap == nil {
		return fmt.Errorf("ios-usb: no audio connection on capture session")
	}
	return nil
}
func (d *usbAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tap == nil {
		return nil, ErrStreamStopped
	}
	n := int(C.capture_usb_audio_read(d.tap, (*C.uint8_t)(unsafe.Pointer(&d.buffer[0])), C.int(len(d.buffer))))
	if n < 0 {
		return nil, ErrStreamStopped
	}
	if n == 0 {
		return nil, nil
	}
	return &AudioFrame{PCM: bytes.Clone(d.buffer[:n]), SampleRate: 48000, Channels: 2}, nil
}
func (d *usbAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tap != nil {
		C.capture_usb_audio_close(d.tap)
		d.tap = nil
	}
	return nil
}
