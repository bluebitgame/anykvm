package capture

// Screen describes one local display addressable by screen or screen:N.
// Index follows the active-display ordering used by the capture backend.
type Screen struct {
	Index         int
	Width, Height int
}
