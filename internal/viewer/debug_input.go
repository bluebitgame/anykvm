//go:build viewer

package viewer

import "log"

// debugInput wraps an InputSender and logs each HID event to stderr.
type debugInput struct {
	inner InputSender
}

// WrapDebugInput returns inner wrapped with stderr logging, or nil if inner is nil.
func WrapDebugInput(inner InputSender) InputSender {
	if inner == nil {
		return nil
	}
	return &debugInput{inner: inner}
}

func (d *debugInput) KeyDown(code uint8) error {
	log.Printf("input-debug: key down  0x%02x", code)
	return d.inner.KeyDown(code)
}

func (d *debugInput) KeyUp() error {
	log.Printf("input-debug: key up (clear all)")
	return d.inner.KeyUp()
}

func (d *debugInput) KeyRelease(code uint8) error {
	log.Printf("input-debug: key release 0x%02x", code)
	return d.inner.KeyRelease(code)
}

func (d *debugInput) MouseMove(x, y int) error {
	log.Printf("input-debug: mouse abs (%d, %d)", x, y)
	return d.inner.MouseMove(x, y)
}

func (d *debugInput) MouseMoveRel(dx, dy int) error {
	d.logMouseRel("", dx, dy)
	return d.inner.MouseMoveRel(dx, dy)
}

func (d *debugInput) logMouseRel(source string, dx, dy int) {
	if source != "" {
		log.Printf("input-debug: mouse rel %s (%d, %d)", source, dx, dy)
		return
	}
	log.Printf("input-debug: mouse rel (%d, %d)", dx, dy)
}

func (d *debugInput) MouseButton(button int, down bool) error {
	state := "up"
	if down {
		state = "down"
	}
	log.Printf("input-debug: mouse btn %d %s", button, state)
	return d.inner.MouseButton(button, down)
}

func (d *debugInput) MouseWheel(delta int) error {
	log.Printf("input-debug: mouse wheel %d", delta)
	return d.inner.MouseWheel(delta)
}
