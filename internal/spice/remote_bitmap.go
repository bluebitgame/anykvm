package spice

import (
	"bytes"
	"fmt"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

type bitmapFrame struct {
	pixels        []byte
	width, height int
}

// captureBitmap runs under Device.mu. Native polling coalesces damage until the
// next paced read and copies pixels on the GLib thread into an owned snapshot.
func (d *Device) captureBitmap() (*capture.Frame, error) {
	now := time.Now()
	if !d.force && now.Sub(d.lastFrame) < time.Second/time.Duration(d.fps) {
		return nil, nil
	}
	lease, err := d.session.next()
	if err != nil {
		return nil, fmt.Errorf("%w: SPICE remote: %v", capture.ErrStreamStopped, err)
	}
	var bitmap *bitmapFrame
	switch {
	case lease != nil:
		defer lease.done()
		if lease.reset {
			d.resetBitmap(now)
			return nil, nil
		}
		bitmap = lease.bitmap
		if !bitmap.valid() {
			return nil, fmt.Errorf("%w: invalid SPICE remote framebuffer", capture.ErrStreamStopped)
		}
		d.lastBitmap = bitmap
		d.stats.BitmapFrames++
	case d.force && d.lastBitmap != nil:
		bitmap = d.lastBitmap
		d.stats.RepeatedFrames++
	default:
		return nil, d.checkBitmapTimeout(now)
	}
	d.width, d.height = bitmap.width, bitmap.height
	d.force = false
	d.lastFrame = now
	return bitmap.output(now), nil
}

// resetBitmap removes pixels and input geometry belonging to the old primary.
// A replacement primary gets a fresh timeout, independent of session age.
func (d *Device) resetBitmap(now time.Time) {
	d.lastBitmap = nil
	d.width, d.height = 0, 0
	d.bitmapWaitAt = now
	d.force = true
}

func (d *Device) checkBitmapTimeout(now time.Time) error {
	timeout := d.opts.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if d.lastBitmap == nil && now.Sub(d.bitmapWaitAt) > timeout {
		return fmt.Errorf("%w: SPICE did not provide a desktop within %s", capture.ErrStreamStopped, timeout)
	}
	return nil
}

func (b *bitmapFrame) valid() bool {
	return b != nil && b.width >= 1 && b.height >= 1 && b.width <= 8192 && b.height <= 8192 &&
		len(b.pixels) == b.width*b.height*4
}

func (b *bitmapFrame) output(now time.Time) *capture.Frame {
	// Keep the idle snapshot isolated even from downstream pixel processors.
	buffer := framebuf.New(bytes.Clone(b.pixels), nil)
	return &capture.Frame{
		Format: capture.FormatRaw, Buffer: buffer, Data: buffer.Bytes(),
		Width: b.width, Height: b.height,
		SourceWidth: b.width, SourceHeight: b.height, CapturedAt: now,
	}
}
