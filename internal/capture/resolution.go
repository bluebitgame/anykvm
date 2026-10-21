package capture

import (
	"fmt"
	"math"
)

// ScaledResolution uses backing pixels, rounding down to even encoder dimensions.
func ScaledResolution(w, h int, scale float64) (int, int, error) {
	sw, sh := math.Floor(float64(w)*scale/2)*2, math.Floor(float64(h)*scale/2)*2
	if w <= 0 || h <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) || sw < 2 || sh < 2 || sw > 8192 || sh > 8192 {
		return 0, 0, fmt.Errorf("screen scale %g at %dx%d must produce dimensions between 2 and 8192", scale, w, h)
	}
	return int(sw), int(sh), nil
}

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
