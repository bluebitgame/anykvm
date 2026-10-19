//go:build linux

package capture

import "strconv"

func buildFFmpegAudioArgs(device string) []string {
	if device == "" {
		device = "default"
	}
	return []string{"ffmpeg", "-f", "alsa", "-i", device, "-f", "s16le", "-ar", strconv.Itoa(AudioSampleRate), "-ac", strconv.Itoa(AudioChannels), "-"}
}
