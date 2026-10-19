//go:build linux

package capture

import (
	"fmt"
)

// buildFFmpegArgs returns ffmpeg arguments (excluding the binary name) for
// Linux V4L2 capture devices. width/height/fps <= 0 mean "device default".
func buildFFmpegArgs(device string, width, height, fps int) []string {
	args := []string{"-f", "v4l2"}
	args = append(args, framerateArgs(fps)...)
	args = append(args, videoSizeArgs(width, height)...)
	args = append(args, "-i", device, "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// buildScreenFFmpegArgs captures the local X11 screen via ffmpeg's x11grab.
// display is the X display number (e.g. 0 for :0.0). width/height are the
// requested video_size and double as the downscale target; <= 0 means native.
// fps <= 0 means the device default frame rate.
func buildScreenFFmpegArgs(display, width, height, fps int, hideCursor bool) []string {
	cursor := "1"
	if hideCursor {
		cursor = "0"
	}
	args := []string{"-f", "x11grab", "-draw_mouse", cursor}
	args = append(args, framerateArgs(fps)...)
	args = append(args, "-i", fmt.Sprintf(":%d.0", display))
	args = append(args, scaleFilterArgs(width, height)...)
	args = append(args, "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// resolveScreenCaptureIndex on Linux passes the X display number through
// unchanged (x11grab addresses screens by display number, not device index).
func resolveScreenCaptureIndex(displayIndex int) int {
	return displayIndex
}
