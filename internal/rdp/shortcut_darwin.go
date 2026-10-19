package rdp

import "errors"

// MS-RDPBCGR General Capability Set platform IDs. These identify the reported
// client OS, not the client vendor or application version.
func macShortcutClient(osMajor uint16) bool {
	return osMajor == 3 || osMajor == 5 || osMajor == 6 // Macintosh, iOS, OS X
}

func swapLeftModifiersClient(osMajor uint16) bool {
	// An unspecified OS does not establish that the keyboard is non-Mac.
	return osMajor != 0 && !macShortcutClient(osMajor)
}

// Map modifiers before updating keys, so repeat detection, Unicode chords and
// disconnect cleanup all track the actual HID usages sent to the host.
func (i *inputState) shortcutKey(code uint8) uint8 {
	if i.swapLeftModifiers {
		switch code {
		case 0xe2: // Left Alt at the physical Command position.
			return 0xe3
		case 0xe3: // Left Win at the physical Option position.
			return 0xe2
		}
	}
	return code
}

// Windows App remaps Command+C/X/V/A/Z/F/W/+/- to Control shortcuts.
// https://learn.microsoft.com/en-us/windows-app/input-keyboard-mouse-touch-pen
func (i *inputState) macShortcut(code uint8) bool {
	// Right Control keeps its original meaning, including when both are held.
	if !i.macShortcuts || !i.keys[0xe0] || i.keys[0xe4] || i.keys[0xe2] || i.keys[0xe6] || i.keys[0xe3] || i.keys[0xe7] {
		return false
	}
	switch code {
	case 0x06, 0x1b, 0x19, 0x04, 0x1d, 0x09, 0x1a, 0x2e, 0x2d:
		return true
	}
	return false
}

// Replace held left Control with Command only while sending a mapped shortcut.
// Restore the client's modifiers even when the backend returns an error.
// The caller holds i.mu.
func (i *inputState) withMacShortcut(code uint8, send func() error) (err error) {
	if !i.macShortcut(code) {
		return send()
	}
	if err = i.hid.KeyRelease(0xe0); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, i.hid.KeyDown(0xe0)) }()
	i.keys[0xe3] = true
	defer func() {
		releaseErr := i.hid.KeyRelease(0xe3)
		if releaseErr == nil {
			i.keys[0xe3] = false
		}
		err = errors.Join(err, releaseErr)
	}()
	if err = i.hid.KeyDown(0xe3); err != nil {
		return err
	}
	return send()
}

// Some clients encode Control+letter as uppercase Unicode or a C0 character.
// The modifier already carries the chord; avoid adding an artificial Shift.
func (i *inputState) shortcutRune(r rune) rune {
	var code uint8
	switch {
	case r >= 1 && r <= 26:
		code = uint8(r-1) + 0x04
	case r >= 'A' && r <= 'Z':
		code = uint8(r-'A') + 0x04
	default:
		return r
	}
	if i.macShortcut(code) {
		return rune(code-0x04) + 'a'
	}
	return r
}
