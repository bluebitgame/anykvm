//go:build !darwin && !linux && !windows

package capture

func newPlatformAudioDevice(deviceID string) AudioDevice {
	return NewFFmpegAudioDevice(deviceID)
}
