package vnc

import (
	"bytes"
	"encoding/binary"
	"image/jpeg"
	"io"
	"net"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

// Retain the bytes written by sendFrame without requiring a network reader.
type jpegWireConn struct {
	net.Conn
	wire bytes.Buffer
	fail bool
}

func (c *jpegWireConn) Write(p []byte) (int, error) {
	if c.fail {
		return 0, io.ErrClosedPipe
	}
	return c.wire.Write(p)
}
func (*jpegWireConn) SetWriteDeadline(time.Time) error { return nil }
func (*jpegWireConn) Close() error                     { return nil }
func (*jpegWireConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

type jpegWireRect struct {
	r    rectangle
	data []byte
	fill bool
	rgb  [3]byte
}

// Parse the actual RFB framing and independently decode each rectangle's JPEG.
func readJPEGWire(t *testing.T, conn *jpegWireConn, enc int32) []jpegWireRect {
	t.Helper()
	wire := conn.wire.Bytes()
	defer conn.wire.Reset()
	if len(wire) < 4 || wire[0] != msgFramebufferUpdate {
		t.Fatalf("missing update header: %x", wire)
	}
	count := int(binary.BigEndian.Uint16(wire[2:4]))
	wire = wire[4:]
	var rects []jpegWireRect
	for i := 0; i < count; i++ {
		if len(wire) < 12 || int32(binary.BigEndian.Uint32(wire[8:12])) != enc {
			t.Fatalf("invalid rectangle header: %x", wire)
		}
		r := rectangle{int(binary.BigEndian.Uint16(wire)), int(binary.BigEndian.Uint16(wire[2:])), int(binary.BigEndian.Uint16(wire[4:])), int(binary.BigEndian.Uint16(wire[6:]))}
		wire = wire[12:]
		n := 0
		if enc == encTight {
			if len(wire) >= 4 && wire[0] == tightFill {
				rects = append(rects, jpegWireRect{r: r, fill: true, rgb: [3]byte{wire[1], wire[2], wire[3]}})
				wire = wire[4:]
				continue
			}
			if len(wire) < 2 || wire[0] != tightJPEG {
				t.Fatal("missing Tight JPEG control byte")
			}
			wire = wire[1:]
			for j := 0; j < 3; j++ {
				if len(wire) == 0 {
					t.Fatal("truncated compact length")
				}
				b := wire[0]
				wire = wire[1:]
				if j == 2 {
					n |= int(b) << 14
				} else {
					n |= int(b&127) << (j * 7)
				}
				if j == 2 || b&128 == 0 {
					break
				}
			}
		} else {
			end := bytes.Index(wire, []byte{0xff, 0xd9})
			if end < 0 {
				t.Fatal("missing JPEG end marker")
			}
			n = end + 2
		}
		if n > len(wire) {
			t.Fatal("truncated JPEG")
		}
		data := append([]byte(nil), wire[:n]...)
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != r.w || img.Bounds().Dy() != r.h {
			t.Fatalf("JPEG size %v differs from rectangle %+v", img.Bounds(), r)
		}
		rects = append(rects, jpegWireRect{r: r, data: data})
		wire = wire[n:]
	}
	if len(wire) != 0 {
		t.Fatalf("%d trailing wire bytes", len(wire))
	}
	return rects
}

func newJPEGWireClient(enc int32, w, h uint16) (*client, *jpegWireConn) {
	conn := &jpegWireConn{}
	return &client{conn: conn, done: make(chan struct{}), encodings: []int32{enc, encJPEGQualityHigh}, jpegQuality: 90,
		width: w, height: h, pixelFmt: defaultPixelFormat()}, conn
}

func TestJPEGDirtyRectDelivery(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			const w, h = 193, 131
			c, conn := newJPEGWireClient(enc, w, h)
			frame := &frameData{format: capture.FormatRaw, width: w, height: h, data: make([]byte, w*h*4)}
			c.sendFrame(frame)
			got := readJPEGWire(t, conn, enc)
			if len(got) != 1 || got[0].r != (rectangle{0, 0, w, h}) {
				t.Fatalf("initial refresh: %+v", got)
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 || !c.pendingReq {
				t.Fatal("unchanged frame sent or pending request lost")
			}

			// Adjacent changed blocks merge, and the bottom-right edge is clipped.
			for _, xy := range [][2]int{{65, 65}, {129, 65}, {192, 130}} {
				frame.data[(xy[1]*w+xy[0])*4+2] = 255
			}
			c.sendFrame(frame)
			got = readJPEGWire(t, conn, enc)
			if len(got) != 2 || got[0].r != (rectangle{64, 64, 128, 64}) || got[1].r != (rectangle{192, 128, 1, 3}) {
				t.Fatalf("dirty rectangles: %+v", got)
			}
			// Inspect decoded content at the local coordinate of the changed pixel.
			img, _ := jpeg.Decode(bytes.NewReader(got[0].data))
			r, _, _, _ := img.At(1, 1).RGBA()
			if r == 0 {
				t.Fatal("rectangle encoded pixels from the wrong source position")
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 {
				t.Fatal("updated baseline resent unchanged pixels")
			}
			c.needFull = true
			c.sendFrame(frame)
			got = readJPEGWire(t, conn, enc)
			area := 0
			for _, part := range got {
				area += part.r.w * part.r.h
			}
			if area != w*h {
				t.Fatal("explicit full refresh suppressed")
			}
			frame.width, frame.height = 65, 67
			frame.data = make([]byte, 65*67*4)
			c.sendFrame(frame)
			got = readJPEGWire(t, conn, enc)
			if len(got) != 1 || got[0].r != (rectangle{0, 0, 65, 67}) {
				t.Fatal("resized framebuffer did not refresh")
			}
		})
	}
}

func TestJPEGDirtyToleranceBaseline(t *testing.T) {
	c, conn := newJPEGWireClient(encTight, 128, 64)
	c.pixelTol = 5
	frame := &frameData{format: capture.FormatRaw, width: 128, height: 64, data: make([]byte, 128*64*4)}
	c.sendFrame(frame)
	readJPEGWire(t, conn, encTight)
	frame.data[0] = 4
	frame.data[64*4] = 6
	c.sendFrame(frame)
	got := readJPEGWire(t, conn, encTight)
	if len(got) != 1 || got[0].r != (rectangle{64, 0, 64, 64}) || c.prevFrame[0] != 0 {
		t.Fatal("unchanged tile baseline overwritten")
	}
	frame.data[0] = 6
	c.sendFrame(frame)
	got = readJPEGWire(t, conn, encTight)
	if len(got) != 1 || got[0].r != (rectangle{0, 0, 64, 64}) {
		t.Fatal("cumulative tolerance drift missed")
	}
}

func TestMJPEGIncrementalKeepsRawBaselineSeparate(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			c, conn := newJPEGWireClient(enc, 65, 67)
			pixels := make([]byte, 65*67*4)
			c.sendFrame(&frameData{format: capture.FormatRaw, width: 65, height: 67, data: pixels})
			readJPEGWire(t, conn, enc)
			data, err := encodeJPEG(pixels, 65, 67, 31)
			if err != nil {
				t.Fatal(err)
			}
			frame := &frameData{format: capture.FormatJPEG, width: 65, height: 67, data: data}
			c.sendFrame(frame)
			got := readJPEGWire(t, conn, enc)
			area := 0
			for _, tile := range got {
				area += tile.r.w * tile.r.h
			}
			if area != 65*67 || c.prevFrame != nil {
				t.Fatal("MJPEG refresh coverage or raw baseline is incorrect")
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 || !c.pendingReq {
				t.Fatal("static MJPEG was resent or request lost")
			}
			c.sendFrame(&frameData{format: capture.FormatRaw, width: 65, height: 67, data: pixels})
			if got := readJPEGWire(t, conn, enc); len(got) != 1 || got[0].r != (rectangle{0, 0, 65, 67}) {
				t.Fatal("switching back to raw pixels missed full refresh")
			}
		})
	}
}

func TestTightJPEGWideRefresh(t *testing.T) {
	const w, h = 2051, 257
	c, conn := newJPEGWireClient(encTight, w, h)
	pixels := make([]byte, w*h*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i] = byte(i / 4)
	}
	c.sendFrame(&frameData{format: capture.FormatRaw, width: w, height: h, data: pixels})
	got := readJPEGWire(t, conn, encTight)
	want := []rectangle{{0, 0, 2048, 256}, {2048, 0, 3, 256}, {0, 256, 2048, 1}, {2048, 256, 3, 1}}
	if len(got) != len(want) {
		t.Fatalf("wide refresh: got %d rectangles", len(got))
	}
	for i := range want {
		if got[i].r != want[i] {
			t.Fatalf("rectangle %d: %+v, want %+v", i, got[i].r, want[i])
		}
	}
}

func TestJPEGFailedWriteKeepsBaseline(t *testing.T) {
	c, conn := newJPEGWireClient(encTight, 64, 64)
	frame := &frameData{format: capture.FormatRaw, width: 64, height: 64, data: make([]byte, 64*64*4)}
	c.sendFrame(frame)
	readJPEGWire(t, conn, encTight)
	frame.data[0] = 255
	conn.fail = true
	c.sendFrame(frame)
	if c.prevFrame[0] != 0 || !c.closing {
		t.Fatal("failed write updated baseline or left connection open")
	}
}

// A static picture must not consume the only outstanding update request.
func TestJPEGRequestSurvivesUnchangedFrame(t *testing.T) {
	sender, receiver := net.Pipe()
	c := &client{conn: sender, done: make(chan struct{}), wake: make(chan struct{}, 1),
		encodings: []int32{encTight, encJPEGQualityHigh}, pixelFmt: defaultPixelFormat(), width: 128, height: 64}
	finished := make(chan struct{})
	go func() { defer close(finished); c.writeLoop() }()
	defer func() { c.close(); receiver.Close(); <-finished }()
	readRect := func() rectangle {
		t.Helper()
		receiver.SetReadDeadline(time.Now().Add(2 * time.Second))
		var header [17]byte
		if _, err := io.ReadFull(receiver, header[:]); err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(header[2:4]) != 1 || int32(binary.BigEndian.Uint32(header[12:16])) != encTight || (header[16] != tightJPEG && header[16] != tightFill) {
			t.Fatalf("invalid Tight update: %x", header)
		}
		n := 3
		if header[16] == tightJPEG {
			n = 0
			for j := 0; j < 3; j++ {
				var b [1]byte
				if _, err := io.ReadFull(receiver, b[:]); err != nil {
					t.Fatal(err)
				}
				if j == 2 {
					n |= int(b[0]) << 14
				} else {
					n |= int(b[0]&127) << (j * 7)
				}
				if j == 2 || b[0]&128 == 0 {
					break
				}
			}
		}
		if _, err := io.CopyN(io.Discard, receiver, int64(n)); err != nil {
			t.Fatal(err)
		}
		return rectangle{int(binary.BigEndian.Uint16(header[4:])), int(binary.BigEndian.Uint16(header[6:])), int(binary.BigEndian.Uint16(header[8:])), int(binary.BigEndian.Uint16(header[10:]))}
	}
	pixels := make([]byte, 128*64*4)
	frame := &frameData{format: capture.FormatRaw, width: 128, height: 64, data: pixels}
	c.onUpdateRequest(false)
	c.offerFrame(frame)
	readRect()
	c.onUpdateRequest(true)
	processed := make(chan struct{})
	owned := framebuf.New(pixels, func() { close(processed) })
	unchanged := *frame
	unchanged.buffer = owned
	c.offerFrame(&unchanged)
	owned.Release()
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("unchanged frame was not processed")
	}
	// No further request: the earlier incremental request must still be armed.
	changed := *frame
	changed.data = append([]byte(nil), pixels...)
	changed.data[65*4] = 255
	c.offerFrame(&changed)
	if got := readRect(); got != (rectangle{64, 0, 64, 64}) {
		t.Fatalf("update after idle: %+v", got)
	}
}
