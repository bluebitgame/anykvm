//go:build windows

package capture

import "fmt"

func buildFFmpegAudioArgs(device string) []string {
	if device == "" {
		device = "default"
	}
	return []string{
		"ffmpeg",
		"-f", "dshow",
		"-audio_buffer_size", "20",
		"-i", "audio=" + device,
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", AudioSampleRate),
		"-ac", fmt.Sprintf("%d", AudioChannels),
		"-",
	}
}
