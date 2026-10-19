package rdp

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Model a client that retains surfaces across RESET_GRAPHICS and rejects a
// CREATE_SURFACE whose ID is still allocated. Tolerant decoders hide this bug.
type surfaceTestConn struct {
	displayActivationConn
	fragment           []byte
	total              int
	surfaceW, surfaceH int
	outputW, outputH   int
	creates, deletes   int
	frames             chan struct{}
}

func (c *surfaceTestConn) Write(packet []byte) (int, error) {
	p, _, err := readPacket(bytes.NewReader(packet))
	if err != nil {
		return 0, err
	}
	_, p, err = takePer(p[9:])
	if err != nil || len(p) < 10 || le.Uint32(p[4:]) != 3 {
		return 0, fmt.Errorf("invalid static channel packet")
	}
	p = p[8:]
	if p[0] == 0x28 {
		c.total, c.fragment = int(le.Uint32(p[2:])), nil
		p = p[6:]
	} else {
		p = p[2:]
	}
	if c.total != 0 {
		c.fragment = append(c.fragment, p...)
		if len(c.fragment) < c.total {
			return len(packet), nil
		}
		p, c.total = c.fragment, 0
	}
	var gfx []byte
	if p[0] == 0xe0 {
		gfx = p[2:]
	} else {
		count := int(le.Uint16(p[1:]))
		p = p[7:]
		for i := 0; i < count; i++ {
			n := int(le.Uint32(p))
			p = p[4:]
			gfx = append(gfx, p[1:n]...)
			p = p[n:]
		}
	}
	for len(gfx) > 0 {
		n, kind := int(le.Uint32(gfx[4:])), le.Uint16(gfx)
		body := gfx[8:n]
		switch kind {
		case 0xe:
			c.outputW, c.outputH = int(le.Uint32(body)), int(le.Uint32(body[4:]))
		case 9:
			if c.surfaceW != 0 {
				return 0, fmt.Errorf("duplicate CREATE_SURFACE: existing=%dx%d requested=%dx%d", c.surfaceW, c.surfaceH, le.Uint16(body[2:]), le.Uint16(body[4:]))
			}
			c.surfaceW, c.surfaceH = int(le.Uint16(body[2:])), int(le.Uint16(body[4:]))
			c.creates++
		case 0xa:
			if c.surfaceW == 0 {
				return 0, fmt.Errorf("DELETE_SURFACE without an allocated surface")
			}
			c.surfaceW, c.surfaceH = 0, 0
			c.deletes++
		case 0xf:
			if c.surfaceW != c.outputW || c.surfaceH != c.outputH {
				return 0, fmt.Errorf("mapped surface dimensions differ from graphics output")
			}
		case 1, 2:
			if c.surfaceW == 0 {
				return 0, fmt.Errorf("bitmap sent before CREATE_SURFACE")
			}
		case 0xc:
			c.frames <- struct{}{}
		}
		gfx = gfx[n:]
	}
	return len(packet), nil
}

func TestFirstFrameAndResizeSurfaceLifecycle(t *testing.T) {
	for _, codec := range []displayCodec{codecClear, codecPlanar, codecProgressive} {
		for _, fixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fixed=%t", codec, fixed), func(t *testing.T) {
				conn := &surfaceTestConn{frames: make(chan struct{}, 4)}
				c := channelTestClient()
				c.conn, c.codec, c.sent, c.suspendAck = conn, codec, 0, true
				c.data.width, c.data.height = 1920, 1080
				if fixed {
					c.data.width, c.data.height = 1976, 952
				}
				c.frames = make(chan bridge.Frame, 1)
				defer func() { c.close(); c.s.wg.Wait() }()
				if err := c.activate(); err != nil {
					t.Fatal(err)
				}
				push := func(w, h int, color byte) {
					t.Helper()
					c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: bytes.Repeat([]byte{color, 20, 30, 255}, w*h)}
					select {
					case <-conn.frames:
					case <-c.done:
						t.Fatal("client rejected the graphics stream")
					case <-time.After(10 * time.Second):
						t.Fatal("first frame or resized frame did not arrive")
					}
				}
				push(1920, 1080, 10)
				push(1920, 1080, 11)
				c.setOutputSize(1280, 720)
				push(1280, 720, 12)
				c.close()
				c.s.wg.Wait()
				want := 2
				if conn.creates != want || conn.deletes != want-1 || conn.surfaceW != 1280 || conn.surfaceH != 720 {
					t.Fatalf("surface lifecycle: creates=%d deletes=%d final=%dx%d", conn.creates, conn.deletes, conn.surfaceW, conn.surfaceH)
				}
			})
		}
	}
}
