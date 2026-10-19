//go:build darwin

package capture

// #cgo LDFLAGS: -framework AVFoundation -framework CoreMedia -framework CoreAudio -framework Foundation
//
// #include <stdlib.h>
//
// extern void* avfoundation_audio_start(const char *deviceName, int sampleRate, int channels);
// extern void  avfoundation_audio_stop(void* session);
// extern int   audio_buf_read(unsigned char *out, int max);
// extern void  audio_buf_destroy(void);
// extern void  avfoundation_list_audio_devices(char *buf, int maxLen);
import "C"
import (
	"fmt"
	"strings"
	"time"
	"unsafe"
)

type darwinAudioDevice struct {
	id      string
	session unsafe.Pointer
}

func newPlatformAudioDevice(deviceID string) AudioDevice {
	return &fallbackAudioDevice{
		backends: []AudioDevice{
			&darwinAudioDevice{id: deviceID}, // native AVFoundation (mic / loopback)
			NewFFmpegAudioDevice(deviceID),   // ffmpeg avfoundation audio (wider device support)
		},
	}
}

func (d *darwinAudioDevice) Open() error {
	name := C.CString(d.id)
	defer C.free(unsafe.Pointer(name))
	session := C.avfoundation_audio_start(name, C.int(AudioSampleRate), C.int(AudioChannels))
	if session == nil {
		return fmt.Errorf("audio: failed to start AVFoundation audio session")
	}
	d.session = session
	return nil
}

func (d *darwinAudioDevice) Capture() (*AudioFrame, error) {
	// 20ms chunk of S16LE stereo.
	const chunk = AudioSampleRate * AudioChannels * (AudioBitsPerSample / 8) / 50
	buf := make([]byte, chunk)
	n := int(C.audio_buf_read((*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	if n <= 0 {
		time.Sleep(5 * time.Millisecond)
		return nil, nil
	}
	return &AudioFrame{PCM: buf[:n], SampleRate: AudioSampleRate, Channels: AudioChannels}, nil
}

func (d *darwinAudioDevice) Close() error {
	if d.session != nil {
		C.avfoundation_audio_stop(d.session)
		d.session = nil
	}
	C.audio_buf_destroy()
	return nil
}

// ListNativeAudioDevices returns the names of available AVFoundation audio
// input devices on macOS (no ffmpeg dependency). Each entry is the localized
// name; the slice index is the device index.
func ListNativeAudioDevices() []string {
	buf := make([]C.char, 65536)
	C.avfoundation_list_audio_devices(&buf[0], 65536)
	raw := C.GoString(&buf[0])
	if raw == "" {
		return nil
	}
	var devices []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if idx := strings.IndexByte(line, '\t'); idx >= 0 {
			devices = append(devices, line[idx+1:])
		}
	}
	return devices
}
