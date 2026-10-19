package wsmedia

// codeToHID maps a DOM KeyboardEvent.code (physical key) to a USB HID Usage ID
// (Keyboard/Keypad page, 0x07). This is the natural mapping for a HID-based KVM:
// the physical key is sent as-is and the remote host applies its own keyboard
// layout. Values are from the USB HID Usage Tables (a public standard); the
// table is authored here and shared with no third-party code.
var codeToHID = map[string]uint8{
	// Letters
	"KeyA": 0x04, "KeyB": 0x05, "KeyC": 0x06, "KeyD": 0x07, "KeyE": 0x08,
	"KeyF": 0x09, "KeyG": 0x0A, "KeyH": 0x0B, "KeyI": 0x0C, "KeyJ": 0x0D,
	"KeyK": 0x0E, "KeyL": 0x0F, "KeyM": 0x10, "KeyN": 0x11, "KeyO": 0x12,
	"KeyP": 0x13, "KeyQ": 0x14, "KeyR": 0x15, "KeyS": 0x16, "KeyT": 0x17,
	"KeyU": 0x18, "KeyV": 0x19, "KeyW": 0x1A, "KeyX": 0x1B, "KeyY": 0x1C,
	"KeyZ": 0x1D,

	// Number row
	"Digit1": 0x1E, "Digit2": 0x1F, "Digit3": 0x20, "Digit4": 0x21,
	"Digit5": 0x22, "Digit6": 0x23, "Digit7": 0x24, "Digit8": 0x25,
	"Digit9": 0x26, "Digit0": 0x27,

	// Whitespace / control
	"Enter": 0x28, "Escape": 0x29, "Backspace": 0x2A, "Tab": 0x2B, "Space": 0x2C,

	// Punctuation
	"Minus": 0x2D, "Equal": 0x2E, "BracketLeft": 0x2F, "BracketRight": 0x30,
	"Backslash": 0x31, "Semicolon": 0x33, "Quote": 0x34, "Backquote": 0x35,
	"Comma": 0x36, "Period": 0x37, "Slash": 0x38, "IntlBackslash": 0x64,
	"IntlRo": 0x87, "IntlYen": 0x89,

	// Locks / system
	"CapsLock": 0x39, "PrintScreen": 0x46, "ScrollLock": 0x47, "Pause": 0x48,
	"NumLock": 0x53, "ContextMenu": 0x65,

	// Function keys
	"F1": 0x3A, "F2": 0x3B, "F3": 0x3C, "F4": 0x3D, "F5": 0x3E, "F6": 0x3F,
	"F7": 0x40, "F8": 0x41, "F9": 0x42, "F10": 0x43, "F11": 0x44, "F12": 0x45,
	"F13": 0x68, "F14": 0x69, "F15": 0x6A, "F16": 0x6B, "F17": 0x6C, "F18": 0x6D,
	"F19": 0x6E, "F20": 0x6F, "F21": 0x70, "F22": 0x71, "F23": 0x72, "F24": 0x73,

	// Navigation cluster
	"Insert": 0x49, "Home": 0x4A, "PageUp": 0x4B, "Delete": 0x4C, "End": 0x4D,
	"PageDown": 0x4E, "ArrowRight": 0x4F, "ArrowLeft": 0x50, "ArrowDown": 0x51,
	"ArrowUp": 0x52,

	// Keypad
	"NumpadDivide": 0x54, "NumpadMultiply": 0x55, "NumpadSubtract": 0x56,
	"NumpadAdd": 0x57, "NumpadEnter": 0x58, "Numpad1": 0x59, "Numpad2": 0x5A,
	"Numpad3": 0x5B, "Numpad4": 0x5C, "Numpad5": 0x5D, "Numpad6": 0x5E,
	"Numpad7": 0x5F, "Numpad8": 0x60, "Numpad9": 0x61, "Numpad0": 0x62,
	"NumpadDecimal": 0x63, "NumpadEqual": 0x67, "NumpadComma": 0x85,

	// Modifiers
	"ControlLeft": 0xE0, "ShiftLeft": 0xE1, "AltLeft": 0xE2, "MetaLeft": 0xE3,
	"ControlRight": 0xE4, "ShiftRight": 0xE5, "AltRight": 0xE6, "MetaRight": 0xE7,
}

// hidForCode returns the USB HID usage for a DOM key code, or (0,false) if the
// key is not mapped.
func hidForCode(code string) (uint8, bool) {
	u, ok := codeToHID[code]
	return u, ok
}
