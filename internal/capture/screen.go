//go:build !darwin && !windows && (!linux || !cgo)

package capture

// screenDevice captures the local machine's screen via ffmpeg. It reuses the
// ffmpeg backend but builds platform-specific screen-grab arguments (see
// buildScreenFFmpegArgs in the per-OS files) instead of capture-card args.
//
// The requested capture width/height (from video_size) is also used as the
// downscale target, since screen capture backends (e.g. macOS AVFoundation)
// capture at native resolution and must be scaled down for efficient transfer.
type screenDevice struct {
	*ffmpegDevice
	index      int
	hideCursor bool
}

// SetHideCursor controls whether the cursor is included in captured pixels.
// Configure it before opening the device; false (the default) shows the cursor.
func (d *screenDevice) SetHideCursor(hide bool) { d.hideCursor = hide }

func newScreenDevice(deviceID string) Device {
	idx := screenIndex(deviceID)
	d := &screenDevice{
		ffmpegDevice: &ffmpegDevice{device: deviceID, decodeImage: true},
		index:        idx,
	}
	d.ffmpegDevice.argsFunc = func(width, height, fps int) []string {
		// Resolve the display index to a platform-specific capture index once,
		// lazily, when the arguments are first built. The requested video_size
		// (width/height) doubles as the downscale target.
		capIdx := resolveScreenCaptureIndex(idx)
		return buildScreenFFmpegArgs(capIdx, width, height, fps, d.hideCursor)
	}
	return d
}
