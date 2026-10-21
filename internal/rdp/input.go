package rdp

import (
	"errors"
	"image"
	"math"
	"sync"
	"unicode/utf16"

	"anykvm/internal/bridge"
	"anykvm/internal/hid"
	"anykvm/internal/logging"
)

type inputState struct {
	mu                     sync.Mutex
	hid                    bridge.HIDSender
	keys                   [256]bool
	buttons                [4]bool
	w, h, sourceW, sourceH int
	contentRect            image.Rectangle
	wheel                  int
	highSurrogate          uint16
	macShortcuts           bool // per-session policy, guarded by mu
	swapLeftModifiers      bool // non-Mac clients on a macOS server, guarded by mu
}

func (i *inputState) configureShortcuts(osMajor uint16, enabled bool) (mac, swap bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.macShortcuts = enabled && macShortcutClient(osMajor)
	i.swapLeftModifiers = enabled && swapLeftModifiersClient(osMajor)
	return i.macShortcuts, i.swapLeftModifiers
}

func (i *inputState) setMacShortcuts(enabled bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.macShortcuts = enabled
}

func (i *inputState) activate(hid bridge.HIDSender) { i.mu.Lock(); i.hid = hid; i.mu.Unlock() }
func (i *inputState) geometry(w, h, sw, sh int) {
	i.frameGeometry(bridge.Frame{Width: w, Height: h, SourceWidth: sw, SourceHeight: sh}, w, h)
}

func (i *inputState) frameGeometry(f bridge.Frame, w, h int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	oldW, oldH, oldSourceW, oldSourceH, oldRect := i.w, i.h, i.sourceW, i.sourceH, i.contentRect
	i.w, i.h = w, h
	sw, sh := f.SourceWidth, f.SourceHeight
	if sw < 1 || sh < 1 {
		sw, sh = f.Width, f.Height
		if sw < 1 || sh < 1 {
			sw, sh = w, h
		}
	}
	i.sourceW, i.sourceH = sw, sh
	i.contentRect = f.ContentRect
	if !i.contentRect.Empty() && f.Width > 0 && f.Height > 0 {
		// AVC420 conversion fits and centers the source with even chroma
		// dimensions/offsets. Transform content bounds through that same fit.
		i.contentRect = transformedContentRect(i.contentRect, f.Width, f.Height, fittedFrameRect(f.Width, f.Height, w, h, true))
		if !i.contentRect.In(image.Rect(0, 0, w, h)) || i.contentRect.Dx() < 2 || i.contentRect.Dy() < 2 {
			i.contentRect = image.Rectangle{}
		}
	}
	if oldW != i.w || oldH != i.h || oldSourceW != sw || oldSourceH != sh || oldRect != i.contentRect {
		logging.Infof("rdp: pointer geometry desktop=%dx%d source=%dx%d content=%v", w, h, sw, sh, i.contentRect)
	}
}
func (i *inputState) release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.hid != nil {
		for code, held := range i.keys {
			if held {
				_ = i.hid.KeyRelease(uint8(code))
			}
		}
		for b, held := range i.buttons {
			if held {
				_ = i.hid.MouseButton(b, false)
			}
		}
	}
	i.hid = nil
	i.keys = [256]bool{}
	i.buttons = [4]bool{}
	i.highSurrogate = 0
}

// Map set-1 RDP scancodes to USB HID usages used by every AnyKVM backend.
func hidKey(scan uint16, extended bool) uint8 {
	if extended {
		return map[uint16]uint8{0x1c: 0x58, 0x1d: 0xe4, 0x35: 0x54, 0x37: 0x46, 0x38: 0xe6, 0x47: 0x4a, 0x48: 0x52, 0x49: 0x4b, 0x4b: 0x50, 0x4d: 0x4f, 0x4f: 0x4d, 0x50: 0x51, 0x51: 0x4e, 0x52: 0x49, 0x53: 0x4c, 0x5b: 0xe3, 0x5c: 0xe7, 0x5d: 0x65}[scan]
	}
	keys := [...]uint8{
		0, 0x29, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x2d, 0x2e, 0x2a, 0x2b,
		0x14, 0x1a, 0x08, 0x15, 0x17, 0x1c, 0x18, 0x0c, 0x12, 0x13, 0x2f, 0x30, 0x28, 0xe0, 0x04, 0x16,
		0x07, 0x09, 0x0a, 0x0b, 0x0d, 0x0e, 0x0f, 0x33, 0x34, 0x35, 0xe1, 0x31, 0x1d, 0x1b, 0x06, 0x19,
		0x05, 0x11, 0x10, 0x36, 0x37, 0x38, 0xe5, 0x55, 0xe2, 0x2c, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e,
		0x3f, 0x40, 0x41, 0x42, 0x43, 0x53, 0x47, 0x5f, 0x60, 0x61, 0x56, 0x5c, 0x5d, 0x5e, 0x57, 0x59,
		0x5a, 0x5b, 0x62, 0x63, 0, 0, 0x64, 0x44, 0x45,
	}
	if int(scan) >= len(keys) {
		return 0
	}
	return keys[scan]
}

func (i *inputState) key(scan, flags uint16) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	code := i.shortcutKey(hidKey(scan, flags&0x100 != 0))
	down := flags&0x8000 == 0
	if i.hid == nil || code == 0 {
		return nil
	}
	if i.keys[code] == down {
		// RDP repeats ordinary key-downs without intervening releases. HID
		// report backends need a release/press transition for every repeat,
		// including Backspace during client-side IME replacement. Modifier
		// and lock-key repeats must not toggle state or retrigger shortcuts.
		if down && code < 0xe0 && code != 0x39 && code != 0x47 && code != 0x53 {
			return i.tapKey(code, false)
		}
		return nil
	}
	if down {
		i.keys[code] = true
	}
	return i.withMacShortcut(code, func() error {
		if down {
			return i.hid.KeyDown(code)
		}
		err := i.hid.KeyRelease(code)
		if err == nil {
			i.keys[code] = false
		}
		return err // Keep a failed release tracked for disconnect cleanup.
	})
}

// Unicode events carry committed text, so emit a complete key stroke on down
// and ignore its matching release. Surrogate pairs can span input packets.
func (i *inputState) unicodeKey(unit uint16, release bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.hid == nil || release {
		return nil
	}
	r := rune(unit)
	high := i.highSurrogate
	i.highSurrogate = 0
	switch {
	case unit >= 0xd800 && unit <= 0xdbff:
		i.highSurrogate = unit
		return nil
	case unit >= 0xdc00 && unit <= 0xdfff:
		if high == 0 {
			return nil
		}
		r = utf16.DecodeRune(rune(high), r)
	}
	r = i.shortcutRune(r)
	// These control characters must act as keys rather than inserted text.
	var code uint8
	switch r {
	case '\b':
		code = 0x2a
	case '\t':
		code = 0x2b
	case '\r', '\n':
		code = 0x28
	case 0x1b:
		code = 0x29
	case 0x7f:
		code = 0x4c
	}
	if code != 0 {
		return i.tapKey(code, false)
	}
	if r < 0x20 {
		return nil
	}
	// Preserve ASCII shortcut semantics when Ctrl, Alt, or GUI is held.
	// Non-ASCII Unicode is committed text, even when an IME switch left a
	// modifier held; it has no US-layout shortcut fallback.
	shortcut := i.keys[0xe0] || i.keys[0xe2] || i.keys[0xe3] || i.keys[0xe4] || i.keys[0xe6] || i.keys[0xe7]
	if sender, ok := i.hid.(hid.TextSender); ok && (!shortcut || r > 0x7e) {
		if err := sender.SendText(string(r)); !errors.Is(err, hid.ErrTextUnsupported) {
			return err
		}
	}
	if r > 0x7e {
		return nil
	}
	code, shift, ok := hid.KeysymToHID(uint32(r))
	if !ok {
		return nil // Physical HID devices can only approximate text on a US layout.
	}
	return i.tapKey(code, shift)
}

// tapKey preserves keys held by scancode input, including physical Shift.
// Temporary keys remain tracked on error so disconnect cleanup releases them.
// The caller holds i.mu.
func (i *inputState) tapKey(code uint8, shift bool) error {
	return i.withMacShortcut(code, func() error { return i.tapHIDKey(code, shift) })
}

func (i *inputState) tapHIDKey(code uint8, shift bool) error {
	temporaryShift := shift && !i.keys[0xe1] && !i.keys[0xe5]
	if temporaryShift {
		i.keys[0xe1] = true
		if err := i.hid.KeyDown(0xe1); err != nil {
			return err
		}
	}
	held := i.keys[code]
	if held {
		if err := i.hid.KeyRelease(code); err != nil {
			return err
		}
	}
	i.keys[code] = true
	if err := i.hid.KeyDown(code); err != nil {
		return err
	}
	if !held {
		if err := i.hid.KeyRelease(code); err != nil {
			return err
		}
		i.keys[code] = false
	}
	if temporaryShift {
		if err := i.hid.KeyRelease(0xe1); err != nil {
			return err
		}
		i.keys[0xe1] = false
	}
	return nil
}

func (i *inputState) mouse(flags, x, y uint16, extended bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.hid == nil || i.w < 2 || i.h < 2 {
		return nil
	}
	if extended {
		return nil
	} // Additional buttons have no common HID backend mapping.
	// Button events carry a position even without PTRFLAGS_MOVE. Wheel
	// events have no usable coordinates (MS-RDPBCGR 2.2.8.1.1.3.1.1.3).
	if flags&0x7800 != 0 && flags&0x600 == 0 {
		px, py := float64(x)/float64(i.w-1), float64(y)/float64(i.h-1)
		if !i.contentRect.Empty() {
			px = float64(int(x)-i.contentRect.Min.X) / float64(i.contentRect.Dx()-1)
			py = float64(int(y)-i.contentRect.Min.Y) / float64(i.contentRect.Dy()-1)
		} else {
			src, output := float64(i.sourceW)/float64(i.sourceH), float64(i.w)/float64(i.h)
			if src > output {
				ratio := output / src
				py = (py - (1-ratio)/2) / ratio
			} else {
				ratio := src / output
				px = (px - (1-ratio)/2) / ratio
			}
		}
		if err := i.hid.MouseMove(int(math.Round(max(0, min(1, px))*32767)), int(math.Round(max(0, min(1, py))*32767))); err != nil {
			return err
		}
	}
	if flags&0x200 != 0 {
		delta := int(flags & 0x1ff)
		if delta&0x100 != 0 {
			delta -= 0x200
		}
		// RDP wheel rotation is in 120-unit detents.
		i.wheel += delta
		steps := i.wheel / 120
		i.wheel %= 120
		if steps != 0 {
			return i.hid.MouseWheel(steps)
		}
	}
	for b, flag := range []uint16{0, 0x1000, 0x2000, 0x4000} {
		if b == 0 || flags&flag == 0 {
			continue
		}
		down := flags&0x8000 != 0
		if i.buttons[b] != down {
			i.buttons[b] = down
			if err := i.hid.MouseButton(b, down); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *inputState) slow(p []byte) error {
	if len(p) < 4 {
		return errProtocol
	}
	count := int(le.Uint16(p))
	p = p[4:]
	if count > 256 || len(p) != count*12 {
		return errProtocol
	}
	for n := 0; n < count; n++ {
		e := p[n*12 : (n+1)*12]
		kind := le.Uint16(e[4:])
		flags := le.Uint16(e[6:])
		switch kind {
		case 4:
			if err := i.key(le.Uint16(e[8:]), flags); err != nil {
				return err
			}
		case 0x8001, 0x8002:
			if err := i.mouse(flags, le.Uint16(e[8:]), le.Uint16(e[10:]), kind == 0x8002); err != nil {
				return err
			}
		case 5:
			if err := i.unicodeKey(le.Uint16(e[8:]), flags&0x8000 != 0); err != nil {
				return err
			}
		case 0, 2: // Synchronize and unused events.
		default:
			return errProtocol
		}
	}
	return nil
}

func (i *inputState) fast(p []byte) error {
	if len(p) < 1 || p[0]&0xc3 != 0 {
		return errProtocol
	}
	count := int((p[0] >> 2) & 15)
	p = p[1:]
	if count == 0 {
		if len(p) == 0 {
			return errProtocol
		}
		count = int(p[0])
		p = p[1:]
	}
	for n := 0; n < count; n++ {
		if len(p) == 0 {
			return errProtocol
		}
		header := p[0]
		p = p[1:]
		switch header >> 5 {
		case 0:
			if len(p) < 1 {
				return errProtocol
			}
			flags := uint16(0)
			if header&1 != 0 {
				flags |= 0x8000
			}
			if header&2 != 0 {
				flags |= 0x100
			}
			if err := i.key(uint16(p[0]), flags); err != nil {
				return err
			}
			p = p[1:]
		case 1, 2:
			if len(p) < 6 {
				return errProtocol
			}
			if err := i.mouse(le.Uint16(p), le.Uint16(p[2:]), le.Uint16(p[4:]), header>>5 == 2); err != nil {
				return err
			}
			p = p[6:]
		case 3:
		case 4:
			if len(p) < 2 {
				return errProtocol
			}
			if err := i.unicodeKey(le.Uint16(p), header&1 != 0); err != nil {
				return err
			}
			p = p[2:]
		default:
			return errProtocol
		}
	}
	if len(p) != 0 {
		return errProtocol
	}
	return nil
}
