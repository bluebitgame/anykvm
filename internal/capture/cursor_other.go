//go:build !darwin

package capture

type cursorCapture struct{}

func NewCursorCapture() CursorCapture { return &cursorCapture{} }

func (c *cursorCapture) Open() error                    { return nil }
func (c *cursorCapture) Capture() (*CursorState, error) { return nil, nil }
func (c *cursorCapture) Close() error                   { return nil }
