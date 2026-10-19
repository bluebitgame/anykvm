//go:build viewer

package viewer

import "github.com/veandco/go-sdl2/sdl"

var keyToHID = map[sdl.Keycode]uint8{
	sdl.K_a: 0x04, sdl.K_b: 0x05, sdl.K_c: 0x06, sdl.K_d: 0x07, sdl.K_e: 0x08,
	sdl.K_f: 0x09, sdl.K_g: 0x0A, sdl.K_h: 0x0B, sdl.K_i: 0x0C, sdl.K_j: 0x0D,
	sdl.K_k: 0x0E, sdl.K_l: 0x0F, sdl.K_m: 0x10, sdl.K_n: 0x11, sdl.K_o: 0x12,
	sdl.K_p: 0x13, sdl.K_q: 0x14, sdl.K_r: 0x15, sdl.K_s: 0x16, sdl.K_t: 0x17,
	sdl.K_u: 0x18, sdl.K_v: 0x19, sdl.K_w: 0x1A, sdl.K_x: 0x1B, sdl.K_y: 0x1C,
	sdl.K_z: 0x1D,

	sdl.K_1: 0x1E, sdl.K_2: 0x1F, sdl.K_3: 0x20, sdl.K_4: 0x21,
	sdl.K_5: 0x22, sdl.K_6: 0x23, sdl.K_7: 0x24, sdl.K_8: 0x25,
	sdl.K_9: 0x26, sdl.K_0: 0x27,

	sdl.K_RETURN: 0x28, sdl.K_ESCAPE: 0x29, sdl.K_BACKSPACE: 0x2A,
	sdl.K_TAB: 0x2B, sdl.K_SPACE: 0x2C,

	sdl.K_MINUS: 0x2D, sdl.K_EQUALS: 0x2E, sdl.K_LEFTBRACKET: 0x2F,
	sdl.K_RIGHTBRACKET: 0x30, sdl.K_BACKSLASH: 0x31, sdl.K_SEMICOLON: 0x33,
	sdl.K_QUOTE: 0x34, sdl.K_BACKQUOTE: 0x35, sdl.K_COMMA: 0x36,
	sdl.K_PERIOD: 0x37, sdl.K_SLASH: 0x38,

	sdl.K_CAPSLOCK: 0x39, sdl.K_PRINTSCREEN: 0x46, sdl.K_SCROLLLOCK: 0x47,
	sdl.K_PAUSE: 0x48, sdl.K_NUMLOCKCLEAR: 0x53, sdl.K_APPLICATION: 0x65,

	sdl.K_F1: 0x3A, sdl.K_F2: 0x3B, sdl.K_F3: 0x3C, sdl.K_F4: 0x3D,
	sdl.K_F5: 0x3E, sdl.K_F6: 0x3F, sdl.K_F7: 0x40, sdl.K_F8: 0x41,
	sdl.K_F9: 0x42, sdl.K_F10: 0x43, sdl.K_F11: 0x44, sdl.K_F12: 0x45,
	sdl.K_F13: 0x68, sdl.K_F14: 0x69, sdl.K_F15: 0x6A, sdl.K_F16: 0x6B,
	sdl.K_F17: 0x6C, sdl.K_F18: 0x6D, sdl.K_F19: 0x6E, sdl.K_F20: 0x6F,
	sdl.K_F21: 0x70, sdl.K_F22: 0x71, sdl.K_F23: 0x72, sdl.K_F24: 0x73,

	sdl.K_INSERT: 0x49, sdl.K_HOME: 0x4A, sdl.K_PAGEUP: 0x4B,
	sdl.K_DELETE: 0x4C, sdl.K_END: 0x4D, sdl.K_PAGEDOWN: 0x4E,
	sdl.K_RIGHT: 0x4F, sdl.K_LEFT: 0x50, sdl.K_DOWN: 0x51, sdl.K_UP: 0x52,

	sdl.K_KP_DIVIDE: 0x54, sdl.K_KP_MULTIPLY: 0x55, sdl.K_KP_MINUS: 0x56,
	sdl.K_KP_PLUS: 0x57, sdl.K_KP_ENTER: 0x58, sdl.K_KP_1: 0x59, sdl.K_KP_2: 0x5A,
	sdl.K_KP_3: 0x5B, sdl.K_KP_4: 0x5C, sdl.K_KP_5: 0x5D, sdl.K_KP_6: 0x5E,
	sdl.K_KP_7: 0x5F, sdl.K_KP_8: 0x60, sdl.K_KP_9: 0x61, sdl.K_KP_0: 0x62,
	sdl.K_KP_PERIOD: 0x63, sdl.K_KP_EQUALS: 0x67, sdl.K_KP_COMMA: 0x85,

	sdl.K_LCTRL: 0xE0, sdl.K_LSHIFT: 0xE1, sdl.K_LALT: 0xE2, sdl.K_LGUI: 0xE3,
	sdl.K_RCTRL: 0xE4, sdl.K_RSHIFT: 0xE5, sdl.K_RALT: 0xE6, sdl.K_RGUI: 0xE7,
}

func hidForKey(key sdl.Keycode) (uint8, bool) {
	u, ok := keyToHID[key]
	return u, ok
}
