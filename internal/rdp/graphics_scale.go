package rdp

import (
	"fmt"
	"image"
	"log"

	"anykvm/internal/bridge"
)

const gfxCapsScaledMapDisable uint32 = 0x80

func graphicsSupportsScaledMapping(version, flags uint32) bool {
	switch version {
	case 0xa0502, 0xa0600, 0xa0601:
		return true
	case 0xa0701:
		return flags&gfxCapsScaledMapDisable == 0
	}
	return false
}

// Surface pixels and output coordinates stay separate. viewport is also used
// for input mapping, so odd dimensions and letterboxing use the same rounding.
type graphicsLayout struct {
	surfaceW, surfaceH int
	outputW, outputH   int
	viewport           image.Rectangle
}

func makeGraphicsLayout(w, h, outputW, outputH int, scaled bool) graphicsLayout {
	if !scaled {
		outputW, outputH = w, h
	}
	return graphicsLayout{w, h, outputW, outputH, fittedFrameRect(w, h, outputW, outputH, false)}
}

func graphicsCreateSurface(id, w, h int) []byte {
	return gfxPDU(9, join(u16(id), u16(w), u16(h), []byte{0x20}))
}

func graphicsMapScaledSurface(id int, rect image.Rectangle) []byte {
	return gfxPDU(0x17, join(u16(id), u16(0), u32(uint32(rect.Min.X)), u32(uint32(rect.Min.Y)), u32(uint32(rect.Dx())), u32(uint32(rect.Dy()))))
}

func graphicsBlackSurface(id int, rect image.Rectangle) []byte {
	// Each bar is a one-pixel black surface scaled into its own non-overlapping
	// output rectangle. No full-sized background bitmap needs to be encoded.
	return join(graphicsCreateSurface(id, 1, 1),
		gfxPDU(4, join(u16(id), []byte{0, 0, 0, 255}, u16(1), u16(0), u16(0), u16(1), u16(1))),
		graphicsMapScaledSurface(id, rect))
}

func (g graphicsLayout) bars() []image.Rectangle {
	if g.surfaceW == 0 {
		return nil
	}
	v := g.viewport
	var bars []image.Rectangle
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, g.outputW, v.Min.Y),
		image.Rect(0, v.Max.Y, g.outputW, g.outputH),
		image.Rect(0, v.Min.Y, v.Min.X, v.Max.Y),
		image.Rect(v.Max.X, v.Min.Y, g.outputW, v.Max.Y),
	} {
		if !r.Empty() {
			bars = append(bars, r)
		}
	}
	return bars
}

func (c *client) ensureGraphicsLayout(next graphicsLayout) error {
	old := c.graphics
	if old == next {
		return nil
	}
	if next.surfaceW < 1 || next.surfaceH < 1 || next.surfaceW > 8192 || next.surfaceH > 8192 || next.outputW < 1 || next.outputH < 1 || next.outputW > 8192 || next.outputH > 8192 {
		return fmt.Errorf("invalid graphics layout: %+v", next)
	}
	var p []byte
	// RESET_GRAPHICS retains offscreen surfaces. Delete old IDs explicitly,
	// including the bars when a new aspect ratio exposes their former pixels.
	if old.surfaceW != 0 {
		p = append(p, gfxPDU(0xa, u16(1))...)
	}
	for i := range old.bars() {
		p = append(p, gfxPDU(0xa, u16(i+2))...)
	}
	if old.outputW != next.outputW || old.outputH != next.outputH {
		p = append(p, graphicsResetOutput(next.outputW, next.outputH)...)
	}
	p = append(p, graphicsCreateSurface(1, next.surfaceW, next.surfaceH)...)
	if next.surfaceW == next.outputW && next.surfaceH == next.outputH {
		p = append(p, gfxPDU(0xf, join(u16(1), u16(0), u32(0), u32(0)))...)
	} else {
		p = append(p, graphicsMapScaledSurface(1, next.viewport)...)
	}
	for i, r := range next.bars() {
		p = append(p, graphicsBlackSurface(i+2, r)...)
	}
	if err := c.sendGFX(p); err != nil {
		return err
	}
	if old.surfaceW != 0 {
		log.Printf("rdp: %s: graphics surface=%dx%d output=%dx%d viewport=%v", c.conn.RemoteAddr(), next.surfaceW, next.surfaceH, next.outputW, next.outputH, next.viewport)
	}
	c.graphics = next
	return nil
}

func (c *client) frameGeometry(f bridge.Frame, w, h int) {
	if !c.scaledMapping {
		c.input.frameGeometry(f, w, h)
		return
	}
	// Account for encoder padding/capture letterboxing first, then apply exactly
	// the viewport sent to the client. Input and the separately sent cursor share it.
	content := transformedContentRect(frameContentRect(f), f.Width, f.Height, fittedFrameRect(f.Width, f.Height, w, h, c.codec == codecAVC420))
	g := c.graphics
	content = transformedContentRect(content, w, h, g.viewport)
	if f.SourceWidth < 1 || f.SourceHeight < 1 {
		f.SourceWidth, f.SourceHeight = f.Width, f.Height
	}
	f.Width, f.Height = g.outputW, g.outputH
	f.ContentRect = content
	c.input.frameGeometry(f, g.outputW, g.outputH)
}
