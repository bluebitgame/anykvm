package vnc

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"anykvm/internal/capture"
)

// fakeAU builds a minimal Annex-B access unit: for the first frame an SPS+PPS+
// IDR, otherwise a single non-IDR slice. Content is not a valid bitstream but is
// sufficient to exercise the server's framing/delivery path.
func fakeAU(i int) []byte {
	sc := func(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }
	if i == 0 {
		return append(append(sc(0x67, 0x42, 0x00, 0x0a), sc(0x68, 0xce, 0x38, 0x80)...), sc(0x65, byte(i))...)
	}
	return sc(0x41, byte(i))
}

// TestH264DeliveryContinuous verifies every H.264 frame pushed to the server is
// delivered to the client (no coalescing/drops), reproducing the "stuck on one
// frame" scenario if delivery is broken.
func TestH264DeliveryContinuous(t *testing.T) {
	testH264Delivery(t, true)
}

func TestH264DeliveryRequestDriven(t *testing.T) {
	testH264Delivery(t, false)
}

func testH264Delivery(t *testing.T, forceContinuous bool) {
	srv := NewServer(Config{Addr: "127.0.0.1:0", Width: 64, Height: 48, ForceContinuous: forceContinuous})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	cli := &pipeClient{t: t, conn: conn}
	cli.handshake()
	cli.setEncodings(encH264)
	cli.framebufferUpdateRequest(false)

	time.Sleep(100 * time.Millisecond)

	const n = 40
	go func() {
		for i := 0; i < n; i++ {
			srv.PushFrame(Frame{Format: capture.FormatH264, Data: fakeAU(i), Width: 64, Height: 48})
			time.Sleep(3 * time.Millisecond)
		}
	}()

	got := 0
	for got < n {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		au, ok := cli.readH264Update()
		if !ok {
			break
		}
		if len(au) == 0 {
			continue
		}
		got++
		if !forceContinuous {
			// Request-driven client re-requests after each update.
			cli.framebufferUpdateRequest(true)
		}
	}
	if got != n {
		t.Fatalf("forceContinuous=%v: received %d H.264 updates, want %d", forceContinuous, got, n)
	}
}

type pipeClient struct {
	t    *testing.T
	conn net.Conn
}

func (p *pipeClient) mustRead(n int) []byte {
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.conn, buf); err != nil {
		p.t.Fatalf("read %d: %v", n, err)
	}
	return buf
}

func (p *pipeClient) handshake() {
	p.mustRead(12)                     // ProtocolVersion
	p.conn.Write([]byte(protoVersion)) // echo
	sec := p.mustRead(2)               // [count, types...]
	_ = sec                            // assume None offered
	p.conn.Write([]byte{secTypeNone})  // choose None
	p.mustRead(4)                      // SecurityResult
	p.conn.Write([]byte{1})            // shared flag
	hdr := p.mustRead(24)              // ServerInit fixed part
	nameLen := int(binary.BigEndian.Uint32(hdr[20:24]))
	if nameLen > 0 {
		p.mustRead(nameLen)
	}
}

func (p *pipeClient) setEncodings(encs ...int32) {
	buf := make([]byte, 4+4*len(encs))
	buf[0] = msgSetEncodings
	binary.BigEndian.PutUint16(buf[2:], uint16(len(encs)))
	for i, e := range encs {
		binary.BigEndian.PutUint32(buf[4+4*i:], uint32(e))
	}
	p.conn.Write(buf)
}

func (p *pipeClient) framebufferUpdateRequest(incremental bool) {
	buf := make([]byte, 10)
	buf[0] = msgFramebufferUpdateRequest
	if incremental {
		buf[1] = 1
	}
	p.conn.Write(buf)
}

// readH264Update reads one server message. For a FramebufferUpdate carrying an
// Open H.264 rectangle it returns the access-unit bytes; other server messages
// (e.g. EndOfContinuousUpdates) return (nil, true) and are skipped.
func (p *pipeClient) readH264Update() ([]byte, bool) {
	var mt [1]byte
	if _, err := io.ReadFull(p.conn, mt[:]); err != nil {
		return nil, false
	}
	switch mt[0] {
	case msgEndOfContinuousUpdates:
		return nil, true
	case msgFramebufferUpdate:
		hdr := make([]byte, 3)
		if _, err := io.ReadFull(p.conn, hdr); err != nil {
			return nil, false
		}
		nrects := int(binary.BigEndian.Uint16(hdr[1:3]))
		var last []byte
		for r := 0; r < nrects; r++ {
			rh := make([]byte, 12)
			if _, err := io.ReadFull(p.conn, rh); err != nil {
				return nil, false
			}
			enc := int32(binary.BigEndian.Uint32(rh[8:12]))
			if enc != encH264 {
				return nil, false
			}
			body := make([]byte, 8)
			if _, err := io.ReadFull(p.conn, body); err != nil {
				return nil, false
			}
			dlen := int(binary.BigEndian.Uint32(body[0:4]))
			data := make([]byte, dlen)
			if _, err := io.ReadFull(p.conn, data); err != nil {
				return nil, false
			}
			last = data
		}
		return last, true
	default:
		return nil, false
	}
}
