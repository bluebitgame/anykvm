package scrcpy

import (
	"fmt"

	"anykvm/internal/capture"
)

// SetCaptureMode enables an independent display only for explicitly configured
// dimensions. OpenWithFormat and GameStream/client sizes never opt into it.
func (c *Capture) SetCaptureMode(value string) error {
	mode, err := capture.ParseMode(value)
	if err != nil {
		return err
	}
	if mode.PixelFormat != "" {
		return fmt.Errorf("capture format selection is unavailable for scrcpy")
	}
	c.sess.mu.Lock()
	defer c.sess.mu.Unlock()
	if c.sess.running {
		return fmt.Errorf("scrcpy: close capture before changing video.mode")
	}
	c.sess.displayWidth, c.sess.displayHeight = mode.Width, mode.Height
	return nil
}
