//go:build windows && cgo

package hid

/*
#cgo LDFLAGS: -luser32
#include <windows.h>
static int anykvm_input_key(unsigned short scan, int up) {
 INPUT input = {0}; input.type = INPUT_KEYBOARD;
 if(scan == 0x200) {
  input.ki.wVk = VK_PAUSE; input.ki.dwFlags = up ? KEYEVENTF_KEYUP : 0;
  return SendInput(1, &input, sizeof(input)) == 1;
 }
 input.ki.wScan = scan & 0xff;
 input.ki.dwFlags = KEYEVENTF_SCANCODE | (scan & 0x100 ? KEYEVENTF_EXTENDEDKEY : 0) | (up ? KEYEVENTF_KEYUP : 0);
 return SendInput(1, &input, sizeof(input)) == 1;
}
static int anykvm_input_text(unsigned short unit) {
 INPUT input[2] = {0};
 input[0].type = input[1].type = INPUT_KEYBOARD;
 input[0].ki.wScan = input[1].ki.wScan = unit;
 input[0].ki.dwFlags = KEYEVENTF_UNICODE;
 input[1].ki.dwFlags = KEYEVENTF_UNICODE | KEYEVENTF_KEYUP;
 return SendInput(2, input, sizeof(INPUT)) == 2;
}
static int anykvm_input_mouse(int x, int y, DWORD flags, int data) {
 INPUT input = {0}; input.type = INPUT_MOUSE;
 input.mi.dx = x; input.mi.dy = y; input.mi.dwFlags = flags; input.mi.mouseData = data;
 return SendInput(1, &input, sizeof(input)) == 1;
}
*/
import "C"
import (
	"fmt"
	"sync"
	"unicode/utf16"
)

type localWindowsHID struct {
	mu      sync.Mutex
	keys    map[uint8]bool
	buttons map[int]bool
	closed  bool
}

func openLocal() (hidi, error) {
	return &localWindowsHID{keys: make(map[uint8]bool), buttons: make(map[int]bool)}, nil
}
func inputResult(ok C.int) error {
	if ok == 0 {
		return fmt.Errorf("Windows SendInput failed (check target process integrity level)")
	}
	return nil
}
func (d *localWindowsHID) key(code uint8, up bool) error {
	scan := windowsScanCode(code)
	if scan == 0 {
		return fmt.Errorf("unsupported HID key 0x%02x", code)
	}
	release := 0
	if up {
		release = 1
	}
	if err := inputResult(C.anykvm_input_key(C.ushort(scan), C.int(release))); err != nil {
		return err
	}
	if up {
		delete(d.keys, code)
	} else {
		d.keys[code] = true
	}
	return nil
}
func (d *localWindowsHID) KeyDown(code uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("Windows input closed")
	}
	return d.key(code, false)
}
func (d *localWindowsHID) KeyRelease(code uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("Windows input closed")
	}
	return d.key(code, true)
}
func (d *localWindowsHID) SendText(text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return fmt.Errorf("Windows input closed")
	}
	for _, unit := range utf16.Encode([]rune(text)) {
		if err := inputResult(C.anykvm_input_text(C.ushort(unit))); err != nil {
			return err
		}
	}
	return nil
}
func (d *localWindowsHID) KeyUp() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for code := range d.keys {
		if err := d.key(code, true); err != nil {
			return err
		}
	}
	return nil
}
func (d *localWindowsHID) mouse(x, y int, flags uint32, data int) error {
	if d.closed {
		return fmt.Errorf("Windows input closed")
	}
	return inputResult(C.anykvm_input_mouse(C.int(x), C.int(y), C.DWORD(flags), C.int(data)))
}
func (d *localWindowsHID) MouseMove(x, y int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mouse(min(max(x, 0), 32767)*65535/32767, min(max(y, 0), 32767)*65535/32767, 0x8000|0x4000|1, 0)
}
func (d *localWindowsHID) MouseMoveRel(x, y int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mouse(x, y, 1, 0)
}

var windowsButtonFlags = map[int][2]uint32{1: {2, 4}, 2: {8, 16}, 3: {32, 64}}

func (d *localWindowsHID) button(button int, down bool) error {
	flags, ok := windowsButtonFlags[button]
	if !ok {
		return fmt.Errorf("unsupported mouse button %d", button)
	}
	index := 1
	if down {
		index = 0
	}
	if err := d.mouse(0, 0, flags[index], 0); err != nil {
		return err
	}
	if down {
		d.buttons[button] = true
	} else {
		delete(d.buttons, button)
	}
	return nil
}
func (d *localWindowsHID) MouseButton(button int, down bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.button(button, down)
}
func (d *localWindowsHID) MouseWheel(delta int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mouse(0, 0, 0x800, delta*120)
}
func (d *localWindowsHID) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	var first error
	for code := range d.keys {
		if err := d.key(code, true); first == nil {
			first = err
		}
	}
	for button := range d.buttons {
		if err := d.button(button, false); first == nil {
			first = err
		}
	}
	d.closed = true
	return first
}
