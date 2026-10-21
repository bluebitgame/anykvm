package vnc

import (
	"encoding/binary"
	"fmt"
	"image"
	"testing"
)

func TestFrameUpdateRectangleCount(t *testing.T) {
	for _, count := range []int{0, 65534, 65535, 65536, 129600} {
		for _, cursor := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cursor=%t", count, cursor), func(t *testing.T) {
				c, conn := newJPEGWireClient(encTight, 16, 16)
				r := rectangle{x: 0, y: 0, w: 16, h: 16}
				rects := make([]rectangle, count)
				for i := range rects {
					rects[i] = r
				}
				frame := &frameData{width: 16, height: 16, tightFills: map[rectangle]uint32{r: 0x123456}}
				// Include a copy before the pixels to verify ordering across updates.
				frame.copies = []copyRectangle{{image.Rect(0, 0, 16, 16), image.Rect(0, 0, 16, 16)}}
				var curEnc int32
				if cursor {
					curEnc = encCursor
				}
				if !c.sendFrameUpdate(encTight, curEnc, nil, 0, 0, 0, 0, frame, defaultPixelFormat(), rects) {
					t.Fatal("send failed")
				}
				wire := conn.wire.Bytes()
				want := count + 1
				if cursor {
					want++
				}
				seen := 0
				for len(wire) > 0 {
					if len(wire) < 4 || wire[0] != msgFramebufferUpdate {
						t.Fatal("invalid update header")
					}
					n := int(binary.BigEndian.Uint16(wire[2:4]))
					if n != min(want-seen, 65535) {
						t.Fatalf("rectangle count %d at %d", n, seen)
					}
					wire = wire[4:]
					for i := 0; i < n; i++ {
						if len(wire) < 12 {
							t.Fatal("truncated rectangle")
						}
						enc := int32(binary.BigEndian.Uint32(wire[8:12]))
						wire = wire[12:]
						expected := int32(encTight)
						copyIndex := 0
						if cursor {
							copyIndex = 1
						}
						if cursor && seen == 0 {
							expected = encCursor
						} else if seen == copyIndex {
							expected = encCopyRect
						}
						if enc != expected {
							t.Fatalf("encoding %d at %d, want %d", enc, seen, expected)
						}
						if enc != encCursor {
							if len(wire) < 4 {
								t.Fatal("truncated payload")
							}
							if enc == encTight && wire[0] != tightFill {
								t.Fatal("invalid fill")
							}
							wire = wire[4:]
						}
						seen++
					}
				}
				if seen != want {
					t.Fatalf("received %d rectangles, want %d", seen, want)
				}
			})
		}
	}
}
