//go:build !windows && !darwin && !linux

package capture

import "fmt"

func buildFFmpegAudioArgs(device string) []string {
	// Generic fallback (used by "other" platforms). darwin/linux use native.
	args := []string{"ffmpeg"}
	if device != "" {
		args = append(args, "-i", device)
	}
	args = append(args,
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", AudioSampleRate),
		"-ac", fmt.Sprintf("%d", AudioChannels),
		"-",
	)
	return args
}
