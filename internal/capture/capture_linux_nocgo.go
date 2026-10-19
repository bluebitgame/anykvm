//go:build linux && !cgo

package capture

// Without native V4L2/ALSA bindings, use the external FFmpeg backends.
const nativeIsFFmpeg = true

func newNativeDevice(deviceID string) Device             { return NewFFmpegDevice(deviceID) }
func newPlatformAudioDevice(deviceID string) AudioDevice { return NewFFmpegAudioDevice(deviceID) }
