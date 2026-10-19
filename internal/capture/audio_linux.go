//go:build linux && cgo

package capture

// #cgo LDFLAGS: -lasound
//
// #include <stdlib.h>
// #include <errno.h>
// #include <alsa/asoundlib.h>
//
// // alsa_open opens a capture PCM device in S16LE interleaved mode.
// static snd_pcm_t* alsa_open(const char *dev, unsigned int rate, unsigned int channels) {
//     snd_pcm_t *pcm = NULL;
//     if (snd_pcm_open(&pcm, dev, SND_PCM_STREAM_CAPTURE, SND_PCM_NONBLOCK) < 0) {
//         return NULL;
//     }
//     if (snd_pcm_set_params(pcm,
//                            SND_PCM_FORMAT_S16_LE,
//                            SND_PCM_ACCESS_RW_INTERLEAVED,
//                            channels,
//                            rate,
//                            1,          // allow resampling
//                            40000) < 0 // bounded 40ms capture latency
//         ) {
//         snd_pcm_close(pcm);
//         return NULL;
//     }
//     return pcm;
// }
//
// // alsa_read reads up to `frames` frames; returns frames read or negative on error.
// static long alsa_read(snd_pcm_t *pcm, unsigned char *buf, unsigned long frames) {
//     snd_pcm_sframes_t n = snd_pcm_readi(pcm, buf, frames);
//     if (n == -EAGAIN) return 0;
//     if (n < 0) {
//         n = snd_pcm_recover(pcm, n, 1);
//         if (n < 0) return n;
//         return 0;
//     }
//     return n;
// }
//
// static void alsa_close(snd_pcm_t *pcm) {
//     if (pcm) {
//         snd_pcm_drop(pcm);
//         snd_pcm_close(pcm);
//     }
// }
import "C"
import (
	"fmt"
	"sync"
	"unsafe"
)

type alsaAudioDevice struct {
	mu  sync.Mutex
	dev string
	pcm *C.snd_pcm_t
}

func newPlatformAudioDevice(deviceID string) AudioDevice {
	if deviceID == "" {
		deviceID = "default"
	}
	return &alsaAudioDevice{dev: deviceID}
}

func (d *alsaAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pcm != nil {
		return fmt.Errorf("audio: ALSA already open")
	}
	cdev := C.CString(d.dev)
	defer C.free(unsafe.Pointer(cdev))
	pcm := C.alsa_open(cdev, C.uint(AudioSampleRate), C.uint(AudioChannels))
	if pcm == nil {
		return fmt.Errorf("audio: alsa cannot open %s", d.dev)
	}
	d.pcm = pcm
	return nil
}

func (d *alsaAudioDevice) Capture() (*AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pcm == nil {
		return nil, fmt.Errorf("audio: alsa not open")
	}
	const frameBytes = AudioChannels * (AudioBitsPerSample / 8)
	// 20ms worth of frames.
	const frames = AudioSampleRate / 50
	buf := make([]byte, frames*frameBytes)
	n := C.alsa_read(d.pcm, (*C.uchar)(unsafe.Pointer(&buf[0])), C.ulong(frames))
	if n < 0 {
		return nil, fmt.Errorf("audio: alsa read error %d", int(n))
	}
	if n == 0 {
		return nil, nil
	}
	got := int(n) * frameBytes
	return &AudioFrame{PCM: buf[:got], SampleRate: AudioSampleRate, Channels: AudioChannels}, nil
}

func (d *alsaAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pcm != nil {
		C.alsa_close(d.pcm)
		d.pcm = nil
	}
	return nil
}
