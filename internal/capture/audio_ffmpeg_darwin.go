//go:build darwin

package capture

import (
	"anykvm/internal/avmedia"
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// buildFFmpegAudioArgs returns ffmpeg arguments for capturing PCM audio via
// AVFoundation's audio device on macOS. An empty device uses the default
// microphone; a loopback driver (e.g. "BlackHole 2ch") can be selected by
// passing its name or index.
func buildFFmpegAudioArgs(device string) []string {
	idx := resolveAudioDeviceIndex(device)
	ffmpegPath, ok := resolveFFmpeg()
	if !ok {
		// Unexpected, but returns something so the caller gets a run error.
		ffmpegPath = "ffmpeg"
	}
	args := []string{
		ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation",
		"-i", fmt.Sprintf("none:%d", idx),
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", AudioSampleRate),
		"-ac", fmt.Sprintf("%d", AudioChannels),
		"-",
	}
	return args
}

// resolveAudioDeviceIndex maps a device name (substring match) or numeric index
// to the AVFoundation audio device index that ffmpeg expects. Falls back to the
// default audio device (index 0) when no match is found.
func resolveAudioDeviceIndex(device string) int {
	if device == "" {
		return 0
	}
	_, ok := resolveFFmpeg()
	if !ok {
		return 0
	}
	out := strings.Join(avmedia.DeviceLog("avfoundation"), "\n")

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "AVFoundation audio devices:") {
			continue
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" || strings.Contains(line, "AVFoundation video devices:") {
				break
			}
			// Lines look like: "[AVFoundation indev @ ...] [0] Built-in Microphone"
			if idx, name := parseDeviceLine(line); idx >= 0 {
				if strings.Contains(strings.ToLower(name), strings.ToLower(device)) {
					return idx
				}
			}
		}
		break
	}
	return 0
}

// parseDeviceLine extracts "[index] Name" from an ffmpeg AVFoundation device-line.
func parseDeviceLine(line string) (int, string) {
	// "[AVFoundation indev @ ...] [0] Built-in Microphone"
	start := strings.Index(line, "] [")
	if start < 0 {
		return -1, ""
	}
	rest := line[start+3:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return -1, ""
	}
	idx, err := strconv.Atoi(rest[:end])
	if err != nil {
		return -1, ""
	}
	name := strings.TrimSpace(rest[end+1:])
	return idx, name
}
