package wsmedia

import "testing"

func TestVideoKeyframeRequestRoutingAndRateLimit(t *testing.T) {
	requests := 0
	s := NewServer(Config{OnVideoKeyframeRequested: func() { requests++ }})
	c := &wsClient{}
	for i := 0; i < 20; i++ {
		s.handleInput(c, []byte{msgKeyframeReq})
	}
	if requests != 1 {
		t.Fatalf("keyframe requests = %d", requests)
	}
}

type fakeHID struct {
	keyDowns      []uint8
	keyReleases   []uint8
	relativeMoves [][2]int
	moves         [][2]int
	buttons       [][2]int // {button, down}
	wheels        []int
}

func (f *fakeHID) KeyDown(k uint8) error    { f.keyDowns = append(f.keyDowns, k); return nil }
func (f *fakeHID) KeyUp() error             { return nil }
func (f *fakeHID) KeyRelease(k uint8) error { f.keyReleases = append(f.keyReleases, k); return nil }
func (f *fakeHID) MouseMove(x, y int) error { f.moves = append(f.moves, [2]int{x, y}); return nil }
func (f *fakeHID) MouseMoveRel(x, y int) error {
	f.relativeMoves = append(f.relativeMoves, [2]int{x, y})
	return nil
}
func (f *fakeHID) MouseButton(b int, down bool) error {
	d := 0
	if down {
		d = 1
	}
	f.buttons = append(f.buttons, [2]int{b, d})
	return nil
}
func (f *fakeHID) MouseWheel(d int) error { f.wheels = append(f.wheels, d); return nil }

func TestKeyboardRouting(t *testing.T) {
	f := &fakeHID{}
	s := &Server{cfg: Config{HID: f}}
	c := &wsClient{}

	kb := func(down bool, code string) []byte {
		b := append([]byte{msgInputKeyboard, 0}, []byte(code)...)
		if down {
			b[1] = 1
		}
		return b
	}
	s.handleInput(c, kb(true, "KeyA"))  // down
	s.handleInput(c, kb(true, "KeyA"))  // repeat -> suppressed
	s.handleInput(c, kb(false, "KeyA")) // up

	if len(f.keyDowns) != 1 || f.keyDowns[0] != 0x04 {
		t.Fatalf("keyDowns = %v, want [0x04]", f.keyDowns)
	}
	if len(f.keyReleases) != 1 || f.keyReleases[0] != 0x04 {
		t.Fatalf("keyReleases = %v, want [0x04]", f.keyReleases)
	}
}

func TestPointerRouting(t *testing.T) {
	f := &fakeHID{}
	s := &Server{cfg: Config{HID: f}}
	s.meta.Width, s.meta.Height = 1000, 500
	c := &wsClient{}

	ptr := func(buttons uint8, x, y int) []byte {
		return []byte{msgInputMouse, buttons, byte(x >> 8), byte(x), byte(y >> 8), byte(y)}
	}
	s.handleInput(c, ptr(0x01, 500, 250)) // left down at center

	if len(f.moves) != 1 || f.moves[0] != [2]int{16399, 16416} {
		t.Fatalf("moves = %v, want [[16399 16416]]", f.moves)
	}
	if len(f.buttons) != 1 || f.buttons[0] != [2]int{1, 1} {
		t.Fatalf("buttons = %v, want [[1 1]]", f.buttons)
	}

	s.handleInput(c, ptr(0x00, 500, 250)) // release
	if len(f.buttons) != 2 || f.buttons[1] != [2]int{1, 0} {
		t.Fatalf("buttons = %v, want release [1 0]", f.buttons)
	}

	// releaseAll clears a still-held button.
	s.handleInput(c, ptr(0x04, 10, 10)) // right down
	s.releaseAll(c)
	last := f.buttons[len(f.buttons)-1]
	if last != [2]int{2, 0} {
		t.Fatalf("releaseAll last button = %v, want [2 0]", last)
	}
}

func TestHelloRouting(t *testing.T) {
	got := -1
	s := NewServer(Config{OnVideoH264Changed: func(v bool) {
		if v {
			got = 1
		} else {
			got = 0
		}
	}})
	c := &wsClient{done: make(chan struct{})}
	s.addClient(c)
	defer s.removeClient(c)
	s.handleInput(c, append([]byte{msgHello}, []byte(`{"h264":true}`)...))
	if got != 1 {
		t.Fatalf("OnVideoH264Changed got %d, want 1", got)
	}
}

func TestPointerInitialOriginAndEdges(t *testing.T) {
	f := &fakeHID{}
	s := NewServer(Config{HID: f})
	s.meta.Width, s.meta.Height = 100, 50
	c := &wsClient{}
	s.handlePointer(c, []byte{msgInputMouse, 1, 0, 0, 0, 0})
	if len(f.moves) != 1 || f.moves[0] != [2]int{0, 0} {
		t.Fatalf("initial origin not sent: %v", f.moves)
	}
	s.handlePointer(c, []byte{msgInputMouse, 0, 0, 99, 0, 49})
	if f.moves[1] != [2]int{32767, 32767} {
		t.Fatalf("edge not reachable: %v", f.moves)
	}
	s.handlePointer(c, []byte{msgInputMouse, 0, 255, 255, 255, 255})
	if f.moves[2] != [2]int{32767, 32767} {
		t.Fatalf("out-of-range input not clamped: %v", f.moves)
	}
}

func TestPointerBeforeMetadataDoesNotSuppressFirstMove(t *testing.T) {
	f := &fakeHID{}
	s := NewServer(Config{HID: f})
	c := &wsClient{}
	msg := []byte{msgInputMouse, 0, 0, 0, 0, 0}
	s.handlePointer(c, msg)
	s.meta.Width, s.meta.Height = 1, 1
	s.handlePointer(c, msg)
	if len(f.moves) != 1 || f.moves[0] != [2]int{0, 0} {
		t.Fatalf("moves = %v", f.moves)
	}
}

func TestRelativePointerRouting(t *testing.T) {
	f := &fakeHID{}
	s := NewServer(Config{HID: f})
	c := &wsClient{}
	packet := []byte{msgInputMouseRelative, 1, 0xff, 0xfe, 0, 3}
	s.handleInput(c, packet)
	s.handleInput(c, packet) // Identical deltas are distinct motion events.
	s.handleInput(c, []byte{msgInputMouseRelative, 9, 0, 0, 0, 0})
	s.handleInput(c, []byte{msgInputMouseRelative, 0, 0, 0, 0, 0})
	s.handleInput(c, packet[:5]) // Truncated input must not move or press.
	if len(f.moves) != 0 || len(f.relativeMoves) != 2 || f.relativeMoves[0] != [2]int{-2, 3} || f.relativeMoves[1] != [2]int{-2, 3} {
		t.Fatalf("absolute=%v relative=%v", f.moves, f.relativeMoves)
	}
	if len(f.buttons) != 2 || f.buttons[0] != [2]int{1, 1} || f.buttons[1] != [2]int{1, 0} || len(f.wheels) != 1 || f.wheels[0] != 1 {
		t.Fatalf("buttons=%v wheels=%v", f.buttons, f.wheels)
	}
}
