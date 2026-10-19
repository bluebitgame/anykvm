//go:build windows

package capture

// buildFFmpegArgs returns ffmpeg arguments (excluding the binary name) for
// Windows DirectShow capture devices. width/height/fps <= 0 mean "device
// default".
func buildFFmpegArgs(device string, width, height, fps int) []string {
	args := []string{"-f", "dshow"}
	args = append(args, framerateArgs(fps)...)
	args = append(args, videoSizeArgs(width, height)...)
	args = append(args, "-i", "video="+device, "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// buildScreenFFmpegArgs captures the local desktop via ffmpeg's gdigrab.
// display is currently ignored (gdigrab grabs the whole virtual desktop).
// width/height are the requested video_size and double as the downscale
// target; <= 0 means native. fps <= 0 means the device default frame rate.
func buildScreenFFmpegArgs(display, width, height, fps int, hideCursor bool) []string {
	cursor := "1"
	if hideCursor {
		cursor = "0"
	}
	args := []string{"-f", "gdigrab", "-draw_mouse", cursor}
	args = append(args, framerateArgs(fps)...)
	args = append(args, "-i", "desktop")
	args = append(args, scaleFilterArgs(width, height)...)
	args = append(args, "-f", "mjpeg", "-q:v", "5")
	args = append(args, outputRateArgs(fps)...)
	args = append(args, "-an", "-")
	return args
}

// resolveScreenCaptureIndex on Windows passes the index through unchanged
// (gdigrab grabs the whole desktop; index is not used).
func resolveScreenCaptureIndex(displayIndex int) int {
	return displayIndex
}
