//go:build windows

package capture

import "strings"

func newPlatformAudioDevice(deviceID string) AudioDevice {
	if deviceID == "wasapi" {
		return newWASAPIAudioDevice("")
	}
	if strings.HasPrefix(deviceID, "wasapi:") {
		return newWASAPIAudioDevice(strings.TrimPrefix(deviceID, "wasapi:"))
	}
	return newWindowsAudioDevice(deviceID)
}
