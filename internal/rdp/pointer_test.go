package rdp

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestDemandActiveInputAndPointerCapabilities(t *testing.T) {
	p := demandActive(640, 480)
	off := 14 + int(le.Uint16(p[10:]))
	count := int(le.Uint16(p[off:]))
	p = p[off+4:]
	input, pointer, large, fragment, fast := false, false, false, false, false
	for n := 0; n < count; n++ {
		kind, size := le.Uint16(p), int(le.Uint16(p[2:]))
		body := p[4:size]
		switch kind {
		case 1:
			fast = le.Uint16(body[10:])&1 != 0
		case 26:
			fragment = le.Uint32(body) >= 38055
		case 27:
			large = le.Uint16(body) == 1
		case 13:
			input = true
			if flags := le.Uint16(body); flags&0x31 != 0x31 {
				t.Fatalf("Unicode/scancode/fast-path capabilities missing: %#x", flags)
			}
			for _, v := range body[2:] {
				if v != 0 {
					t.Fatal("server specified client-only keyboard fields")
				}
			}
		case 8:
			pointer = true
			if le.Uint16(body) != 1 || le.Uint16(body[2:]) < 1 || le.Uint16(body[4:]) < 1 {
				t.Fatal("missing color pointer capability/cache")
			}
		}
		p = p[size:]
	}
	if !input || !pointer || !large || !fragment || !fast || len(p) != 4 {
		t.Fatal("input or pointer capabilities absent")
	}
}

func TestCapturedPointerPixels(t *testing.T) {
	cs := &capture.CursorState{Width: 3, Height: 2, HotX: 2, HotY: 1, Image: []byte{
		13, 29, 61, 255, 0, 0, 0, 0, 64, 32, 16, 128,
		0, 0, 0, 255, 3, 5, 7, 64, 31, 63, 127, 255,
	}}
	for _, alpha := range []bool{false, true} {
		p, err := cursorPointer(cs, alpha)
		if err != nil {
			t.Fatal(err)
		}
		a := p[22:]
		bpp, kind := 3, uint16(6)
		if alpha {
			bpp, kind = 4, 8
			if le.Uint16(a) != 32 {
				t.Fatal("not a 32-bpp pointer")
			}
			a = a[2:]
		}
		if le.Uint16(p[18:]) != kind || le.Uint16(a[2:]) != 2 || le.Uint16(a[4:]) != 1 {
			t.Fatal("wrong type or hotspot")
		}
		xorStride, andStride := (3*bpp+1)&^1, 2
		xorLen := int(le.Uint16(a[12:]))
		if xorLen != xorStride*2 || le.Uint16(a[10:]) != 4 || len(a) != 14+xorLen+4 {
			t.Fatal("odd-width mask alignment or lengths invalid")
		}
		xor, and := a[14:14+xorLen], a[14+xorLen:]
		for y := 0; y < 2; y++ {
			for x := 0; x < 3; x++ {
				src, dst := (y*3+x)*4, (1-y)*xorStride+x*bpp
				transparent := cs.Image[src+3] == 0 || (!alpha && cs.Image[src+3] < 128)
				if got := and[(1-y)*andStride+x/8]&(0x80>>(x%8)) != 0; got != transparent {
					t.Fatalf("wrong transparency at (%d,%d)", x, y)
				}
				if transparent {
					for _, v := range xor[dst : dst+bpp] {
						if v != 0 {
							t.Fatal("transparent XOR pixel would invert background")
						}
					}
				} else {
					if !bytes.Equal(xor[dst:dst+3], cs.Image[src:src+3]) {
						t.Fatal("BGRA colors or row order changed")
					}
					if alpha && xor[dst+3] != cs.Image[src+3] {
						t.Fatal("partial alpha lost")
					}
				}
			}
		}
	}
	cs.Premultiplied = true
	p, err := cursorPointer(cs, true)
	if err != nil {
		t.Fatal(err)
	}
	// Source pixel (2,0) is premultiplied BGRA 64,32,16 at alpha 128.
	off := 24 + 14 + 12 + 8
	if !bytes.Equal(p[off:off+4], []byte{128, 64, 32, 128}) {
		t.Fatal("premultiplied alpha was not converted", p[off:off+4])
	}
}

func TestPointerBoundsScalingAndHiddenState(t *testing.T) {
	cs := &capture.CursorState{Width: 64, Height: 16, HotX: 40, HotY: 10, Image: make([]byte, 64*16*4)}
	p, err := cursorPointer(cs, true)
	if err != nil {
		t.Fatal(err)
	}
	a := p[24:]
	if le.Uint16(a[6:]) != 32 || le.Uint16(a[8:]) != 8 || le.Uint16(a[2:]) != 20 || le.Uint16(a[4:]) != 5 {
		t.Fatal("shape/hotspot scaling changed aspect ratio")
	}
	cs.HotX, cs.HotY = -9, 999
	p, err = cursorPointer(cs, true)
	if err != nil {
		t.Fatal(err)
	}
	if le.Uint16(p[26:]) != 0 || le.Uint16(p[28:]) != 7 {
		t.Fatal("hotspot was not clamped")
	}
	for _, invalid := range []*capture.CursorState{
		{Width: 257, Height: 1}, {Width: -1, Height: 1}, {Width: 0, Height: 1}, {Width: 2, Height: 2, Image: make([]byte, 15)},
	} {
		if _, err := cursorPointer(invalid, true); err == nil {
			t.Fatal("accepted invalid cursor", invalid)
		}
	}
	p, err = cursorPointer(&capture.CursorState{}, true)
	if err != nil || len(p) != 26 || le.Uint16(p[18:]) != 1 || le.Uint32(p[22:]) != 0 {
		t.Fatal("empty cursor not hidden")
	}
	p, err = cursorPointer(nil, true)
	if err != nil || !bytes.Equal(p, arrowPointer()) {
		t.Fatal("missing capture did not use arrow fallback")
	}
}

func TestPointerCapabilityNegotiation(t *testing.T) {
	for _, tc := range []struct {
		payload []byte
		alpha   bool
	}{
		{join(u16(1), u16(20)), false},
		{join(u16(1), u16(20), u16(0)), false},
		{join(u16(1), u16(20), u16(1)), true},
	} {
		caps := capability(8, tc.payload)
		p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(caps)+4), u16(1), u16(0), caps))
		alpha, err := confirmActivePointers(p)
		if err != nil || alpha != tc.alpha {
			t.Fatalf("alpha=%t want=%t err=%v", alpha, tc.alpha, err)
		}
	}
	caps := capability(8, u16(1))
	p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(caps)+4), u16(1), u16(0), caps))
	if _, err := confirmActivePointers(p); err == nil {
		t.Fatal("accepted truncated pointer capability")
	}
}

type cursorFixture struct {
	mu            sync.Mutex
	state         *capture.CursorState
	err           error
	calls, closes int
	observed      chan struct{}
}

func (*cursorFixture) Open() error    { return nil }
func (f *cursorFixture) Close() error { f.mu.Lock(); defer f.mu.Unlock(); f.closes++; return nil }
func (f *cursorFixture) Capture() (*capture.CursorState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.observed != nil {
		select {
		case f.observed <- struct{}{}:
		default:
		}
	}
	return f.state, f.err
}
func (f *cursorFixture) set(state *capture.CursorState, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.err = state, err
	for {
		select {
		case <-f.observed:
		default:
			return
		}
	}
}

type pointerRecordingConn struct {
	net.Conn
	packets chan []byte
}

func (c *pointerRecordingConn) Write(p []byte) (int, error) {
	c.packets <- bytes.Clone(p)
	return len(p), nil
}
func (*pointerRecordingConn) SetWriteDeadline(time.Time) error { return nil }
func (*pointerRecordingConn) Close() error                     { return nil }

func TestCursorLoopChangesReconnectAndCleanup(t *testing.T) {
	fixture := &cursorFixture{state: &capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}}, observed: make(chan struct{}, 32)}
	s := &Server{cfg: Config{CursorCapture: fixture}, cursorOpen: true}
	start := func() (*client, *pointerRecordingConn, chan struct{}) {
		conn := &pointerRecordingConn{packets: make(chan []byte, 16)}
		c := &client{s: s, conn: conn, done: make(chan struct{}), cursorAlpha: true}
		stopped := make(chan struct{})
		go func() { c.cursorLoop(); close(stopped) }()
		return c, conn, stopped
	}
	next := func(conn *pointerRecordingConn) []byte {
		t.Helper()
		select {
		case packet := <-conn.packets:
			p, fast, err := readPacket(bytes.NewReader(packet))
			if err != nil || fast {
				t.Fatal("invalid cursor transport", err)
			}
			_, data, err := takePer(p[9:])
			if err != nil {
				t.Fatal(err)
			}
			return data
		case <-time.After(time.Second):
			t.Fatal("cursor update did not arrive without video")
			return nil
		}
	}
	stop := func(c *client, stopped chan struct{}) {
		t.Helper()
		close(c.done)
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("cursor polling did not stop")
		}
	}
	c, conn, stopped := start()
	first := next(conn)
	// Wait for another three polls; unchanged shapes must not be resent even
	// when capture reports Changed=true on every call.
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}, Changed: true}, nil)
	for n := 0; n < 4; n++ {
		select {
		case <-fixture.observed:
		case <-time.After(time.Second):
			t.Fatal("cursor did not poll")
		}
	}
	select {
	case <-conn.packets:
		t.Fatal("unchanged cursor resent")
	default:
	}
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{9, 8, 7, 255}, Changed: false}, nil)
	if bytes.Equal(first, next(conn)) {
		t.Fatal("ignored image change with Changed=false")
	}
	fixture.set(&capture.CursorState{SystemDefault: true}, nil)
	if p := next(conn); le.Uint16(p[18:]) != 1 || le.Uint32(p[22:]) != 0x7f00 {
		t.Fatal("client system default cursor did not reach client")
	}
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}}, nil)
	if !bytes.Equal(first, next(conn)) {
		t.Fatal("same cursor not restored after system default")
	}
	fixture.set(&capture.CursorState{}, nil)
	if p := next(conn); le.Uint16(p[18:]) != 1 || le.Uint32(p[22:]) != 0 {
		t.Fatal("hidden cursor did not reach client")
	}
	// Returning from a capture card must restore even the same cached shape.
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}}, nil)
	if !bytes.Equal(first, next(conn)) {
		t.Fatal("same cursor not restored after hiding")
	}
	fixture.set(nil, nil)
	if p := next(conn); !bytes.Equal(p, arrowPointer()) {
		t.Fatal("missing source did not restore fallback")
	}
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{1, 2, 3, 255}}, nil)
	if !bytes.Equal(first, next(conn)) {
		t.Fatal("cursor not restored")
	}
	stop(c, stopped)
	c2, conn2, stopped2 := start()
	if !bytes.Equal(first, next(conn2)) {
		t.Fatal("reconnect missed unchanged current cursor")
	}
	fixture.set(nil, errors.New("capture unavailable"))
	for n := 0; n < 4; n++ {
		select {
		case <-fixture.observed:
		case <-time.After(time.Second):
			t.Fatal("cursor stopped retrying capture")
		}
	}
	select {
	case <-conn2.packets:
		t.Fatal("capture error changed cursor")
	default:
	}
	fixture.set(&capture.CursorState{Width: 1, Height: 1, Image: []byte{9, 8, 7, 255}}, nil)
	_ = next(conn2)
	stop(c2, stopped2)
	s.Close()
	s.Close()
	if fixture.closes != 1 {
		t.Fatalf("cursor closed %d times", fixture.closes)
	}
}

func TestArrowPointerMasks(t *testing.T) {
	p := arrowPointer()
	if int(le.Uint16(p)) != len(p) || p[14] != 27 || le.Uint16(p[18:]) != 6 {
		t.Fatal("not a color pointer share-data PDU")
	}
	a := p[22:]
	w, h := int(le.Uint16(a[6:])), int(le.Uint16(a[8:]))
	andLen, xorLen := int(le.Uint16(a[10:])), int(le.Uint16(a[12:]))
	if w != 16 || h != 24 || le.Uint16(a) != 0 || le.Uint16(a[2:]) != 0 || le.Uint16(a[4:]) != 0 {
		t.Fatal("invalid size, cache index, or hotspot")
	}
	xorStride, andStride := (w*3+1)&^1, ((w+7)/8+1)&^1
	if andLen != andStride*h || xorLen != xorStride*h || len(a) != 14+xorLen+andLen {
		t.Fatal("invalid mask lengths")
	}
	xor, and := a[14:14+xorLen], a[14+xorLen:]
	// Decode points independently using the normative bottom-up mask format.
	for _, point := range []struct {
		x, y        int
		transparent bool
		color       byte
	}{
		{0, 0, false, 0}, {1, 2, false, 255}, {15, 0, true, 0}, {0, 23, true, 0}, {3, 15, true, 0},
	} {
		xoff, aoff := (h-1-point.y)*xorStride+point.x*3, (h-1-point.y)*andStride+point.x/8
		transparent := and[aoff]&(0x80>>(point.x%8)) != 0
		if transparent != point.transparent || xor[xoff] != point.color || xor[xoff+1] != point.color || xor[xoff+2] != point.color {
			t.Fatalf("incorrect cursor pixel (%d,%d)", point.x, point.y)
		}
	}
}

func TestFontListSendsArrowBitmap(t *testing.T) {
	conn := &dvcRecordingConn{}
	c := channelTestClient()
	c.conn = conn
	c.dvcCaps = false
	c.data.dvc = 1004
	if err := c.handleShare(shareData(39, make([]byte, 8))); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		p, fast, err := readPacket(&conn.data)
		if err != nil || fast {
			t.Fatal("invalid activation packet", err)
		}
		if n == 1 {
			_, data, err := takePer(p[9:])
			if err != nil || len(data) < 22 || data[14] != 27 || le.Uint16(data[18:]) != 6 {
				t.Fatal("activation did not send an explicit cursor shape")
			}
		}
	}
}
