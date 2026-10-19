package rdp

import (
	"errors"
	"fmt"
	"image"
	"reflect"
	"testing"
	"unicode/utf16"

	"anykvm/internal/bridge"
	"anykvm/internal/hid"
)

type mouseRecorder struct {
	recordingHID
	positions [][2]int
}

func (h *mouseRecorder) MouseMove(x, y int) error {
	h.positions = append(h.positions, [2]int{x, y})
	return nil
}

func TestMouseButtonUsesItsCoordinates(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(fmt.Sprintf("fast=%t", fast), func(t *testing.T) {
			h := &mouseRecorder{}
			var i inputState
			i.activate(h)
			i.geometry(1650, 1050, 1650, 1050)
			for _, flags := range []int{0x9000, 0x1000} {
				var err error
				if fast {
					err = i.fast(join([]byte{4, 0x20}, u16(flags), u16(1649), u16(1049)))
				} else {
					err = i.slow(join(u16(1), u16(0), u32(0), u16(0x8001), u16(flags), u16(1649), u16(1049)))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(h.positions, [][2]int{{32767, 32767}, {32767, 32767}}) {
				t.Fatalf("button coordinates ignored: %v", h.positions)
			}
		})
	}
}

func TestMouseUsesActualFrameContent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		w, h, sw, sh int
		content      image.Rectangle
	}{
		{"matched auto", 1650, 1050, 1650, 1050, image.Rect(0, 0, 1650, 1050)},
		{"fixed stretched desktop", 1920, 1080, 2880, 1800, image.Rect(0, 0, 1920, 1080)},
		{"frame metadata overrides estimated fit", 1280, 720, 1920, 1080, image.Rect(32, 18, 1248, 702)},
		{"pillarboxed desktop", 1280, 720, 1000, 1000, image.Rect(280, 0, 1000, 720)},
		{"letterboxed desktop", 1650, 1050, 1920, 1080, image.Rect(0, 61, 1650, 989)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &mouseRecorder{}
			var i inputState
			i.activate(h)
			// The initial client request deliberately differs from the transmitted
			// picture. Frame geometry must replace it before handling pointer input.
			i.geometry(1976, 952, 1976, 952)
			i.frameGeometry(bridge.Frame{Width: tc.w, Height: tc.h, SourceWidth: tc.sw, SourceHeight: tc.sh, ContentRect: tc.content}, tc.w, tc.h)
			points := []image.Point{tc.content.Min, tc.content.Max.Sub(image.Pt(1, 1)), tc.content.Min.Add(image.Pt((tc.content.Dx()-1)/4, (tc.content.Dy()-1)/4))}
			for _, p := range points {
				if err := i.mouse(0x800, uint16(p.X), uint16(p.Y), false); err != nil {
					t.Fatal(err)
				}
			}
			if h.positions[0] != [2]int{0, 0} || h.positions[1] != [2]int{32767, 32767} {
				t.Fatalf("content corners=%v", h.positions[:2])
			}
			for _, n := range h.positions[2] {
				if n < 8150 || n > 8230 {
					t.Fatalf("quarter point=%v; expected about 8192", h.positions[2])
				}
			}
			if err := i.mouse(0x800, 0, 0, false); err != nil {
				t.Fatal(err)
			}
			if err := i.mouse(0x800, uint16(tc.w-1), uint16(tc.h-1), false); err != nil {
				t.Fatal(err)
			}
			if h.positions[3] != [2]int{0, 0} || h.positions[4] != [2]int{32767, 32767} {
				t.Fatal("black borders did not clamp to desktop edges")
			}
			count := len(h.positions)
			if err := i.mouse(0x800|0x200|120, 0, 0, false); err != nil {
				t.Fatal(err)
			}
			if len(h.positions) != count {
				t.Fatal("wheel event moved the pointer")
			}
		})
	}
}

func TestMouseContentBoundsFollowEncodedSize(t *testing.T) {
	h := &mouseRecorder{}
	var i inputState
	i.activate(h)
	i.frameGeometry(bridge.Frame{Width: 129, Height: 131, ContentRect: image.Rect(0, 0, 129, 131)}, 130, 132)
	if i.contentRect != image.Rect(0, 0, 128, 132) {
		t.Fatalf("AVC420 fitted content=%v", i.contentRect)
	}
	if err := i.mouse(0x800, 127, 131, false); err != nil {
		t.Fatal(err)
	}
	if err := i.mouse(0x800, 129, 131, false); err != nil {
		t.Fatal(err)
	}
	if h.positions[0] != [2]int{32767, 32767} || h.positions[1] != [2]int{32767, 32767} {
		t.Fatalf("encoded corner=%v", h.positions)
	}
}

type keyRecorder struct {
	recordingHID
	keys        []string
	failRelease bool
}

func (h *keyRecorder) KeyDown(code uint8) error {
	h.keys = append(h.keys, fmt.Sprintf("down:%02x", code))
	return nil
}
func (h *keyRecorder) KeyRelease(code uint8) error {
	h.keys = append(h.keys, fmt.Sprintf("up:%02x", code))
	if h.failRelease {
		return errors.New("release failed")
	}
	return nil
}

type textRecorder struct {
	keyRecorder
	text []string
	err  error
}

func (h *textRecorder) SendText(text string) error {
	h.text = append(h.text, text)
	return h.err
}

// Model a text field so composition replacement checks the resulting text,
// rather than only checking that individual events reached a recorder.
type compositionHID struct {
	keyRecorder
	value []rune
	held  [256]bool
}

func (h *compositionHID) SendText(text string) error {
	h.value = append(h.value, []rune(text)...)
	return nil
}

func (h *compositionHID) KeyDown(code uint8) error {
	// A physical HID report only acts on a transition to pressed.
	if !h.held[code] && code == 0x2a && len(h.value) > 0 {
		h.value = h.value[:len(h.value)-1]
	}
	h.held[code] = true
	return h.keyRecorder.KeyDown(code)
}

func (h *compositionHID) KeyRelease(code uint8) error {
	h.held[code] = false
	return h.keyRecorder.KeyRelease(code)
}

func TestMobileCompositionReplacement(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, fixture := range []struct{ composing, committed string }{
			{"nihao", "你好"}, {"kanji", "漢字"}, {"emoji", "😀"},
		} {
			t.Run(fmt.Sprintf("fast=%t/%s", fast, fixture.committed), func(t *testing.T) {
				h := &compositionHID{value: []rune("prefix ")}
				var i inputState
				i.activate(h)
				sendText := func(text string) {
					units := utf16.Encode([]rune(text))
					for _, release := range []bool{false, true} {
						var err error
						if fast {
							err = i.fast(fastUnicode(units, release))
						} else {
							err = i.slow(slowUnicode(units, release))
						}
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				key := func(scan, flags uint16) {
					var err error
					if fast {
						header := byte(0)
						if flags&0x8000 != 0 {
							header = 1
						}
						err = i.fast([]byte{4, header, byte(scan)})
					} else {
						err = i.slow(join(u16(1), u16(0), u32(0), u16(4), u16(int(flags)), u16(int(scan)), u16(0)))
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				sendText(fixture.composing)
				// Repeated downs with one final up are valid scancode input.
				for range []rune(fixture.composing) {
					key(0x0e, 0)
				}
				key(0x0e, 0x8000)
				sendText(fixture.committed)
				if got, want := string(h.value), "prefix "+fixture.committed; got != want {
					t.Fatalf("replacement=%q want=%q", got, want)
				}
				i.release()
			})
		}
	}
}

func TestUnicodeNonASCIIWithHeldModifiers(t *testing.T) {
	for _, modifier := range []uint8{0xe0, 0xe2, 0xe3, 0xe4, 0xe6, 0xe7} {
		t.Run(fmt.Sprintf("modifier=%02x", modifier), func(t *testing.T) {
			h := &textRecorder{}
			var i inputState
			i.activate(h)
			i.keys[modifier] = true
			for _, unit := range utf16.Encode([]rune("中文漢字😀")) {
				if err := i.unicodeKey(unit, false); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(h.text, []string{"中", "文", "漢", "字", "😀"}) {
				t.Fatalf("committed text lost with held modifier: %q", h.text)
			}
			if !i.keys[modifier] || len(h.keys) != 0 {
				t.Fatal("committed text changed physical key state")
			}
		})
	}
}

func TestRepeatedModifierAndLockKeys(t *testing.T) {
	for _, scan := range []uint16{0x1d, 0x2a, 0x36, 0x38, 0x3a, 0x45, 0x46} {
		t.Run(fmt.Sprintf("scan=%02x", scan), func(t *testing.T) {
			h := &keyRecorder{}
			var i inputState
			i.activate(h)
			for n := 0; n < 3; n++ {
				if err := i.key(scan, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := i.key(scan, 0x8000); err != nil {
				t.Fatal(err)
			}
			if err := i.key(scan, 0x8000); err != nil {
				t.Fatal(err)
			}
			code := hidKey(scan, false)
			want := []string{fmt.Sprintf("down:%02x", code), fmt.Sprintf("up:%02x", code)}
			if !reflect.DeepEqual(h.keys, want) || i.keys[code] {
				t.Fatalf("modifier/lock toggled on repeat: %v", h.keys)
			}
		})
	}
}

func slowUnicode(units []uint16, release bool) []byte {
	p := join(u16(len(units)), u16(0))
	flags := 0
	if release {
		flags = 0x8000
	}
	for _, unit := range units {
		p = append(p, join(u32(0), u16(5), u16(flags), u16(int(unit)), u16(0))...)
	}
	return p
}

func fastUnicode(units []uint16, release bool) []byte {
	p := []byte{0, byte(len(units))} // Explicit event count also covers batches >15.
	header := byte(4 << 5)
	if release {
		header |= 1
	}
	for _, unit := range units {
		p = append(p, header, byte(unit), byte(unit>>8))
	}
	return p
}

func TestUnicodeInputBothPaths(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(fmt.Sprintf("fast=%t", fast), func(t *testing.T) {
			h := &textRecorder{}
			var i inputState
			i.activate(h)
			send := func(units []uint16, release bool) error {
				if fast {
					return i.fast(fastUnicode(units, release))
				}
				return i.slow(slowUnicode(units, release))
			}
			// Repeat downs must emit repeated characters, without duplicating ups.
			units := []uint16{'a', 'a', 'A', '!', '中', '文', 0xd83d}
			if err := send(units, false); err != nil {
				t.Fatal(err)
			}
			if err := send(units, true); err != nil {
				t.Fatal(err)
			}
			// The surrogate pair spans packets and an intervening key release.
			if err := send([]uint16{0xde00}, false); err != nil {
				t.Fatal(err)
			}
			if err := send([]uint16{0xde00}, true); err != nil {
				t.Fatal(err)
			}
			if want := []string{"a", "a", "A", "!", "中", "文", "😀"}; !reflect.DeepEqual(h.text, want) {
				t.Fatalf("text=%q want=%q", h.text, want)
			}
			if len(h.keys) != 0 {
				t.Fatal("text was also emitted as HID keys", h.keys)
			}
		})
	}
}

func TestUnicodeHIDFallbackAndControls(t *testing.T) {
	h := &keyRecorder{}
	var i inputState
	i.activate(h)
	for _, r := range "aA!\b\t\r\x1b\x7f中\uffbe" {
		if err := i.unicodeKey(uint16(r), false); err != nil {
			t.Fatal(err)
		}
		if err := i.unicodeKey(uint16(r), true); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"down:04", "up:04", "down:e1", "down:04", "up:04", "up:e1", "down:e1", "down:1e", "up:1e", "up:e1",
		"down:2a", "up:2a", "down:2b", "up:2b", "down:28", "up:28", "down:29", "up:29", "down:4c", "up:4c"}
	if !reflect.DeepEqual(h.keys, want) {
		t.Fatalf("keys=%v want=%v", h.keys, want)
	}
	i.release()
	if !reflect.DeepEqual(h.keys, want) {
		t.Fatal("tap left keys held", h.keys)
	}
}

func TestUnicodePreservesHeldKeysAndShortcuts(t *testing.T) {
	h := &textRecorder{}
	var i inputState
	i.activate(h)
	if err := i.key(0x1d, 0); err != nil {
		t.Fatal(err)
	} // Ctrl
	if err := i.key(0x36, 0); err != nil {
		t.Fatal(err)
	} // Right Shift
	if err := i.key(0x30, 0); err != nil {
		t.Fatal(err)
	} // B
	if err := i.unicodeKey('B', false); err != nil {
		t.Fatal(err)
	}
	if len(h.text) != 0 {
		t.Fatal("shortcut injected literal text")
	}
	if !i.keys[0xe0] || !i.keys[0xe5] || !i.keys[5] || i.keys[0xe1] {
		t.Fatal("physical key state changed")
	}
	i.release()
	want := []string{"down:e0", "down:e5", "down:05", "up:05", "down:05", "up:05", "up:e0", "up:e5"}
	if !reflect.DeepEqual(h.keys, want) {
		t.Fatalf("keys=%v want=%v", h.keys, want)
	}
}

func TestUnicodeSurrogateAndSessionBoundaries(t *testing.T) {
	h := &textRecorder{}
	var i inputState
	i.activate(h)
	for _, unit := range []uint16{0xdc00, 0xd800, 'x', 0xdc00, 0xd83d} {
		if err := i.unicodeKey(unit, false); err != nil {
			t.Fatal(err)
		}
	}
	i.release()
	i.activate(h)
	if err := i.unicodeKey(0xde00, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.text, []string{"x"}) {
		t.Fatalf("invalid or stale surrogate emitted: %q", h.text)
	}
	if i.fast([]byte{4, 4 << 5, 'a'}) == nil {
		t.Fatal("accepted truncated Unicode event")
	}
}

func TestUnicodeBackendErrors(t *testing.T) {
	h := &textRecorder{err: hid.ErrTextUnsupported}
	var i inputState
	i.activate(h)
	if err := i.unicodeKey('a', false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.keys, []string{"down:04", "up:04"}) {
		t.Fatal("unsupported native text did not fall back", h.keys)
	}
	h.err = errors.New("native input failed")
	if err := i.unicodeKey('中', false); !errors.Is(err, h.err) {
		t.Fatalf("lost native error: %v", err)
	}
	h.failRelease = true
	h.err = hid.ErrTextUnsupported
	if err := i.unicodeKey('A', false); err == nil {
		t.Fatal("lost HID error")
	}
	h.failRelease = false
	i.release()
	if i.keys[4] || i.keys[0xe1] {
		t.Fatal("failed tap not cleaned up")
	}
}
