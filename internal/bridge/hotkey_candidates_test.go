package bridge

import "testing"

func TestTargetHotkeyCandidatesRequireEveryModifier(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right []uint8
	}{
		{"ctrl+alt", []uint8{hidKeyCtrlLeft, hidKeyAltLeft}, []uint8{hidKeyCtrlRight, hidKeyAltRight}},
		{"alt+shift", []uint8{hidKeyAltLeft, hidKeyShiftLeft}, []uint8{hidKeyAltRight, hidKeyShiftRight}},
		{"ctrl+shift", []uint8{hidKeyCtrlLeft, hidKeyShiftLeft}, []uint8{hidKeyCtrlRight, hidKeyShiftRight}},
		{"ctrl+alt+shift", []uint8{hidKeyCtrlLeft, hidKeyAltLeft, hidKeyShiftLeft}, []uint8{hidKeyCtrlRight, hidKeyAltRight, hidKeyShiftRight}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for missing := range tc.left {
				h := NewHotkeyMatcher(tc.name, 2)
				for i, key := range tc.left {
					if i != missing {
						h.CheckKeyDown(key)
					}
				}
				if _, matched := h.CheckKeyDown(0x1f); matched {
					t.Fatalf("matched without required modifier %x", tc.left[missing])
				}
			}
			h := NewHotkeyMatcher(tc.name, 2)
			for _, key := range tc.right {
				h.CheckKeyDown(key)
			}
			if index, matched := h.CheckKeyDown(0x1f); !matched || index != 1 {
				t.Fatal("complete right-hand combination did not select target 2")
			}
			h.CheckKeyRelease(0x1f)
			for _, key := range tc.right {
				if !h.CheckKeyRelease(key) {
					t.Fatal("matched modifier release leaked into target")
				}
			}
			if _, matched := h.CheckKeyDown(0x1f); matched {
				t.Fatal("released combination remained active")
			}
		})
	}
	h := NewHotkeyMatcher("", 2)
	h.CheckKeyDown(hidKeyAltLeft)
	if _, matched := h.CheckKeyDown(0x1e); matched {
		t.Fatal("default still accepts Alt alone")
	}
	h.CheckKeyDown(hidKeyCtrlLeft)
	if index, matched := h.CheckKeyDown(0x1e); !matched || index != 0 {
		t.Fatal("default does not accept Ctrl+Alt")
	}
}
