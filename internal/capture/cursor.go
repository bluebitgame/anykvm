package capture

// CursorState with no image and zero dimensions explicitly hides the protocol cursor.
// A nil state leaves fallback behavior to the consumer.
type CursorState struct {
	SystemDefault bool   // request the client system cursor instead of a captured image
	Image         []byte // BGRA pixel data
	Width         int
	Height        int
	HotX          int  // hotspot X relative to image
	HotY          int  // hotspot Y relative to image
	Changed       bool // set by Capture: true when state differs from previous capture
	Premultiplied bool // BGRA color channels are multiplied by alpha
	// Native display pixels corresponding to this cursor's raster scale.
	// Zero for cursors without local display geometry.
	SourceWidth, SourceHeight int
}

// NativeCursorCapture supplies cursor images in backing pixels for protocols
// that scale them to the captured desktop. Capture returns logical-size images.
type NativeCursorCapture interface {
	CaptureNative() (*CursorState, error)
}

type CursorCapture interface {
	Open() error
	Capture() (*CursorState, error)
	Close() error
}

// External video devices carry any target cursor in their video signal.
func (s *smartDevice) UsesLocalCursor() bool { return false }
