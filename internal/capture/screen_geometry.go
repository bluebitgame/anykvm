package capture

import "image"

// screenContentRect mirrors ScreenCaptureKit's configured destinationRect,
// including its even pixel alignment. A stretched capture fills the canvas.
func screenContentRect(w, h, sw, sh int, preserveAspect bool) image.Rectangle {
	if !preserveAspect || sw < 1 || sh < 1 {
		return image.Rect(0, 0, w, h)
	}
	scale := min(float64(w)/float64(sw), float64(h)/float64(sh))
	cw, ch := int(float64(sw)*scale)&^1, int(float64(sh)*scale)&^1
	if cw < 2 || ch < 2 {
		return image.Rect(0, 0, w, h)
	}
	x, y := (w-cw)/2, (h-ch)/2
	return image.Rect(x, y, x+cw, y+ch)
}
