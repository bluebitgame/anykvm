package hid

// VNC keysym to USB HID keycode mapping.
// X11 keysyms → USB HID Usage ID (Keyboard/Keypad Page, HUT 1.12)

var keysymToHID = map[uint32]uint8{
	// Letters
	'a': 0x04, 'b': 0x05, 'c': 0x06, 'd': 0x07, 'e': 0x08,
	'f': 0x09, 'g': 0x0A, 'h': 0x0B, 'i': 0x0C, 'j': 0x0D,
	'k': 0x0E, 'l': 0x0F, 'm': 0x10, 'n': 0x11, 'o': 0x12,
	'p': 0x13, 'q': 0x14, 'r': 0x15, 's': 0x16, 't': 0x17,
	'u': 0x18, 'v': 0x19, 'w': 0x1A, 'x': 0x1B, 'y': 0x1C,
	'z': 0x1D,

	// Digits
	'1': 0x1E, '2': 0x1F, '3': 0x20, '4': 0x21, '5': 0x22,
	'6': 0x23, '7': 0x24, '8': 0x25, '9': 0x26, '0': 0x27,

	// Function keys
	0xFFBE: 0x3A, // F1
	0xFFBF: 0x3B, // F2
	0xFFC0: 0x3C, // F3
	0xFFC1: 0x3D, // F4
	0xFFC2: 0x3E, // F5
	0xFFC3: 0x3F, // F6
	0xFFC4: 0x40, // F7
	0xFFC5: 0x41, // F8
	0xFFC6: 0x42, // F9
	0xFFC7: 0x43, // F10
	0xFFC8: 0x44, // F11
	0xFFC9: 0x45, // F12

	// Navigation
	0xFF08: 0x2A, // BackSpace
	0xFF09: 0x2B, // Tab
	0xFF0D: 0x28, // Return/Enter
	0xFF1B: 0x29, // Escape
	0xFF50: 0x4A, // Home
	0xFF55: 0x4B, // Page Up
	0xFF56: 0x4E, // Page Down
	0xFF57: 0x4D, // End
	0xFF51: 0x50, // Left
	0xFF53: 0x4F, // Right
	0xFF52: 0x52, // Up
	0xFF54: 0x51, // Down
	0xFF63: 0x49, // Insert
	0xFFFF: 0x4C, // Delete
	0xFFE3: 0xE0, // Control_L
	0xFFE1: 0xE1, // Shift_L
	0xFFE9: 0xE2, // Alt_L
	0xFFEB: 0xE3, // Super_L (GUI)
	0xFFE4: 0xE4, // Control_R
	0xFFE2: 0xE5, // Shift_R
	0xFFEA: 0xE6, // Alt_R
	0xFFEC: 0xE7, // Super_R (GUI)

	// Special
	0x0020: 0x2C, // Space
	0x002D: 0x2D, // Minus
	0x003D: 0x2E, // Equals
	0x005B: 0x2F, // Left bracket
	0x005D: 0x30, // Right bracket
	0x005C: 0x31, // Backslash
	0x003B: 0x33, // Semicolon
	0x0027: 0x34, // Quote
	0x0060: 0x35, // Grave accent
	0x002C: 0x36, // Comma
	0x002E: 0x37, // Period
	0x002F: 0x38, // Slash

	// Keypad digits: HID keypad 1-9 are 0x59-0x61, keypad 0 is 0x62.
	0xFFB0: 0x62, // KP_0
	0xFFB1: 0x59, // KP_1
	0xFFB2: 0x5A, // KP_2
	0xFFB3: 0x5B, // KP_3
	0xFFB4: 0x5C, // KP_4
	0xFFB5: 0x5D, // KP_5
	0xFFB6: 0x5E, // KP_6
	0xFFB7: 0x5F, // KP_7
	0xFFB8: 0x60, // KP_8
	0xFFB9: 0x61, // KP_9

	// Keypad operators
	0xFFAF: 0x54, // KP_Divide
	0xFFAA: 0x55, // KP_Multiply
	0xFFAD: 0x56, // KP_Subtract
	0xFFAB: 0x57, // KP_Add
	0xFF8D: 0x58, // KP_Enter
	0xFFAE: 0x63, // KP_Decimal
	0xFFBD: 0x67, // KP_Equal

	// Alternate modifier names (some VNC clients use these)
	0xFE03: 0xE2, // ISO_Level3_Shift / AltGr → same as Alt
	0xFF7E: 0xE6, // Mode_switch (macOS right Option) → Alt_R
	0xFFE7: 0xE3, // Meta_L → GUI
	0xFFE8: 0xE7, // Meta_R → GUI

	// CapsLock
	0xFFE5: 0x39, // Caps_Lock
}

// shiftedKeysym maps ASCII keysyms that require Shift to the base HID keycode
// of the unshifted key on a US layout. VNC clients typically send these symbol
// keysyms directly (e.g. '!' rather than Shift+'1'), so the server must inject
// the Shift modifier itself.
var shiftedKeysym = map[uint32]uint8{
	'!': 0x1E, // 1
	'@': 0x1F, // 2
	'#': 0x20, // 3
	'$': 0x21, // 4
	'%': 0x22, // 5
	'^': 0x23, // 6
	'&': 0x24, // 7
	'*': 0x25, // 8
	'(': 0x26, // 9
	')': 0x27, // 0
	'_': 0x2D, // -
	'+': 0x2E, // =
	'{': 0x2F, // [
	'}': 0x30, // ]
	'|': 0x31, // \
	':': 0x33, // ;
	'"': 0x34, // '
	'~': 0x35, // `
	'<': 0x36, // ,
	'>': 0x37, // .
	'?': 0x38, // /
}

// KeysymToHID converts a VNC keysym to a USB HID keycode. needsShift is true
// when the character requires the Shift modifier to be held on a US layout.
func KeysymToHID(keysym uint32) (code uint8, needsShift bool, ok bool) {
	if k, ok := keysymToHID[keysym]; ok {
		return k, false, true
	}
	if k, ok := shiftedKeysym[keysym]; ok {
		return k, true, true
	}
	// Uppercase ASCII → lowercase keycode + shift
	if keysym >= 'A' && keysym <= 'Z' {
		return uint8(keysym - 'A' + 0x04), true, true
	}
	// Lowercase ASCII → keycode
	if keysym >= 'a' && keysym <= 'z' {
		return uint8(keysym - 'a' + 0x04), false, true
	}
	// ASCII digits 1-9
	if keysym >= '1' && keysym <= '9' {
		return uint8(keysym - '1' + 0x1E), false, true
	}
	// ASCII digit 0
	if keysym == '0' {
		return 0x27, false, true
	}
	return 0, false, false
}
