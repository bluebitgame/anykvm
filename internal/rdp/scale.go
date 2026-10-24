package rdp

import (
	"image"
	"math"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func (c *client) requestedOutputSize(w, h int) (int, int) {
	if size := c.outputSize.Load(); size != 0 {
		return int(size >> 32), int(uint32(size))
	}
	return w, h
}

// Fixed capture sources within the Target ceiling retain their encoded dimensions. Reset the RDPGFX
// canvas to that size instead of decoding a native AU just to rescale it.
// Keep the client's requested layout separately for local-screen targets.
func (c *client) videoOutputSize(f bridge.Frame) (int, int) {
	maxW, maxH := f.OutputMaxW&^1, f.OutputMaxH&^1
	if c.scaledMapping {
		return sourceEncodingSize(f.Width, f.Height, maxW, maxH)
	}
	withinLimit := (maxW <= 0 || f.Width <= maxW) && (maxH <= 0 || f.Height <= maxH)
	if f.PreserveSource && predictionCoded(f.Format) && withinLimit {
		return f.Width, f.Height
	}
	if f.CaptureScalesOutput && f.SourceWidth > 0 && f.SourceHeight > 0 {
		tw, th := c.requestedOutputSize(f.Width, f.Height)
		return capture.LimitedOutputSize(f.SourceWidth, f.SourceHeight, tw, th, maxW, maxH)
	}
	tw, th := c.requestedOutputSize(f.Width, f.Height)
	return capture.LimitedOutputSize(f.Width, f.Height, tw, th, maxW, maxH)
}

func (c *client) bitmapOutputSize(f bridge.Frame, w, h int) (int, int) {
	if c.scaledMapping {
		return sourceEncodingSize(w, h, f.OutputMaxW, f.OutputMaxH)
	}
	tw, th := c.requestedOutputSize(w, h)
	return capture.LimitedOutputSize(w, h, tw, th, f.OutputMaxW, f.OutputMaxH)
}

func (c *client) setOutputSize(w, h int) {
	size := uint64(w)<<32 | uint64(uint32(h))
	previous := c.outputSize.Swap(size)
	if previous == 0 || previous == size {
		return
	}
	c.requestGraphicsRefresh()
}

func (c *client) requestGraphicsRefresh() {
	c.reset.Store(true)
	// A fixed-size source may be idle. Reuse its last independent picture so a
	// refresh or client layout change does not wait for fresh source damage.
	c.framePushMu.Lock()
	defer c.framePushMu.Unlock()
	if c.lastSource != nil && !c.compressedSource {
		select {
		case <-c.done:
			return
		default:
		}
		f := c.lastSource.Retain()
		select {
		case c.frames <- f:
		default:
			f.Release()
		}
	}
}

// One fit drives both pixel scaling and pointer coordinates. AVC420 requires
// even chroma dimensions and offsets; bitmap output supports odd dimensions.
func fittedFrameRect(sw, sh, w, h int, even bool) image.Rectangle {
	if sw == w && sh == h {
		return image.Rect(0, 0, w, h)
	}
	dw, dh := w, h
	if sw*dh > sh*dw {
		dh = max(1, sh*dw/sw)
	} else {
		dw = max(1, sw*dh/sh)
	}
	if even {
		dw, dh = max(2, dw&^1), max(2, dh&^1)
	}
	x, y := (w-dw)/2, (h-dh)/2
	if even {
		x, y = x&^1, y&^1
	}
	return image.Rect(x, y, x+dw, y+dh)
}

func transformedContentRect(rect image.Rectangle, sw, sh int, fit image.Rectangle) image.Rectangle {
	return image.Rect(fit.Min.X+rect.Min.X*fit.Dx()/sw, fit.Min.Y+rect.Min.Y*fit.Dy()/sh,
		fit.Min.X+rect.Max.X*fit.Dx()/sw, fit.Min.Y+rect.Max.Y*fit.Dy()/sh)
}

func frameContentRect(f bridge.Frame) image.Rectangle {
	if !f.ContentRect.Empty() {
		return f.ContentRect
	}
	if f.SourceWidth > 0 && f.SourceHeight > 0 {
		return fittedFrameRect(f.SourceWidth, f.SourceHeight, f.Width, f.Height, false)
	}
	return image.Rect(0, 0, f.Width, f.Height)
}

type scaleSample struct{ lo, hi, weight int }

func scaleAxis(source, target, stride int) []scaleSample {
	axis := make([]scaleSample, target)
	for x := range axis {
		position := max(0, min(float64(source-1), (float64(x)+0.5)*float64(source)/float64(target)-0.5))
		lo := int(position)
		axis[x] = scaleSample{lo * stride, min(lo+1, source-1) * stride, int(math.Round((position - float64(lo)) * 256))}
	}
	return axis
}

// Fallback for capture devices without output scaling. Bilinear interpolation
// keeps desktop text smooth; captures already at the target size bypass it.
func scaleBitmap(pixels []byte, f bridge.Frame, w, h int) ([]byte, bridge.Frame) {
	if f.Width == w && f.Height == h {
		return pixels, f
	}
	fit := fittedFrameRect(f.Width, f.Height, w, h, false)
	xaxis, yaxis := scaleAxis(f.Width, fit.Dx(), 4), scaleAxis(f.Height, fit.Dy(), f.Width*4)
	out := make([]byte, w*h*4)
	for i := 3; i < len(out); i += 4 {
		out[i] = 255
	}
	for y, sy := range yaxis {
		row := out[((fit.Min.Y+y)*w+fit.Min.X)*4:]
		for x, sx := range xaxis {
			for channel := 0; channel < 4; channel++ {
				top := int(pixels[sy.lo+sx.lo+channel])*(256-sx.weight) + int(pixels[sy.lo+sx.hi+channel])*sx.weight
				bottom := int(pixels[sy.hi+sx.lo+channel])*(256-sx.weight) + int(pixels[sy.hi+sx.hi+channel])*sx.weight
				row[x*4+channel] = byte((top*(256-sy.weight) + bottom*sy.weight + 32768) >> 16)
			}
		}
	}
	f.ContentRect = transformedContentRect(frameContentRect(f), f.Width, f.Height, fit)
	f.Width, f.Height = w, h
	return out, f
}

// Client scaling removes layout-driven resampling, while Target ceilings still
// limit encoded dimensions. Keep the source aspect ratio when a ceiling applies.
func sourceEncodingSize(w, h, maxW, maxH int) (int, int) {
	bw, bh := w, h
	if maxW > 0 {
		bw = min(bw, maxW)
	}
	if maxH > 0 {
		bh = min(bh, maxH)
	}
	fit := fittedFrameRect(w, h, bw, bh, false)
	return fit.Dx(), fit.Dy()
}
