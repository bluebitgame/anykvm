package bridge

import (
	"strings"
	"sync"

	"anykvm/internal/logging"
)

const (
	hidKeyAltLeft    uint8 = 0xE2
	hidKeyAltRight   uint8 = 0xE6
	hidKeyCtrlLeft   uint8 = 0xE0
	hidKeyCtrlRight  uint8 = 0xE4
	hidKeyShiftLeft  uint8 = 0xE1
	hidKeyShiftRight uint8 = 0xE5
	hidKeyMetaLeft   uint8 = 0xE3
	hidKeyMetaRight  uint8 = 0xE7
)

var hidKeyDigit = [10]uint8{
	0x1E, 0x1F, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
} // Digit1(0x1E) .. Digit0(0x27)

// parseModKey parses modifier strings such as "ctrl+alt" or "ctrl+alt+shift"
// into a slice of HID keycodes.
func parseModKey(mod string) []uint8 {
	if mod == "" {
		mod = "ctrl+alt"
	}
	var keys []uint8
	for _, part := range strings.Split(strings.ToLower(mod), "+") {
		part = strings.TrimSpace(part)
		switch part {
		case "alt":
			keys = append(keys, hidKeyAltLeft, hidKeyAltRight)
		case "ctrl":
			keys = append(keys, hidKeyCtrlLeft, hidKeyCtrlRight)
		case "shift":
			keys = append(keys, hidKeyShiftLeft, hidKeyShiftRight)
		case "meta", "cmd", "win":
			keys = append(keys, hidKeyMetaLeft, hidKeyMetaRight)
		}
	}
	return keys
}

// HotkeyMatcher intercepts modifier+digit combos for target switching.
// When a match is detected, SwitchMachine is called on the bridge.
type HotkeyMatcher struct {
	modKeys     []uint8        // modifier keycodes (e.g. both AltLeft and AltRight)
	digitKeys   [10]uint8      // Digit1..Digit0 HID codes
	numMachines int            // number of configured targets (max 10)
	swallowed   map[uint8]bool // keys whose release should also be swallowed
	heldModKeys map[uint8]bool // which specific modifier keys are pressed
	mu          sync.Mutex
}

// NewHotkeyMatcher creates a matcher for mod+digit.
func NewHotkeyMatcher(mod string, numMachines int) *HotkeyMatcher {
	h := &HotkeyMatcher{
		modKeys:     parseModKey(mod),
		digitKeys:   hidKeyDigit,
		numMachines: numMachines,
		swallowed:   make(map[uint8]bool),
		heldModKeys: make(map[uint8]bool),
	}
	return h
}

func (h *HotkeyMatcher) isModKey(code uint8) bool {
	for _, m := range h.modKeys {
		if code == m {
			return true
		}
	}
	return false
}

func (h *HotkeyMatcher) isDigitKey(code uint8) (int, bool) {
	for i := 0; i < 10 && i < h.numMachines; i++ {
		if code == h.digitKeys[i] {
			return i, true
		}
	}
	return -1, false
}

// CheckKeyDown returns (machineIndex, matched) to indicate a hotkey match.
// If matched, the caller should swallow the key and switch to the given target.
func (h *HotkeyMatcher) CheckKeyDown(code uint8) (idx int, matched bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.isModKey(code) {
		h.heldModKeys[code] = true
		return -1, false
	}

	if h.modifiersHeld() {
		i, ok := h.isDigitKey(code)
		if ok {
			h.swallowed[code] = true
			for m := range h.heldModKeys {
				h.swallowed[m] = true
			}
			logging.Infof("hotkey: switching to target %d", i)
			return i, true
		}
	}

	return -1, false
}

// CheckKeyRelease returns true if the release should be swallowed (part of a
// matched hotkey combo).
func (h *HotkeyMatcher) CheckKeyRelease(code uint8) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.isModKey(code) {
		if h.heldModKeys[code] {
			delete(h.heldModKeys, code)
		}
		if h.swallowed[code] {
			delete(h.swallowed, code)
			return true
		}
		return false
	}

	if h.swallowed[code] {
		delete(h.swallowed, code)
		return true
	}
	return false
}

// Each adjacent pair is the left/right variant of one required modifier.
// All configured modifiers must be held; repeats do not change this state.
func (h *HotkeyMatcher) modifiersHeld() bool {
	if len(h.modKeys) == 0 {
		return false
	}
	for i := 0; i < len(h.modKeys); i += 2 {
		if !h.heldModKeys[h.modKeys[i]] && !h.heldModKeys[h.modKeys[i+1]] {
			return false
		}
	}
	return true
}
