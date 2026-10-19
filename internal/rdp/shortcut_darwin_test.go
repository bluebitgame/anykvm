package rdp

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type shortcutHID struct {
	keyRecorder
	held     [256]bool
	chords   []string
	failCode uint8
}

func (h *shortcutHID) KeyDown(code uint8) error {
	h.held[code] = true
	if code < 0xe0 {
		h.chords = append(h.chords, fmt.Sprintf("%02x ctrl=%t cmd=%t shift=%t", code,
			h.held[0xe0] || h.held[0xe4], h.held[0xe3] || h.held[0xe7], h.held[0xe1] || h.held[0xe5]))
	}
	return h.keyRecorder.KeyDown(code)
}

func (h *shortcutHID) KeyRelease(code uint8) error {
	if code == h.failCode {
		return errors.New("release failed")
	}
	h.held[code] = false
	return h.keyRecorder.KeyRelease(code)
}

func shortcutScan(i *inputState, fast bool, scan, flags uint16) error {
	if !fast {
		return i.slow(join(u16(1), u16(0), u32(0), u16(4), u16(int(flags)), u16(int(scan)), u16(0)))
	}
	header := byte(0)
	if flags&0x8000 != 0 {
		header |= 1
	}
	if flags&0x100 != 0 {
		header |= 2
	}
	return i.fast([]byte{4, header, byte(scan)})
}

func TestMacShortcutsBothInputPaths(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, unicode := range []bool{false, true} {
			for _, key := range []struct {
				r          uint16
				scan, code uint8
				shift      bool
			}{
				{'c', 0x2e, 0x06, false}, {'x', 0x2d, 0x1b, false}, {'v', 0x2f, 0x19, false},
				{'a', 0x1e, 0x04, false}, {'z', 0x2c, 0x1d, false}, {'f', 0x21, 0x09, false},
				{'w', 0x11, 0x1a, false}, {'+', 0x0d, 0x2e, true}, {'-', 0x0c, 0x2d, false},
			} {
				t.Run(fmt.Sprintf("fast=%t/unicode=%t/%c", fast, unicode, key.r), func(t *testing.T) {
					h := &shortcutHID{}
					var i inputState
					i.setMacShortcuts(true)
					i.activate(h)
					must := func(err error) {
						t.Helper()
						if err != nil {
							t.Fatal(err)
						}
					}
					must(shortcutScan(&i, fast, 0x1d, 0)) // Left Control.
					if unicode {
						for _, release := range []bool{false, true} {
							if fast {
								must(i.fast(fastUnicode([]uint16{key.r}, release)))
							} else {
								must(i.slow(slowUnicode([]uint16{key.r}, release)))
							}
						}
						if i.keys[key.code] {
							t.Fatal("Unicode shortcut left key held")
						}
					} else {
						if key.shift {
							must(shortcutScan(&i, fast, 0x2a, 0))
						}
						must(shortcutScan(&i, fast, uint16(key.scan), 0))
						must(shortcutScan(&i, fast, uint16(key.scan), 0x8000))
					}
					want := []string{fmt.Sprintf("%02x ctrl=false cmd=true shift=%t", key.code, key.shift)}
					if !reflect.DeepEqual(h.chords, want) {
						t.Fatalf("chords=%v want=%v", h.chords, want)
					}
					if !h.held[0xe0] || h.held[0xe3] || !i.keys[0xe0] || i.keys[0xe3] {
						t.Fatal("client modifiers were not restored")
					}
					i.release()
					if h.held != [256]bool{} || i.keys != [256]bool{} {
						t.Fatal("disconnect left keys held")
					}
				})
			}
		}
	}
}

func TestMacShortcutClientOSPolicyAtActivation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		os     uint16
		mapped bool
	}{
		{"unspecified", 0, false}, {"Windows", 1, false}, {"OS2", 2, false},
		{"Macintosh", 3, true}, {"UNIX", 4, false}, {"iOS", 5, true},
		{"macOS", 6, true}, {"Android", 7, false}, {"ChromeOS", 8, false},
		{"unknown", 0xffff, false},
	} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%t", tc.name, disabled), func(t *testing.T) {
				c := channelTestClient()
				c.s.cfg.DisableAutoShortcutMapping = disabled
				c.activated = false
				general := make([]byte, 20)
				le.PutUint16(general, tc.os)
				le.PutUint16(general[2:], 6)
				cap := capability(1, general)
				p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(cap)+4), u16(1), u16(0), cap))
				if err := c.handleShare(p); err != nil {
					t.Fatal(err)
				}
				wantSwap := !disabled && tc.os != 0 && !tc.mapped
				if c.input.swapLeftModifiers != wantSwap || c.input.macShortcuts != (tc.mapped && !disabled) {
					t.Fatal("activation selected incorrect shortcut policy")
				}
				// Input-path variants are covered by TestMacShortcutsBothInputPaths
				// and TestNonMacLeftModifierSwap. Check activation wiring once here.
				h := &shortcutHID{}
				c.input.activate(h)
				for _, scan := range []uint16{0x1d, 0x2e} {
					if err := c.input.key(scan, 0); err != nil {
						t.Fatal(err)
					}
				}
				mapped := tc.mapped && !disabled
				want := []string{fmt.Sprintf("06 ctrl=%t cmd=%t shift=false", !mapped, mapped)}
				if !reflect.DeepEqual(h.chords, want) {
					t.Fatalf("chords=%v want %v", h.chords, want)
				}
				c.input.release()
				if h.held != [256]bool{} {
					t.Fatal("policy left modifiers held")
				}
			})
		}
	}
}

func TestNonMacLeftModifierSwap(t *testing.T) {
	for _, osMajor := range []uint16{0, 1, 3, 4, 5, 6, 7, 8, 0xffff} {
		for _, enabled := range []bool{false, true} {
			for _, fast := range []bool{false, true} {
				for _, unicode := range []bool{false, true} {
					t.Run(fmt.Sprintf("os=%d/enabled=%t/fast=%t/unicode=%t", osMajor, enabled, fast, unicode), func(t *testing.T) {
						h := &shortcutHID{}
						var i inputState
						i.configureShortcuts(osMajor, enabled)
						i.activate(h)
						swap := enabled && osMajor != 0 && osMajor != 3 && osMajor != 5 && osMajor != 6
						for _, key := range []struct {
							scan, flags uint16
							code        uint8
						}{
							{0x38, 0, 0xe2}, {0x5b, 0x100, 0xe3},
							{0x38, 0x100, 0xe6}, {0x5c, 0x100, 0xe7},
						} {
							code := key.code
							if swap && (code == 0xe2 || code == 0xe3) {
								code ^= 1
							}
							h.keys, h.chords = nil, nil
							for repeat := 0; repeat < 2; repeat++ {
								if err := shortcutScan(&i, fast, key.scan, key.flags); err != nil {
									t.Fatal(err)
								}
							}
							if !h.held[code] || !i.keys[code] || !reflect.DeepEqual(h.keys, []string{fmt.Sprintf("down:%02x", code)}) {
								t.Fatal("modifier repeat or held state uses the wrong key", h.keys)
							}
							if unicode {
								if err := i.unicodeKey('c', false); err != nil {
									t.Fatal(err)
								}
							} else {
								if err := shortcutScan(&i, fast, 0x2e, 0); err != nil {
									t.Fatal(err)
								}
								if err := shortcutScan(&i, fast, 0x2e, 0x8000); err != nil {
									t.Fatal(err)
								}
							}
							want := []string{fmt.Sprintf("06 ctrl=false cmd=%t shift=false", code == 0xe3 || code == 0xe7)}
							if !reflect.DeepEqual(h.chords, want) {
								t.Fatalf("chord=%v want=%v", h.chords, want)
							}
							if err := shortcutScan(&i, fast, key.scan, key.flags|0x8000); err != nil {
								t.Fatal(err)
							}
							if h.held != [256]bool{} || i.keys != [256]bool{} {
								t.Fatal("mapped modifier release left a key held")
							}
						}
						// Both left keys can be held together. Releasing Alt releases
						// Command only; the swapped Option survives until disconnect.
						if err := shortcutScan(&i, fast, 0x38, 0); err != nil {
							t.Fatal(err)
						}
						if err := shortcutScan(&i, fast, 0x5b, 0x100); err != nil {
							t.Fatal(err)
						}
						if !h.held[0xe2] || !h.held[0xe3] {
							t.Fatal("simultaneous modifiers collided")
						}
						if err := shortcutScan(&i, fast, 0x38, 0x8000); err != nil {
							t.Fatal(err)
						}
						if h.held[0xe2] != swap || h.held[0xe3] == swap {
							t.Fatal("releasing one modifier released the other")
						}
						i.release()
						if h.held != [256]bool{} || i.keys != [256]bool{} {
							t.Fatal("disconnect retained swapped modifier")
						}
					})
				}
			}
		}
	}
}

func TestSwappedModifierReleaseFailureCleanup(t *testing.T) {
	for _, code := range []uint8{0xe2, 0xe3} {
		h := &shortcutHID{failCode: code}
		var i inputState
		i.configureShortcuts(1, true)
		i.activate(h)
		scan, flags := uint16(0x38), uint16(0)
		if code == 0xe2 {
			scan, flags = 0x5b, 0x100
		}
		if err := i.key(scan, flags); err != nil {
			t.Fatal(err)
		}
		if err := i.key(scan, flags|0x8000); err == nil || !i.keys[code] || !h.held[code] {
			t.Fatal("failed release lost the mapped key needed for cleanup")
		}
		h.failCode = 0
		i.release()
		if h.held != [256]bool{} {
			t.Fatal("disconnect did not retry mapped release")
		}
	}
}

func TestMacShortcutMissingGeneralCapabilityDefaultsOff(t *testing.T) {
	c := channelTestClient()
	c.activated = false
	// Even a build matching a Microsoft release is not an OS identifier.
	c.data.clientBuild = 3078
	p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(4), u16(0), u16(0)))
	if err := c.handleShare(p); err != nil {
		t.Fatal(err)
	}
	h := &shortcutHID{}
	c.input.activate(h)
	if err := c.input.key(0x1d, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.input.key(0x2e, 0); err != nil {
		t.Fatal(err)
	}
	if want := []string{"06 ctrl=true cmd=false shift=false"}; !reflect.DeepEqual(h.chords, want) {
		t.Fatalf("unidentified client remapped: %v", h.chords)
	}
	c.input.release()
}

func TestMacShortcutUnicodeForms(t *testing.T) {
	for _, r := range []uint16{'c', 'C', 3} {
		h := &shortcutHID{}
		var i inputState
		i.setMacShortcuts(true)
		i.activate(h)
		if err := i.key(0x1d, 0); err != nil {
			t.Fatal(err)
		}
		if err := i.unicodeKey(r, false); err != nil {
			t.Fatal(err)
		}
		if want := []string{"06 ctrl=false cmd=true shift=false"}; !reflect.DeepEqual(h.chords, want) {
			t.Fatalf("r=%x chords=%v", r, h.chords)
		}
		i.release()
	}
}

func TestMacShortcutRightControlKeepsConsoleInterrupt(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, unicode := range []bool{false, true} {
			for _, both := range []bool{false, true} {
				t.Run(fmt.Sprintf("fast=%t/unicode=%t/both=%t", fast, unicode, both), func(t *testing.T) {
					h := &shortcutHID{}
					var i inputState
					i.setMacShortcuts(true)
					i.activate(h)
					must := func(err error) {
						t.Helper()
						if err != nil {
							t.Fatal(err)
						}
					}
					if both {
						must(shortcutScan(&i, fast, 0x1d, 0))
					}
					must(shortcutScan(&i, fast, 0x1d, 0x100)) // Right Control.
					before := len(h.keys)
					if unicode {
						if fast {
							must(i.fast(fastUnicode([]uint16{'c'}, false)))
						} else {
							must(i.slow(slowUnicode([]uint16{'c'}, false)))
						}
					} else {
						must(shortcutScan(&i, fast, 0x2e, 0))
						must(shortcutScan(&i, fast, 0x2e, 0x8000))
					}
					if want := []string{"06 ctrl=true cmd=false shift=false"}; !reflect.DeepEqual(h.chords, want) {
						t.Fatalf("console Ctrl+C changed: %v", h.chords)
					}
					if got := h.keys[before:]; !reflect.DeepEqual(got, []string{"down:06", "up:06"}) {
						t.Fatalf("changed modifiers for right Ctrl+C: %v", got)
					}
					if !i.keys[0xe4] || i.keys[0xe0] != both || i.keys[0xe3] {
						t.Fatal("changed held Control state")
					}
					i.release()
					if h.held != [256]bool{} {
						t.Fatal("disconnect left keys held")
					}
				})
			}
		}
	}
}

func TestMacShortcutPreservesHeldKeys(t *testing.T) {
	h := &shortcutHID{}
	var i inputState
	i.setMacShortcuts(true)
	i.activate(h)
	for _, scan := range []uint16{0x1d, 0x36, 0x1e, 0x1e} {
		if err := i.key(scan, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.unicodeKey('A', false); err != nil {
		t.Fatal(err)
	}
	if !i.keys[0xe0] || !i.keys[0xe5] || !i.keys[0x04] || i.keys[0xe1] || i.keys[0xe3] {
		t.Fatal("changed client key state")
	}
	if want := []string{"04 ctrl=false cmd=true shift=true", "04 ctrl=false cmd=true shift=true", "04 ctrl=false cmd=true shift=true"}; !reflect.DeepEqual(h.chords, want) {
		t.Fatal(h.chords)
	}
	i.release()
	if h.held != [256]bool{} {
		t.Fatal("disconnect left keys held")
	}
}

func TestMacShortcutScopeAndErrors(t *testing.T) {
	for _, fixture := range []struct {
		name                              string
		modifierScan, modifierFlags, scan uint16
		want                              string
	}{
		{"plain C", 0, 0, 0x2e, "06 ctrl=false cmd=false shift=false"},
		{"Ctrl S", 0x1d, 0, 0x1f, "16 ctrl=true cmd=false shift=false"},
		{"Ctrl N", 0x1d, 0, 0x31, "11 ctrl=true cmd=false shift=false"},
		{"Command C", 0x5b, 0x100, 0x2e, "06 ctrl=false cmd=true shift=false"},
		{"Alt C", 0x38, 0, 0x2e, "06 ctrl=false cmd=false shift=false"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			h := &shortcutHID{}
			var i inputState
			i.setMacShortcuts(true)
			i.activate(h)
			if fixture.modifierScan != 0 {
				if err := i.key(fixture.modifierScan, fixture.modifierFlags); err != nil {
					t.Fatal(err)
				}
			}
			if err := i.key(fixture.scan, 0); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(h.chords, []string{fixture.want}) {
				t.Fatal(h.chords)
			}
			i.release()
		})
	}
	for _, modifier := range []uint16{0x38, 0x5b} {
		h := &shortcutHID{}
		var i inputState
		i.setMacShortcuts(true)
		i.activate(h)
		if err := i.key(0x1d, 0); err != nil {
			t.Fatal(err)
		}
		flags := uint16(0)
		if modifier == 0x5b {
			flags = 0x100
		}
		if err := i.key(modifier, flags); err != nil {
			t.Fatal(err)
		}
		before := len(h.keys)
		if err := i.key(0x2e, 0); err != nil {
			t.Fatal(err)
		}
		if len(h.keys) != before+1 {
			t.Fatal("converted a chord with Alt or Command already held", h.keys)
		}
		i.release()
	}
	h := &shortcutHID{failCode: 0xe3}
	var i inputState
	i.setMacShortcuts(true)
	i.activate(h)
	if err := i.key(0x1d, 0); err != nil {
		t.Fatal(err)
	}
	if err := i.unicodeKey('c', false); err == nil {
		t.Fatal("lost Command release error")
	}
	if !h.held[0xe0] || !i.keys[0xe3] {
		t.Fatal("lost modifiers needed for cleanup")
	}
	h.failCode = 0
	i.release()
	if h.held != [256]bool{} {
		t.Fatal("disconnect did not clean up failed shortcut")
	}
}
