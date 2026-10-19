package bridge

import "anykvm/internal/capture"

// NewCursorCapture follows the active remote capture source. Remote sources
// own their cursor lifecycle; device sources can disable the local fallback.
func (b *Bridge) NewCursorCapture(local capture.CursorCapture) capture.CursorCapture {
	return &sourceCursor{bridge: b, local: local}
}

type sourceCursor struct {
	bridge *Bridge
	local  capture.CursorCapture
}

func (c *sourceCursor) Open() error {
	if c.local != nil {
		return c.local.Open()
	}
	return nil
}
func (c *sourceCursor) Close() error {
	if c.local != nil {
		return c.local.Close()
	}
	return nil
}
func (c *sourceCursor) Capture() (*capture.CursorState, error) {
	return c.capture(false)
}
func (c *sourceCursor) CaptureNative() (*capture.CursorState, error) {
	return c.capture(true)
}
func (c *sourceCursor) capture(native bool) (*capture.CursorState, error) {
	m := c.bridge.activeMachine()
	if m != nil {
		if m.MouseMode == "relative" || m.CursorRendering == "remote" {
			return &capture.CursorState{}, nil
		}
		if source, ok := m.Capture.(interface {
			CaptureCursor() (*capture.CursorState, error)
		}); ok {
			return source.CaptureCursor()
		}
		if source, ok := m.Capture.(interface{ UsesLocalCursor() bool }); ok && !source.UsesLocalCursor() {
			return &capture.CursorState{SystemDefault: m.MouseMode != "relative"}, nil
		}
	}
	if c.local != nil {
		if source, ok := c.local.(capture.NativeCursorCapture); native && ok {
			return source.CaptureNative()
		}
		return c.local.Capture()
	}
	return nil, nil
}
