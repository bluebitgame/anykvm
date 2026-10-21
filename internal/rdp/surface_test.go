package rdp

import (
	"bytes"
	"fmt"
	"image"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Model a client that retains surfaces across RESET_GRAPHICS and rejects a
// CREATE_SURFACE whose ID is still allocated. Tolerant decoders hide this bug.
type surfaceTestConn struct {
	displayActivationConn
	fragment               []byte
	total                  int
	surfaceW, surfaceH     int
	outputW, outputH       int
	creates, deletes       int
	copyCommands           int
	onGraphics             func(uint16, []byte)
	frames                 chan struct{}
	allowScaled            bool
	resets, scaledMaps     int
	surfaces               map[int]image.Point
	mapped                 map[int]image.Rectangle
	black                  map[int]bool
	progressiveInitialized bool
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
		if c.onGraphics != nil {
			c.onGraphics(kind, body)
		}
		switch kind {
		case 0xe:
			c.resets++
			c.progressiveInitialized = false
			c.outputW, c.outputH = int(le.Uint32(body)), int(le.Uint32(body[4:]))
		case 9:
			id := int(le.Uint16(body))
			size := image.Pt(int(le.Uint16(body[2:])), int(le.Uint16(body[4:])))
			if c.surfaces == nil {
				c.surfaces = make(map[int]image.Point)
				c.mapped = make(map[int]image.Rectangle)
				c.black = make(map[int]bool)
			}
			if _, exists := c.surfaces[id]; exists {
				return 0, fmt.Errorf("duplicate CREATE_SURFACE %d", id)
			}
			if id != 1 && id != progressiveMoveScratch && !c.allowScaled {
				return 0, fmt.Errorf("unexpected surface %d", id)
			}
			c.surfaces[id] = size
			if id == 1 {
				c.surfaceW, c.surfaceH = size.X, size.Y
				c.creates++
				c.progressiveInitialized = false
			}
		case 5:
			if len(body) != 18 || le.Uint16(body[12:]) != 1 {
				return 0, fmt.Errorf("invalid surface copy")
			}
			source, destination := int(le.Uint16(body)), int(le.Uint16(body[2:]))
			sw, ok := c.surfaces[source]
			if !ok {
				return 0, fmt.Errorf("missing source surface")
			}
			dw, ok := c.surfaces[destination]
			if !ok {
				return 0, fmt.Errorf("missing target surface")
			}
			x0, y0, x1, y1 := int(le.Uint16(body[4:])), int(le.Uint16(body[6:])), int(le.Uint16(body[8:])), int(le.Uint16(body[10:]))
			x, y := int(le.Uint16(body[14:])), int(le.Uint16(body[16:]))
			if x1 <= x0 || y1 <= y0 || x1 > sw.X || y1 > sw.Y || x+x1-x0 > dw.X || y+y1-y0 > dw.Y {
				return 0, fmt.Errorf("surface copy outside bounds")
			}
			c.copyCommands++
		case 0xa:
			id := int(le.Uint16(body))
			if _, exists := c.surfaces[id]; !exists {
				return 0, fmt.Errorf("DELETE_SURFACE %d without an allocated surface", id)
			}
			delete(c.surfaces, id)
			delete(c.mapped, id)
			delete(c.black, id)
			if id == 1 {
				c.surfaceW, c.surfaceH = 0, 0
				c.deletes++
				c.progressiveInitialized = false
			}
		case 0xf:
			if c.surfaceW != c.outputW || c.surfaceH != c.outputH {
				return 0, fmt.Errorf("mapped surface dimensions differ from graphics output")
			}
			c.mapped[int(le.Uint16(body))] = image.Rect(0, 0, c.outputW, c.outputH)
		case 0x17:
			if !c.allowScaled || len(body) != 20 {
				return 0, fmt.Errorf("unexpected scaled mapping")
			}
			id := int(le.Uint16(body))
			x, y := int(le.Uint32(body[4:])), int(le.Uint32(body[8:]))
			rect := image.Rect(x, y, x+int(le.Uint32(body[12:])), y+int(le.Uint32(body[16:])))
			if _, exists := c.surfaces[id]; !exists || rect.Empty() || !rect.In(image.Rect(0, 0, c.outputW, c.outputH)) {
				return 0, fmt.Errorf("invalid scaled surface %d rectangle %v", id, rect)
			}
			c.mapped[id] = rect
			c.scaledMaps++
		case 4:
			id := int(le.Uint16(body))
			if !c.allowScaled || c.surfaces[id] != image.Pt(1, 1) || !bytes.Equal(body[2:], []byte{0, 0, 0, 255, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0}) {
				return 0, fmt.Errorf("invalid black bar surface %d", id)
			}
			c.black[id] = true
		case 3:
			c.progressiveInitialized = false
		case 1, 2:
			if c.surfaceW == 0 {
				return 0, fmt.Errorf("bitmap sent before CREATE_SURFACE")
			}
			if kind == 2 {
				stream := body[13:]
				for len(stream) >= 6 {
					block, n := le.Uint16(stream), int(le.Uint32(stream[2:]))
					if n < 6 || n > len(stream) {
						return 0, fmt.Errorf("invalid Progressive block")
					}
					if block == 0xccc3 {
						c.progressiveInitialized = true
					}
					if block == 0xccc4 && !c.progressiveInitialized {
						return 0, fmt.Errorf("Progressive region without context after surface reset")
					}
					stream = stream[n:]
				}
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
				if fixed {
					want++
				} // Activation and the first source use different sizes.
				if conn.creates != want || conn.deletes != want-1 || conn.surfaceW != 1280 || conn.surfaceH != 720 {
					t.Fatalf("surface lifecycle: creates=%d deletes=%d final=%dx%d", conn.creates, conn.deletes, conn.surfaceW, conn.surfaceH)
				}
			})
		}
	}
}
