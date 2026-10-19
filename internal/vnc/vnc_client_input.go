package vnc

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// inputConnected is checked with lifecycleMu held, so Open cannot replace
// the connection while an input message is being sent.
func (c *VNCClient) inputConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running && c.conn != nil
}

// writeMessage serializes complete messages and bounds writes to stalled peers.
// Callers either hold lifecycleMu or belong to the current session's workers.
func (c *VNCClient) writeMessage(msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := c.conn.Write(msg)
	if err == nil && n != len(msg) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A partial message leaves the protocol stream unusable.
		c.conn.Close()
	}
	return err
}

func (c *VNCClient) KeyDown(keycode uint8) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	ks, ok := hidToKeysymLookup(keycode)
	if !ok {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if c.pressed == nil {
		c.pressed = make(map[uint8]uint32)
	}
	c.pressed[keycode] = ks
	return c.sendKeyEvent(ks, true)
}

func (c *VNCClient) KeyRelease(keycode uint8) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	ks, ok := hidToKeysymLookup(keycode)
	if !ok {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	delete(c.pressed, keycode)
	return c.sendKeyEvent(ks, false)
}

func (c *VNCClient) KeyUp() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	var err error
	for usage, keysym := range c.pressed {
		err = errors.Join(err, c.sendKeyEvent(keysym, false))
		delete(c.pressed, usage)
	}
	return err
}

func (c *VNCClient) sendKeyEvent(keysym uint32, down bool) error {
	buf := make([]byte, 8)
	buf[0] = msgKeyEvent
	if down {
		buf[1] = 1
	}
	binary.BigEndian.PutUint32(buf[4:8], keysym)
	return c.writeMessage(buf)
}

func (c *VNCClient) MouseMove(dx, dy int) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	c.mu.Lock()
	w, h := c.fbWidth, c.fbHeight
	c.mu.Unlock()
	if w <= 0 || h <= 0 {
		return nil
	}
	x := clampInt(dx, 0, 32767) * (w - 1) / 32767
	y := clampInt(dy, 0, 32767) * (h - 1) / 32767
	c.lastMouseX = x
	c.lastMouseY = y
	return c.sendPointerEvent(x, y, c.lastBtn)
}

func (c *VNCClient) MouseMoveRel(dx, dy int) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	c.mu.Lock()
	w, h := c.fbWidth, c.fbHeight
	c.mu.Unlock()
	if w <= 0 || h <= 0 {
		return nil
	}
	var x, y int
	if c.mouseRelative {
		cx := w / 2
		cy := h / 2
		x = clampInt(cx+dx, 0, w-1)
		y = clampInt(cy+dy, 0, h-1)
	} else {
		c.lastMouseX = clampInt(c.lastMouseX+dx, 0, w-1)
		c.lastMouseY = clampInt(c.lastMouseY+dy, 0, h-1)
		x = c.lastMouseX
		y = c.lastMouseY
	}
	return c.sendPointerEvent(x, y, c.lastBtn)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (c *VNCClient) MouseButton(button int, down bool) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if button < 1 || button > 3 {
		return nil
	}
	// HID uses left/right/middle; RFB uses left/middle/right.
	mask := [3]uint8{1, 4, 2}[button-1]
	if down {
		c.lastBtn |= mask
	} else {
		c.lastBtn &^= mask
	}
	return c.sendPointerEvent(c.lastMouseX, c.lastMouseY, c.lastBtn)
}

func (c *VNCClient) MouseWheel(delta int) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if !c.inputConnected() {
		return nil
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	var wheel uint8
	if delta > 0 {
		wheel = 0x08
	} else if delta < 0 {
		wheel = 0x10
	} else {
		return nil
	}
	if err := c.sendPointerEvent(c.lastMouseX, c.lastMouseY, c.lastBtn|wheel); err != nil {
		return err
	}
	return c.sendPointerEvent(c.lastMouseX, c.lastMouseY, c.lastBtn)
}

func (c *VNCClient) sendPointerEvent(x, y int, buttons uint8) error {
	buf := make([]byte, 6)
	buf[0] = msgPointerEvent
	buf[1] = buttons
	binary.BigEndian.PutUint16(buf[2:4], uint16(x))
	binary.BigEndian.PutUint16(buf[4:6], uint16(y))
	return c.writeMessage(buf)
}
