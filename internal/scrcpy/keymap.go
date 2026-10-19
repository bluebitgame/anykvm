package scrcpy

// Android meta state flags (android.view.KeyEvent.META_*).
const (
	metaShiftOn      = 0x1
	metaAltOn        = 0x2
	metaAltLeftOn    = 0x10
	metaAltRightOn   = 0x20
	metaShiftLeftOn  = 0x40
	metaShiftRightOn = 0x80
	metaCtrlOn       = 0x1000
	metaCtrlLeftOn   = 0x2000
	metaCtrlRightOn  = 0x4000
	metaMetaOn       = 0x10000
	metaMetaLeftOn   = 0x20000
	metaMetaRightOn  = 0x40000
)

// hidModifierMeta maps modifier USB HID usages (0xE0-0xE7) to Android meta
// state flags.
var hidModifierMeta = map[uint8]uint32{
	0xE0: metaCtrlOn | metaCtrlLeftOn,
	0xE1: metaShiftOn | metaShiftLeftOn,
	0xE2: metaAltOn | metaAltLeftOn,
	0xE3: metaMetaOn | metaMetaLeftOn,
	0xE4: metaCtrlOn | metaCtrlRightOn,
	0xE5: metaShiftOn | metaShiftRightOn,
	0xE6: metaAltOn | metaAltRightOn,
	0xE7: metaMetaOn | metaMetaRightOn,
}

// hidToAndroidKey maps USB HID keyboard usage codes to Android keycodes
// (android.view.KeyEvent.KEYCODE_*).
var hidToAndroidKey = map[uint8]int32{
	// Letters A-Z: HID 0x04-0x1D -> KEYCODE_A(29)..KEYCODE_Z(54)
	0x04: 29, 0x05: 30, 0x06: 31, 0x07: 32, 0x08: 33, 0x09: 34,
	0x0A: 35, 0x0B: 36, 0x0C: 37, 0x0D: 38, 0x0E: 39, 0x0F: 40,
	0x10: 41, 0x11: 42, 0x12: 43, 0x13: 44, 0x14: 45, 0x15: 46,
	0x16: 47, 0x17: 48, 0x18: 49, 0x19: 50, 0x1A: 51, 0x1B: 52,
	0x1C: 53, 0x1D: 54,

	// Digits 1-9,0: HID 0x1E-0x27 -> KEYCODE_1(8)..KEYCODE_9(16), KEYCODE_0(7)
	0x1E: 8, 0x1F: 9, 0x20: 10, 0x21: 11, 0x22: 12,
	0x23: 13, 0x24: 14, 0x25: 15, 0x26: 16, 0x27: 7,

	0x28: 66,  // Enter
	0x29: 111, // Escape
	0x2A: 67,  // Backspace -> KEYCODE_DEL
	0x2B: 61,  // Tab
	0x2C: 62,  // Space
	0x2D: 69,  // - -> KEYCODE_MINUS
	0x2E: 70,  // = -> KEYCODE_EQUALS
	0x2F: 71,  // [ -> KEYCODE_LEFT_BRACKET
	0x30: 72,  // ] -> KEYCODE_RIGHT_BRACKET
	0x31: 73,  // \ -> KEYCODE_BACKSLASH
	0x32: 73,  // Non-US # -> KEYCODE_BACKSLASH
	0x33: 74,  // ; -> KEYCODE_SEMICOLON
	0x34: 75,  // ' -> KEYCODE_APOSTROPHE
	0x35: 68,  // ` -> KEYCODE_GRAVE
	0x36: 55,  // , -> KEYCODE_COMMA
	0x37: 56,  // . -> KEYCODE_PERIOD
	0x38: 76,  // / -> KEYCODE_SLASH
	0x39: 115, // CapsLock

	// F1-F12: HID 0x3A-0x45 -> KEYCODE_F1(131)..KEYCODE_F12(142)
	0x3A: 131, 0x3B: 132, 0x3C: 133, 0x3D: 134, 0x3E: 135, 0x3F: 136,
	0x40: 137, 0x41: 138, 0x42: 139, 0x43: 140, 0x44: 141, 0x45: 142,

	0x46: 120, // PrintScreen -> KEYCODE_SYSRQ
	0x47: 116, // ScrollLock
	0x48: 121, // Pause -> KEYCODE_BREAK
	0x49: 124, // Insert
	0x4A: 122, // Home -> KEYCODE_MOVE_HOME
	0x4B: 92,  // PageUp
	0x4C: 112, // Delete -> KEYCODE_FORWARD_DEL
	0x4D: 123, // End -> KEYCODE_MOVE_END
	0x4E: 93,  // PageDown
	0x4F: 22,  // Right -> KEYCODE_DPAD_RIGHT
	0x50: 21,  // Left -> KEYCODE_DPAD_LEFT
	0x51: 20,  // Down -> KEYCODE_DPAD_DOWN
	0x52: 19,  // Up -> KEYCODE_DPAD_UP

	0x53: 143, // NumLock
	0x54: 154, // KP / -> KEYCODE_NUMPAD_DIVIDE
	0x55: 155, // KP * -> KEYCODE_NUMPAD_MULTIPLY
	0x56: 156, // KP - -> KEYCODE_NUMPAD_SUBTRACT
	0x57: 157, // KP + -> KEYCODE_NUMPAD_ADD
	0x58: 160, // KP Enter -> KEYCODE_NUMPAD_ENTER
	// KP 1-9: HID 0x59-0x61 -> KEYCODE_NUMPAD_1(145)..KEYCODE_NUMPAD_9(153)
	0x59: 145, 0x5A: 146, 0x5B: 147, 0x5C: 148, 0x5D: 149,
	0x5E: 150, 0x5F: 151, 0x60: 152, 0x61: 153,
	0x62: 144, // KP 0 -> KEYCODE_NUMPAD_0
	0x63: 158, // KP . -> KEYCODE_NUMPAD_DOT

	0x65: 82, // Application -> KEYCODE_MENU

	// Modifiers: HID 0xE0-0xE7
	0xE0: 113, // LeftControl -> KEYCODE_CTRL_LEFT
	0xE1: 59,  // LeftShift -> KEYCODE_SHIFT_LEFT
	0xE2: 57,  // LeftAlt -> KEYCODE_ALT_LEFT
	0xE3: 117, // LeftGUI -> KEYCODE_META_LEFT
	0xE4: 114, // RightControl -> KEYCODE_CTRL_RIGHT
	0xE5: 60,  // RightShift -> KEYCODE_SHIFT_RIGHT
	0xE6: 58,  // RightAlt -> KEYCODE_ALT_RIGHT
	0xE7: 118, // RightGUI -> KEYCODE_META_RIGHT
}
