package bridge

import "testing"

func TestHotkeyMatcher_AltDigit(t *testing.T) {
	h := NewHotkeyMatcher("alt", 10)

	// Alt+1 should match machine 0.
	idx, matched := h.CheckKeyDown(hidKeyAltLeft)
	if matched {
		t.Fatal("Alt key alone should not match")
	}

	idx, matched = h.CheckKeyDown(0x1E) // Digit1
	if !matched {
		t.Fatal("Alt+1 should match")
	}
	if idx != 0 {
		t.Fatalf("Alt+1 should match machine 0, got %d", idx)
	}

	// Release Digit1 should be swallowed.
	if !h.CheckKeyRelease(0x1E) {
		t.Fatal("Digit1 release should be swallowed")
	}

	// Release AltLeft should be swallowed (part of hotkey combo).
	if !h.CheckKeyRelease(hidKeyAltLeft) {
		t.Fatal("Alt release after hotkey should be swallowed")
	}
}

func TestHotkeyMatcher_AltNoMatch(t *testing.T) {
	h := NewHotkeyMatcher("alt", 10)

	// Alt+Tab should not match.
	h.CheckKeyDown(hidKeyAltLeft)

	idx, matched := h.CheckKeyDown(0x2B) // Tab
	if matched {
		t.Fatal("Alt+Tab should not match")
	}
	_ = idx

	// Release Tab: not swallowed.
	if h.CheckKeyRelease(0x2B) {
		t.Fatal("Tab release should NOT be swallowed")
	}

	// Release Alt: not swallowed (no hotkey matched).
	if h.CheckKeyRelease(hidKeyAltLeft) {
		t.Fatal("Alt release without hotkey should NOT be swallowed")
	}
}

func TestHotkeyMatcher_AltRight(t *testing.T) {
	h := NewHotkeyMatcher("alt", 5)

	// AltRight+2 should match machine 1.
	h.CheckKeyDown(hidKeyAltRight)

	idx, matched := h.CheckKeyDown(0x1F) // Digit2
	if !matched {
		t.Fatal("RightAlt+2 should match")
	}
	if idx != 1 {
		t.Fatalf("RightAlt+2 should match machine 1, got %d", idx)
	}

	if !h.CheckKeyRelease(0x1F) {
		t.Fatal("Digit2 release should be swallowed")
	}
	if !h.CheckKeyRelease(hidKeyAltRight) {
		t.Fatal("RightAlt release after hotkey should be swallowed")
	}
}

func TestHotkeyMatcher_Digit0(t *testing.T) {
	h := NewHotkeyMatcher("alt", 10)

	h.CheckKeyDown(hidKeyAltLeft)
	idx, matched := h.CheckKeyDown(0x27) // Digit0
	if !matched {
		t.Fatal("Alt+0 should match")
	}
	if idx != 9 {
		t.Fatalf("Alt+0 should match machine 9, got %d", idx)
	}
}

func TestHotkeyMatcher_CtrlAlt(t *testing.T) {
	h := NewHotkeyMatcher("ctrl+alt", 10)

	h.CheckKeyDown(hidKeyCtrlLeft)
	h.CheckKeyDown(hidKeyAltLeft)

	idx, matched := h.CheckKeyDown(0x22) // Digit5
	if !matched {
		t.Fatal("Ctrl+Alt+5 should match")
	}
	if idx != 4 {
		t.Fatalf("Ctrl+Alt+5 should match machine 4, got %d", idx)
	}
}

func TestHotkeyMatcher_ParseModKey(t *testing.T) {
	keys := parseModKey("ctrl+shift")
	foundCtrl := false
	foundShift := false
	for _, k := range keys {
		if k == hidKeyCtrlLeft || k == hidKeyCtrlRight {
			foundCtrl = true
		}
		if k == hidKeyShiftLeft || k == hidKeyShiftRight {
			foundShift = true
		}
	}
	if !foundCtrl || !foundShift {
		t.Fatal("ctrl+shift should parse to ctrl and shift keys")
	}

	keys = parseModKey("")
	if len(keys) != 4 {
		t.Fatalf("empty mod should default to ctrl+alt, got %d keys", len(keys))
	}
}

func TestHotkeyRequiresEveryModifier(t *testing.T) {
	h := NewHotkeyMatcher("ctrl+alt", 2)
	h.CheckKeyDown(hidKeyCtrlLeft)
	if _, matched := h.CheckKeyDown(0x1e); matched {
		t.Fatal("Ctrl alone must not match Ctrl+Alt")
	}
	h.CheckKeyDown(hidKeyAltRight)
	if _, matched := h.CheckKeyDown(0x1e); !matched {
		t.Fatal("Ctrl+RightAlt should match")
	}
}

func TestModifierRepeatDoesNotLeaveHotkeyHeld(t *testing.T) {
	h := NewHotkeyMatcher("alt", 2)
	h.CheckKeyDown(hidKeyAltLeft)
	h.CheckKeyDown(hidKeyAltLeft)
	h.CheckKeyRelease(hidKeyAltLeft)
	if _, matched := h.CheckKeyDown(0x1e); matched {
		t.Fatal("released Alt must not remain held after repeat")
	}
}
