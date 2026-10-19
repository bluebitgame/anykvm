//go:build darwin

package capture

import (
	"anykvm/internal/avmedia"
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// buildFFmpegArgs returns ffmpeg arguments (excluding the binary name) for
// macOS AVFoundation capture devices. width/height/fps <= 0 mean "device
// default".
//
// The device string is the AVFoundation video device index or name; ffmpeg
// expects it as the input specifier "<video>:<audio>". We select video only
// (audio disabled via -an) and let ffmpeg transcode to MJPEG on stdout.
func buildFFmpegArgs(device string, width, height, fps int) []string {
	args := []string{"-f", "avfoundation"}
	args = append(args, framerateArgs(fps)...)
	args = append(args, videoSizeArgs(width, height)...)
	args = append(args, "-i", device+":none", "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// buildScreenFFmpegArgs returns ffmpeg arguments (excluding the binary name)
// for capturing the local screen on macOS via AVFoundation.
//
// On macOS the screen appears as an AVFoundation video device (shown as
// "Capture screen N" by `ffmpeg -f avfoundation -list_devices true -i ""`).
// avfIndex is the AVFoundation device index for that screen; it is resolved at
// runtime by resolveScreenCaptureIndex.
//
// width/height are the requested video_size; because avfoundation screen
// devices ignore -video_size (they always capture at native, often Retina,
// resolution), we downscale to width x height with a scale filter. <= 0 means
// capture at native resolution.
//
// fps <= 0 means the device default frame rate. When fps > 0 it is requested as
// the avfoundation input -framerate so the screen is actually captured at that
// rate (an output -r can only cap/duplicate, not raise the real capture rate).
// If the display does not support the requested rate, avfoundation fails to
// open; the ffmpeg device retries without -framerate (see OpenWithFormat), so
// an unsupported rate degrades to the device default rather than breaking.
func buildScreenFFmpegArgs(avfIndex, width, height, fps int, hideCursor bool) []string {
	cursor := "1"
	if hideCursor {
		cursor = "0"
	}
	args := []string{"-f", "avfoundation", "-capture_cursor", cursor}
	args = append(args, framerateArgs(fps)...)
	args = append(args, "-i", strconv.Itoa(avfIndex)+":none")
	args = append(args, scaleFilterArgs(width, height)...)
	args = append(args, "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// screenLineRe matches AVFoundation device-list lines like:
//
//	[AVFoundation indev @ 0x...] [1] Capture screen 0
var screenLineRe = regexp.MustCompile(`\[(\d+)\]\s+Capture screen\s+(\d+)`)

// resolveScreenCaptureIndex maps a display index (0-based) to the AVFoundation
// video-device index that ffmpeg expects. It enumerates devices via
// `ffmpeg -f avfoundation -list_devices true -i ""` and finds the entry whose
// "Capture screen N" number matches displayIndex. If resolution fails, it
// falls back to returning displayIndex unchanged.
func resolveScreenCaptureIndex(displayIndex int) int {
	_, ok := resolveFFmpeg()
	if !ok {
		return displayIndex
	}
	out := strings.Join(avmedia.DeviceLog("avfoundation"), "\n")

	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		m := screenLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		avfIdx, _ := strconv.Atoi(m[1])
		screenNo, _ := strconv.Atoi(m[2])
		if screenNo == displayIndex {
			return avfIdx
		}
	}
	return displayIndex
}
