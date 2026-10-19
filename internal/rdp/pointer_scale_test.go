package rdp

import (
	"bytes"
	"image"
	"testing"

	"anykvm/internal/capture"
)

func TestLargePointerNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flags  uint16
		buffer uint32
		fast   bool
		want   bool
	}{
		{"retina", 1, 38055, true, true},
		{"both-sizes", 3, 608299, true, true},
		{"baseline", 0, 38055, true, false},
		{"no-fragment-buffer", 1, 0, true, false},
		{"short-buffer", 1, 38054, true, false},
		{"no-fast-path", 1, 38055, false, false},
		{"384-only", 2, 608299, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			general := make([]byte, 20)
			if tc.fast {
				le.PutUint16(general[10:], 1)
			}
			// Deliberately place the dependent capabilities in reverse order.
			caps := join(capability(27, u16(int(tc.flags))), capability(26, u32(tc.buffer)), capability(1, general))
			p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(caps)+4), u16(3), u16(0), caps))
			got, err := confirmActiveCapabilities(p)
			if err != nil || got.largePointer != tc.want {
				t.Fatalf("caps=%+v err=%v", got, err)
			}
			conn := &pointerRecordingConn{packets: make(chan []byte, 2)}
			c := &client{s: &Server{}, conn: conn}
			if err := c.handleShare(p); err != nil {
				t.Fatal(err)
			}
			limit := 32
			if tc.want {
				limit = 96
			}
			if c.cursorLimit != limit {
				t.Fatalf("activation cursor limit=%d want=%d", c.cursorLimit, limit)
			}
		})
	}
	for _, caps := range [][]byte{capability(26, u16(1)), capability(27, []byte{1})} {
		p := shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(caps)+4), u16(1), u16(0), caps))
		if _, err := confirmActiveCapabilities(p); err == nil {
			t.Fatal("accepted truncated large-pointer capability")
		}
	}
}

func TestLargePointerBoundsAndHotspot(t *testing.T) {
	cs := &capture.CursorState{Width: 256, Height: 128, HotX: 999, HotY: -1, Image: make([]byte, 256*128*4)}
	for _, alpha := range []bool{false, true} {
		p, err := scaledCursorPointer(cs, alpha, 96, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		a := p[22:]
		if alpha {
			a = a[2:]
		}
		if le.Uint16(a[6:]) != 96 || le.Uint16(a[8:]) != 48 || le.Uint16(a[2:]) != 95 || le.Uint16(a[4:]) != 0 {
			t.Fatal("large cursor cap distorted aspect ratio or hotspot")
		}
		if len(a) != 14+int(le.Uint16(a[10:]))+int(le.Uint16(a[12:])) {
			t.Fatal("mask lengths do not cover pointer data")
		}
	}
}

type nativeCursorFixture struct {
	cursorFixture
	nativeCalls int
}

func (f *nativeCursorFixture) CaptureNative() (*capture.CursorState, error) {
	f.nativeCalls++
	return f.state, f.err
}

func TestRetinaCursorFollowsOutputGeometry(t *testing.T) {
	// A 24x32 point cursor captured on a 2x desktop is 48x64 pixels.
	cs := &capture.CursorState{Width: 48, Height: 64, HotX: 8, HotY: 12,
		Image: make([]byte, 48*64*4), SourceWidth: 3840, SourceHeight: 2160}
	for _, tc := range []struct {
		name                       string
		w, h, limit                int
		rect                       image.Rectangle
		wantW, wantH, wantX, wantY int
	}{
		{"native-2x", 3840, 2160, 96, image.Rectangle{}, 48, 64, 8, 12},
		{"downscaled-1x", 1920, 1080, 96, image.Rectangle{}, 24, 32, 4, 6},
		{"letterboxed", 1920, 1200, 96, image.Rect(0, 60, 1920, 1140), 24, 32, 4, 6},
		{"capture-content-scale", 1920, 1080, 96, image.Rect(160, 90, 1760, 990), 20, 27, 3, 5},
		{"fractional-0.7x", 2688, 1512, 96, image.Rectangle{}, 34, 45, 5, 8},
		{"fractional-0.75x", 2880, 1620, 96, image.Rectangle{}, 36, 48, 6, 9},
		{"fractional-0.8x", 3072, 1728, 96, image.Rectangle{}, 38, 51, 6, 9},
		{"no-enlargement-1.5x", 5760, 3240, 96, image.Rectangle{}, 48, 64, 8, 12},
		{"no-enlargement-2x", 7680, 4320, 96, image.Rectangle{}, 48, 64, 8, 12},
		{"reduce-quarter", 960, 540, 96, image.Rectangle{}, 12, 16, 2, 3},
		{"reduce-sixth", 640, 360, 96, image.Rectangle{}, 8, 11, 1, 2},
		{"reduce-eighth", 480, 270, 96, image.Rectangle{}, 6, 8, 1, 1},
		{"reduce-fifth", 768, 432, 96, image.Rectangle{}, 10, 13, 1, 2},
		{"reduce-third", 1280, 720, 96, image.Rectangle{}, 16, 21, 2, 3},
		{"fractional-0.4x", 1536, 864, 96, image.Rectangle{}, 19, 26, 3, 4},
		{"old-client", 3840, 2160, 32, image.Rectangle{}, 24, 32, 4, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &nativeCursorFixture{cursorFixture: cursorFixture{state: cs}}
			c := &client{s: &Server{cfg: Config{CursorCapture: fixture}, cursorOpen: true},
				done: make(chan struct{}), cursorAlpha: true, cursorLimit: tc.limit}
			c.outputSize.Store(uint64(tc.w)<<32 | uint64(tc.h))
			c.input.w, c.input.h, c.input.contentRect = tc.w, tc.h, tc.rect
			p, err := c.capturePointer()
			if err != nil {
				t.Fatal(err)
			}
			a := p[24:]
			if int(le.Uint16(a[6:])) != tc.wantW || int(le.Uint16(a[8:])) != tc.wantH ||
				int(le.Uint16(a[2:])) != tc.wantX || int(le.Uint16(a[4:])) != tc.wantY {
				t.Fatalf("incorrect pointer dimensions/hotspot: %x", a[:14])
			}
			if fixture.nativeCalls != 1 || fixture.calls != 0 {
				t.Fatal("RDP did not request native pixels")
			}
			// Stale content geometry must not override a resize. A new scale
			// updates an unchanged captured shape.
			c.outputSize.Store(uint64(960)<<32 | 540)
			p2, err := c.capturePointer()
			if err != nil || le.Uint16(p2[30:]) != 12 || le.Uint16(p2[32:]) != 16 ||
				(tc.wantW != 12 && bytes.Equal(p, p2)) {
				t.Fatalf("resize did not update unchanged cursor: %v", err)
			}
		})
	}
}

func TestLargePointerTransport(t *testing.T) {
	for _, alpha := range []bool{false, true} {
		for _, size := range []int{32, 48, 96} {
			cs := &capture.CursorState{Width: size, Height: size, HotX: size - 1, HotY: size - 2, Image: make([]byte, size*size*4)}
			for i := 0; i < len(cs.Image); i += 4 {
				cs.Image[i], cs.Image[i+3] = byte(i/4), 255
			}
			p, err := scaledCursorPointer(cs, alpha, 96, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			conn := &pointerRecordingConn{packets: make(chan []byte, 1)}
			c := &client{conn: conn}
			if err := c.sendPointer(p); err != nil {
				t.Fatal(err)
			}
			wire := <-conn.packets
			if size == 32 {
				if wire[0] != 3 {
					t.Fatal("baseline pointer did not use TPKT")
				}
				continue
			}
			var reassembled []byte
			var fragments []byte
			for len(wire) > 0 {
				if len(wire) < 6 || wire[0] != 0 || wire[1]&128 == 0 {
					t.Fatal("invalid TLS fast-path header")
				}
				n := int(wire[1]&127)<<8 | int(wire[2])
				if n > 16383 || n < 6 || n > len(wire) || int(le.Uint16(wire[4:])) != n-6 {
					t.Fatal("invalid PDU/update length")
				}
				code := byte(9)
				if alpha {
					code = 11
				}
				if wire[3]&15 != code || wire[3]&0xc0 != 0 {
					t.Fatal("incorrect pointer type/compression")
				}
				fragments = append(fragments, (wire[3]>>4)&3)
				reassembled = append(reassembled, wire[6:n]...)
				wire = wire[n:]
			}
			if !bytes.Equal(reassembled, p[22:]) {
				t.Fatal("pointer masks/hotspot changed in transport")
			}
			if size == 96 {
				if fragments[0] != 2 || fragments[len(fragments)-1] != 1 {
					t.Fatal("missing FIRST/LAST fragments")
				}
				for _, v := range fragments[1 : len(fragments)-1] {
					if v != 3 {
						t.Fatal("invalid middle fragment")
					}
				}
			} else if !bytes.Equal(fragments, []byte{0}) {
				t.Fatal("small update should use SINGLE")
			}
		}
	}
}

func TestPointerNeverEnlargesAndKeepsAlpha(t *testing.T) {
	cs := &capture.CursorState{Width: 2, Height: 2, HotX: 1, HotY: 1, Image: []byte{
		10, 20, 30, 255, 40, 50, 60, 128,
		0, 0, 0, 0, 70, 80, 90, 64,
	}}
	p, err := scaledCursorPointer(cs, true, 96, 1.6, 1.6)
	if err != nil {
		t.Fatal(err)
	}
	a := p[24:]
	if le.Uint16(a[6:]) != 2 || le.Uint16(a[8:]) != 2 || le.Uint16(a[2:]) != 1 || le.Uint16(a[4:]) != 1 {
		t.Fatal("enlargement changed captured dimensions or hotspot")
	}
	xor := a[14 : 14+int(le.Uint16(a[12:]))]
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			want := cs.Image[(y*2+x)*4:][:4]
			got := xor[((1-y)*2+x)*4:][:4]
			if !bytes.Equal(got, want) {
				t.Fatalf("pixel/Alpha changed at %d,%d: %v want %v", x, y, got, want)
			}
		}
	}
}
