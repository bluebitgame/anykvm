package capture

import "math"

// ParseResolution extracts dimensions and rounded FPS from a capture mode.
// A missing/zero component means "device default" (0).
func ParseResolution(s string) (w, h, fps int) {
	mode, err := ParseMode(s)
	if err != nil {
		return 0, 0, 0
	}
	return mode.Width, mode.Height, int(math.Round(mode.FPS))
}

// ResolutionFollowsClient distinguishes explicit auto from omitted dimensions.
func ResolutionFollowsClient(s string) bool {
	mode, err := ParseMode(s)
	return err == nil && mode.FollowClient
}

// IsScreenDevice reports whether a selector requests a local display.
func IsScreenDevice(deviceID string) bool { return isScreenDevice(deviceID) }
