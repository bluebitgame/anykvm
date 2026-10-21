package capture

// Screen describes one local display addressable by screen or screen:N.
// Dimensions are physical backing pixels. Index follows the active-display
// ordering used by the capture backend.
type Screen struct {
	Index         int
	Width, Height int
}

// ScreenSize reports current physical pixels without opening capture.
// Zero dimensions mean that display discovery is unavailable.
func ScreenSize(deviceID string) (int, int) {
	if !isScreenDevice(deviceID) {
		return 0, 0
	}
	index := screenIndex(deviceID)
	for _, screen := range ListScreens() {
		if screen.Index == index {
			return screen.Width, screen.Height
		}
	}
	return 0, 0
}
