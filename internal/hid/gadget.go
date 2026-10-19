//go:build linux

package hid

import (
	"fmt"
	"os"
	"time"
)

// gadgetHID uses Linux USB gadget HID via /dev/hidg*.
type gadgetHID struct {
	kbd *os.File
	ms  *os.File
}

func openGadget() (hidi, error) {
	kbd, err := os.OpenFile("/dev/hidg0", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("hid gadget: /dev/hidg0: %w", err)
	}
	ms, err := os.OpenFile("/dev/hidg1", os.O_RDWR, 0)
	if err != nil {
		kbd.Close()
		return nil, fmt.Errorf("hid gadget: /dev/hidg1: %w", err)
	}

	g := &gadgetHID{kbd: kbd, ms: ms}
	g.KeyUp()
	time.Sleep(100 * time.Millisecond)
	return g, nil
}

func (g *gadgetHID) Close() error {
	g.kbd.Close()
	g.ms.Close()
	return nil
}

func (g *gadgetHID) KeyDown(keycode uint8) error {
	report := []byte{0, 0, keycode, 0, 0, 0, 0, 0}
	_, err := g.kbd.Write(report)
	return err
}

func (g *gadgetHID) KeyUp() error {
	_, err := g.kbd.Write(make([]byte, 8))
	return err
}

func (g *gadgetHID) KeyRelease(keycode uint8) error {
	// For gadget mode, we can't release a single key easily.
	// This would need tracking pressed keys. Fall back to KeyUp.
	return g.KeyUp()
}

func (g *gadgetHID) MouseMove(dx, dy int) error {
	report := []byte{0, byte(int8(dx)), byte(int8(dy)), 0}
	_, err := g.ms.Write(report)
	return err
}

// MouseMoveRel is identical to MouseMove for the gadget backend: Linux USB
// gadget HID mouse is natively relative.
func (g *gadgetHID) MouseMoveRel(dx, dy int) error {
	return g.MouseMove(dx, dy)
}

func (g *gadgetHID) MouseButton(button int, down bool) error {
	var btn byte
	switch button {
	case 1:
		btn = 1
	case 2:
		btn = 2
	case 3:
		btn = 4
	}
	if down {
		_, err := g.ms.Write([]byte{btn, 0, 0, 0})
		return err
	}
	_, err := g.ms.Write([]byte{0, 0, 0, 0})
	return err
}

func (g *gadgetHID) MouseWheel(delta int) error {
	_, err := g.ms.Write([]byte{0, 0, 0, byte(int8(delta))})
	return err
}
