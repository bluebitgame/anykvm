package vnc

import (
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type cursorSourceFixture struct {
	mu    sync.Mutex
	state *capture.CursorState
}

func (*cursorSourceFixture) Open() error  { return nil }
func (*cursorSourceFixture) Close() error { return nil }
func (s *cursorSourceFixture) Capture() (*capture.CursorState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *cursorSourceFixture) set(state *capture.CursorState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func TestVNCCursorAlphaAndLegacyWireFormats(t *testing.T) {
	state := &capture.CursorState{Width: 1, Height: 1, Image: []byte{16, 32, 64, 128}, Premultiplied: true, HotX: 9, HotY: -1}
	c := &client{encodings: []int32{encCursorWithAlpha}}
	enc, data, w, h, x, y := c.prepareCursor(state)
	if enc != encCursorWithAlpha || w != 1 || h != 1 || x != 0 || y != 0 || !bytes.Equal(data, []byte{0, 0, 0, 0, 64, 32, 16, 128}) {
		t.Fatalf("alpha wire: %d %v", enc, data)
	}
	c.encodings = []int32{encCursor}
	enc, data, _, _, _, _ = c.prepareCursor(state)
	if enc != encCursor || len(data) != 5 || !bytes.Equal(data[:3], []byte{32, 64, 128}) || data[4] != 0x80 {
		t.Fatalf("legacy alpha conversion: %v", data)
	}
	c.encodings = []int32{encXCursor}
	enc, data, _, _, _, _ = c.prepareCursor(state)
	if enc != encXCursor || len(data) != 8 || data[7] != 0x80 {
		t.Fatalf("XCursor color/source/mask: %v", data)
	}
}
func TestVNCCursorStateIsPerViewer(t *testing.T) {
	source := &cursorSourceFixture{state: &capture.CursorState{Width: 1, Height: 1, Image: []byte{10, 20, 30, 255}, Changed: false}}
	c1 := &client{cursorCap: source, encodings: []int32{encCursor}}
	c2 := &client{cursorCap: source, encodings: []int32{encCursor}}
	if _, ok := c1.captureCursorState(); !ok {
		t.Fatal("first viewer missed initial cursor")
	}
	if _, ok := c1.captureCursorState(); ok {
		t.Fatal("unchanged cursor resent")
	}
	if _, ok := c2.captureCursorState(); !ok {
		t.Fatal("new viewer missed a source-wide unchanged cursor")
	}
	source.set(&capture.CursorState{})
	if _, ok := c1.captureCursorState(); !ok {
		t.Fatal("hidden cursor suppressed")
	}
	if _, ok := c1.captureCursorState(); ok {
		t.Fatal("hidden state resent")
	}
	source.set(&capture.CursorState{SystemDefault: true})
	state, ok := c1.captureCursorState()
	if !ok {
		t.Fatal("default cursor suppressed")
	}
	enc, data, w, h, _, _ := c1.prepareCursor(state)
	if enc == 0 || len(data) == 0 || w == 0 || h == 0 {
		t.Fatal("system cursor was not represented as a default arrow")
	}
	large := &capture.CursorState{Width: 256, Height: 128, HotX: 255, HotY: 127, Image: make([]byte, 256*128*4)}
	_, _, w, h, x, y := c1.prepareCursor(large)
	if w != 128 || h != 64 || x != 127 || y != 63 {
		t.Fatal("large cursor dimensions/hotspot not reduced")
	}
}
func TestVNCCursorFollowsDesktopScaling(t *testing.T) {
	c := &client{width: 960, height: 540, encodings: []int32{encCursor}}
	state := &capture.CursorState{Width: 64, Height: 32, HotX: 20, HotY: 10, Image: make([]byte, 64*32*4), SourceWidth: 1920, SourceHeight: 1080}
	_, _, w, h, x, y := c.prepareCursor(state)
	if w != 32 || h != 16 || x != 10 || y != 5 {
		t.Fatalf("incorrect cursor scaling: %dx%d (%d,%d)", w, h, x, y)
	}
}

func TestVNCCursorUpdatesOnIdleDesktop(t *testing.T) {
	sender, receiver := net.Pipe()
	source := &cursorSourceFixture{state: &capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}}}
	c := &client{conn: sender, done: make(chan struct{}), wake: make(chan struct{}, 1), cursorCap: source, encodings: []int32{encCursor}, pendingReq: true}
	finished := make(chan struct{})
	go func() { defer close(finished); c.writeLoop() }()
	defer func() { c.close(); receiver.Close(); <-finished }()
	receiver.SetReadDeadline(time.Now().Add(time.Second))
	var header [16]byte
	if _, err := io.ReadFull(receiver, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != msgFramebufferUpdate || binary.BigEndian.Uint16(header[2:]) != 1 || int32(binary.BigEndian.Uint32(header[12:])) != encCursor {
		t.Fatalf("invalid idle cursor update: %x", header)
	}
	var pixels [5]byte
	if _, err := io.ReadFull(receiver, pixels[:]); err != nil {
		t.Fatal(err)
	}
	source.set(&capture.CursorState{})
	c.stateMu.Lock()
	c.pendingReq = true
	c.stateMu.Unlock()
	c.signalWake()
	if _, err := io.ReadFull(receiver, header[:]); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(header[8:]) != 0 || binary.BigEndian.Uint16(header[10:]) != 0 {
		t.Fatal("hide requires a zero-size cursor update")
	}
}
