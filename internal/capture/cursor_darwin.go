//go:build darwin

package capture

// #cgo LDFLAGS: -framework AppKit -framework CoreGraphics -framework ApplicationServices
// #include "cursor_darwin.h"
import "C"
import (
	"runtime"
	"unsafe"
)

type cursorCapture struct {
	lastWidth  int
	lastHeight int
	lastHotX   int
	lastHotY   int
	lastPixels []byte
	avail      bool
}

func NewCursorCapture() CursorCapture { return &cursorCapture{} }

func (c *cursorCapture) Open() error {
	runtime.LockOSThread()
	c.avail = C.cursor_available() != 0
	runtime.UnlockOSThread()
	return nil
}

func (c *cursorCapture) Capture() (*CursorState, error) {
	return c.capture(false)
}

func (c *cursorCapture) CaptureNative() (*CursorState, error) {
	return c.capture(true)
}

func (c *cursorCapture) capture(native bool) (*CursorState, error) {
	if !c.avail {
		return nil, nil
	}

	runtime.LockOSThread()
	var cr *C.cursor_result_t
	if native {
		cr = C.cursor_capture_native()
	} else {
		cr = C.cursor_capture()
	}
	runtime.UnlockOSThread()

	if cr == nil {
		return nil, nil
	}
	defer C.cursor_free(cr)

	w := int(cr.width)
	h := int(cr.height)
	hotX := int(cr.hot_x)
	hotY := int(cr.hot_y)
	sz := w * h * 4
	pixels := C.GoBytes(unsafe.Pointer(cr.pixels), C.int(sz))

	changed := w != c.lastWidth || h != c.lastHeight ||
		hotX != c.lastHotX || hotY != c.lastHotY ||
		len(pixels) != len(c.lastPixels)

	if !changed && len(pixels) > 0 {
		for i := range pixels {
			if pixels[i] != c.lastPixels[i] {
				changed = true
				break
			}
		}
	}

	c.lastWidth = w
	c.lastHeight = h
	c.lastHotX = hotX
	c.lastHotY = hotY
	c.lastPixels = pixels

	return &CursorState{
		Image:         pixels,
		Width:         w,
		Height:        h,
		HotX:          hotX,
		HotY:          hotY,
		Changed:       changed,
		Premultiplied: true,
		SourceWidth:   int(cr.source_width),
		SourceHeight:  int(cr.source_height),
	}, nil
}

func (c *cursorCapture) Close() error { return nil }
