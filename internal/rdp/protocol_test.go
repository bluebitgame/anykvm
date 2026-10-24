package rdp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestAVC420Required(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions [][2]uint32
		want     uint32
	}{
		{"8.1 enabled", [][2]uint32{{0x80105, 0x10}}, 0x80105},
		{"8.1 disabled", [][2]uint32{{0x80105, 0}}, 0},
		{"10 enabled", [][2]uint32{{0xa0002, 0}}, 0xa0002},
		{"10.6 historical version enabled", [][2]uint32{{0xa0601, 0}}, 0xa0601},
		{"10.6 historical version disabled", [][2]uint32{{0xa0601, 0x20}}, 0},
		{"10.1 malformed flags layout", [][2]uint32{{0xa0100, 0}}, 0},
		{"10.7 enabled", [][2]uint32{{0xa0701, 0}}, 0xa0701},
		{"10 disabled", [][2]uint32{{0xa0002, 0x20}}, 0},
		{"legacy only", [][2]uint32{{0x80004, 0}}, 0},
		{"downgrade to AVC", [][2]uint32{{0xa0002, 0x20}, {0x80105, 0x10}}, 0x80105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := u16(len(tc.versions))
			for _, v := range tc.versions {
				p = append(p, join(u32(v[0]), u32(4), u32(v[1]))...)
			}
			got, _, err := avcCapability(p)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("version=%x error=%v", got, err)
			}
		})
	}
	for _, p := range [][]byte{{}, {1}, join(u16(1), u32(0xa0002), u32(0xffffffff)), join(u16(0), u32(0))} {
		if _, _, err := avcCapability(p); err == nil {
			t.Fatalf("accepted invalid capabilities %x", p)
		}
	}
}

func TestVersion101CapabilitiesLayout(t *testing.T) {
	valid := join(u16(1), u32(0xa0100), u32(16), make([]byte, 16))
	version, flags, err := avcCapability(valid)
	if err != nil || version != 0xa0100 || flags != 0 {
		t.Fatalf("valid version 10.1 capabilities rejected: version=%#x flags=%#x error=%v", version, flags, err)
	}
	confirm := gfxCapabilityConfirm(version, flags)
	if le.Uint16(confirm) != 0x13 || int(le.Uint32(confirm[4:])) != len(confirm) ||
		le.Uint32(confirm[8:]) != version || le.Uint32(confirm[12:]) != 16 ||
		!bytes.Equal(confirm[16:], make([]byte, 16)) {
		t.Fatalf("invalid version 10.1 confirm: %x", confirm)
	}
	bad := append([]byte(nil), valid...)
	bad[10] = 0x20
	if _, _, err := avcCapability(bad); err == nil {
		t.Fatal("accepted nonzero reserved data")
	}
}

func TestDisabledAVCDiagnosticIncludesCapabilities(t *testing.T) {
	p := join(u16(2), u32(0xa0002), u32(4), u32(0x20), u32(0x80105), u32(4), u32(2))
	_, _, err := avcCapability(p)
	if err == nil {
		t.Fatal("accepted a client that did not advertise AVC420")
	}
	for _, detail := range []string{"version=0x000a0002", "flags=0x00000020", "AVC_DISABLED", "version=0x00080105", "AVC420_ENABLED_absent"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("capability diagnostic omits %q: %v", detail, err)
		}
	}
}

func TestMobileWindowsAppAVCDisabledAdvertisement(t *testing.T) {
	// Identical capability values were reported by Android Windows App
	// 11.0.26081 and iOS Windows App in real connections on 2026-10-07.
	// The iOS application version is unknown; this does not cover every session.
	capabilities := [][2]uint32{
		{0x00080004, 0x00000002},
		{0x00080105, 0x00000002},
		{0x000a0002, 0x00000022},
		{0x000a0200, 0x00000022},
		{0x000a0301, 0x00000020},
		{0x000a0400, 0x00000022},
		{0x000b0101, 0x000001a2},
		{0x000b0300, 0x000001a2},
	}
	p := u16(len(capabilities))
	for _, capability := range capabilities {
		p = append(p, join(u32(capability[0]), u32(4), u32(capability[1]))...)
	}
	version, flags, err := avcCapability(p)
	if err == nil || version != 0 || flags != 0 {
		t.Fatalf("accepted the recorded non-AVC advertisement: version=%#x flags=%#x error=%v", version, flags, err)
	}
	if errors.Is(err, errProtocol) {
		t.Fatalf("valid capability layout rejected as malformed: %v", err)
	}
	if strings.Count(err.Error(), "AVC420=false") != len(capabilities) ||
		strings.Count(err.Error(), "(AVC_DISABLED)") != 4 ||
		!strings.Contains(err.Error(), "AVC420_ENABLED_absent") ||
		!strings.Contains(err.Error(), "version=0x000b0300") {
		t.Fatalf("incomplete recorded capability diagnostic: %v", err)
	}
}

func testInfo(user, pass string) []byte {
	header := make([]byte, 22)
	le.PutUint16(header, 0x40)
	le.PutUint32(header[8:], 0x10)
	for i, s := range []string{"", user, pass, "", ""} {
		units := utf16.Encode([]rune(s))
		le.PutUint16(header[12+2*i:], uint16(len(units)*2))
	}
	for _, s := range []string{"", user, pass, "", ""} {
		for _, u := range utf16.Encode([]rune(s)) {
			header = append(header, u16(int(u))...)
		}
		header = append(header, 0, 0)
	}
	return header
}

func TestClientInfoBounds(t *testing.T) {
	p := testInfo("user😀", "password")
	u, pass, err := credentials(p)
	if err != nil || u != "user😀" || pass != "password" {
		t.Fatalf("credentials decode: %q %v", u, err)
	}
	for n := 0; n < len(p); n++ {
		if _, _, err := credentials(p[:n]); err == nil {
			t.Fatalf("accepted truncated Client Info: %d", n)
		}
	}
	bad := append([]byte(nil), p...)
	le.PutUint16(bad[14:], 65535)
	if _, _, err := credentials(bad); err == nil {
		t.Fatal("accepted overflowing username")
	}
}

func TestClientInfoSecurityFlagsWithTLS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   uint16
		flagsHi uint16
		valid   bool
	}{
		{"info", 0x40, 0, true},
		{"unused high flags", 0x40, 0xa5a5, true},
		{"incorrect legacy encryption flag", 0x48, 0, true},
		{"ignored sequence flags", 0x70, 0xffff, true},
		{"valid high flags are reserved", 0x8040, 0xffff, true},
		{"licensing encryption ability", 0x240, 0, true},
		{"missing info flag", 0, 0, false},
		{"license packet is not client info", 0x80, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testInfo("anykvm", "test-password")
			le.PutUint16(p, tc.flags)
			le.PutUint16(p[2:], tc.flagsHi)
			user, pass, err := credentials(p)
			if tc.valid {
				if err != nil || user != "anykvm" || pass != "test-password" {
					t.Fatalf("credentials rejected: %q %v", user, err)
				}
			} else if !errors.Is(err, errProtocol) {
				t.Fatalf("accepted non-Client Info packet: %v", err)
			}
		})
	}
}

func TestPacketFramingAndBounds(t *testing.T) {
	for _, p := range [][]byte{{3, 0, 0, 3}, {3, 1, 0, 4}, {0, 0}, {0, 0x80}, {3, 0, 0xff, 0xff, 1}} {
		if _, _, err := readPacket(bytes.NewReader(p)); err == nil {
			t.Fatalf("accepted invalid packet %x", p)
		}
	}
	out, fast, err := readPacket(bytes.NewReader([]byte{4, 4, 0, 0x1e}))
	if err != nil || !fast || !bytes.Equal(out, []byte{4, 0, 0x1e}) {
		t.Fatalf("fast path: %x %v", out, err)
	}
	_, _, err = takeBER([]byte{4, 0x80}, []byte{4})
	if err == nil {
		t.Fatal("accepted indefinite BER")
	}
}

type recordingHID struct{ events []string }

func (h *recordingHID) KeyDown(uint8) error    { h.events = append(h.events, "down"); return nil }
func (h *recordingHID) KeyUp() error           { return nil }
func (h *recordingHID) KeyRelease(uint8) error { h.events = append(h.events, "up"); return nil }
func (h *recordingHID) MouseMove(x, y int) error {
	if x == 32767 && y == 32767 {
		h.events = append(h.events, "corner")
	}
	return nil
}
func (h *recordingHID) MouseMoveRel(int, int) error { return nil }
func (h *recordingHID) MouseButton(_ int, down bool) error {
	if down {
		h.events = append(h.events, "button down")
	} else {
		h.events = append(h.events, "button up")
	}
	return nil
}
func (h *recordingHID) MouseWheel(d int) error {
	if d == 1 {
		h.events = append(h.events, "wheel")
	}
	return nil
}

func TestInputAndDisconnectRelease(t *testing.T) {
	h := &recordingHID{}
	var i inputState
	i.activate(h)
	i.geometry(640, 480, 640, 480)
	if err := i.fast([]byte{4, 0, 0x1e}); err != nil {
		t.Fatal(err)
	}
	if err := i.key(0x1e, 0); err != nil {
		t.Fatal(err)
	}
	if err := i.mouse(0x0800|0x1000|0x8000, 639, 479, false); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := i.mouse(0x200|60, 0, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	i.release()
	i.release()
	if !reflect.DeepEqual(h.events, []string{"down", "up", "down", "corner", "button down", "wheel", "up", "button up"}) {
		t.Fatal(h.events)
	}
	if hidKey(0x1d, true) != 0xe4 || hidKey(0x1c, true) != 0x58 || hidKey(0x1e, false) != 4 {
		t.Fatal("scancode mapping")
	}
	if i.fast([]byte{4, 0}) == nil || i.slow([]byte{1, 0, 0, 0}) == nil {
		t.Fatal("accepted truncated input")
	}
}

func TestGraphicsSegmentSizes(t *testing.T) {
	for _, n := range []int{8, 65535, 65536, 200000} {
		p := bytes.Repeat([]byte{0x55}, n)
		seg := segmentGFX(p)
		var data []byte
		if seg[0] == 0xe0 {
			data = seg[2:]
		} else {
			if int(le.Uint32(seg[3:])) != n {
				t.Fatal("uncompressed length")
			}
			count := int(le.Uint16(seg[1:]))
			seg = seg[7:]
			for j := 0; j < count; j++ {
				size := int(le.Uint32(seg))
				seg = seg[4:]
				if size > 65536 || seg[0] != 4 {
					t.Fatal("segment size")
				}
				data = append(data, seg[1:size]...)
				seg = seg[size:]
			}
			if len(seg) != 0 {
				t.Fatal("trailing segments")
			}
		}
		if !bytes.Equal(data, p) {
			t.Fatal("corrupt segmented graphics")
		}
	}
	reset := graphicsReset(1920, 1080)
	if le.Uint32(reset[4:]) != 340 || le.Uint32(reset[28:]) != 1919 || le.Uint32(reset[32:]) != 1079 {
		t.Fatal("reset monitor geometry")
	}
}

func TestUnencryptedConnectionRejected(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	s := &Server{cfg: Config{TLSConfig: &tls.Config{}}, clients: make(map[*client]bool)}
	c := &client{s: s, raw: raw, conn: tls.Server(raw, s.cfg.TLSConfig), done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- c.run(); raw.Close() }()
	_, err := peer.Write(tpkt([]byte{6, 0xe0, 0, 0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	p, _, err := readPacket(peer)
	if err != nil {
		t.Fatal(err)
	}
	if p[7] != 3 || le.Uint32(p[11:]) != 1 {
		t.Fatalf("TLS-required failure: %x", p)
	}
	if err := <-result; err == nil {
		t.Fatal("accepted unencrypted session")
	}
}

func TestFrameWindowDoesNotBlockShutdown(t *testing.T) {
	c := &client{done: make(chan struct{}), ack: make(chan struct{}, 1)}
	c.nextGraphicsFrame()
	c.nextGraphicsFrame()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.waitWindow(); err != nil {
			t.Error(err)
		}
	}()
	close(c.done)
	wg.Wait()
}

func TestCompressedPushCanBeCanceledOnCaptureShutdown(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	s := &Server{active: c}
	f := bridge.Frame{Format: capture.FormatH264, Data: []byte{1, 2, 3}}
	s.PushFrame(f)
	s.PushFrame(f)
	stop, returned := make(chan struct{}), make(chan struct{})
	go func() { s.PushFrameWithStop(f, stop); close(returned) }()
	close(stop)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("capture shutdown blocked behind compressed output")
	}
	if len(c.frames) != 2 {
		t.Fatal("lost an earlier prediction reference")
	}
	f.Data[0] = 9
	if (<-c.frames).Data[0] != 1 {
		t.Fatal("queued frame aliases the capture buffer")
	}
}

func TestPixelPushKeepsLatestCapture(t *testing.T) {
	for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatJPEG} {
		c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
		s := &Server{active: c}
		data := []byte{0}
		for i := byte(1); i <= 20; i++ {
			data[0] = i
			s.PushFrame(bridge.Frame{Format: format, Data: data})
		}
		data[0] = 99
		if len(c.frames) != 1 || (<-c.frames).Data[0] != 20 {
			t.Fatal("pending capture is stale or aliases capture memory")
		}
	}
}

func TestPixelPushRetainsMixedSourceReferences(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	s := &Server{active: c}
	s.PushFrame(bridge.Frame{Format: capture.FormatH264, Data: []byte{1}})
	s.PushFrame(bridge.Frame{Format: capture.FormatRaw, Data: []byte{2}})
	s.PushFrame(bridge.Frame{Format: capture.FormatRaw, Data: []byte{3}})
	if len(c.frames) != 2 || (<-c.frames).Data[0] != 1 || (<-c.frames).Data[0] != 2 {
		t.Fatal("pixel capture removed a prediction reference or reordered mixed input")
	}
}

type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

func channelTestClient() *client {
	return &client{s: &Server{}, conn: discardConn{}, done: make(chan struct{}), ack: make(chan struct{}, 1), activated: true, dvcCaps: true, gfxCreated: true, gfxOpen: true, ready: true, sent: 2}
}

func TestDVCClientMaximumVersionNegotiation(t *testing.T) {
	for _, version := range []uint16{1, 2, 3} {
		c := channelTestClient()
		c.gfxCreated, c.gfxOpen, c.ready = false, false, false
		p := join([]byte{0x50, 0}, u16(int(version)))
		if err := c.handleVC(join(u32(uint32(len(p))), u32(3), p)); err != nil {
			t.Fatalf("client maximum version %d: %v", version, err)
		}
		if !c.gfxCreated || c.gfxOpen || c.dvcClientVersion != version || c.dvcVersion != version {
			t.Fatalf("client maximum version %d did not request the graphics channel", version)
		}
		if err := c.handleDVC(join([]byte{0x10, gfxChannel}, u32(0))); err != nil {
			t.Fatal(err)
		}
		c.ready = true
		if err := c.handleDVC(join([]byte{0x30, gfxChannel}, gfxPDU(0xd, join(u32(0), u32(2), u32(2))))); err != nil {
			t.Fatalf("uncompressed graphics on DVC version %d: %v", version, err)
		}
		if c.acknowledged != 2 {
			t.Fatalf("DVC version %d did not process the frame acknowledgment", version)
		}
	}
	for _, p := range [][]byte{{0x50}, {0x50, 0, 1}, {0x50, 0, 0, 0}, {0x50, 0, 4, 0}} {
		c := channelTestClient()
		c.gfxCreated, c.gfxOpen, c.ready = false, false, false
		if err := c.handleDVC(p); !errors.Is(err, errProtocol) || c.gfxCreated {
			t.Fatalf("invalid DVC capabilities accepted: %x, %v", p, err)
		}
	}
}

func TestDVCCreateResponseHRESULT(t *testing.T) {
	for _, status := range []uint32{0, 1, 0x7fffffff, 0x80004005, 0xffffffff} {
		for width := byte(0); width <= 2; width++ {
			c := channelTestClient()
			c.gfxOpen = false
			id := u32(gfxChannel)[:1<<width]
			err := c.handleDVC(join([]byte{0x10 | width}, id, u32(status)))
			want := int32(status) >= 0
			if c.gfxOpen != want || (err == nil) != want {
				t.Fatalf("HRESULT=%#x, channel width=%d, opened=%t error=%v", status, width, c.gfxOpen, err)
			}
		}
	}
}

func TestVirtualChannelReassemblyAndBounds(t *testing.T) {
	c := channelTestClient()
	p := join([]byte{0x30, 1}, gfxPDU(0xd, join(u32(0), u32(2), u32(2))))
	if err := c.handleVC(join(u32(uint32(len(p))), u32(1), p[:7])); err != nil {
		t.Fatal(err)
	}
	if c.acknowledged != 0 {
		t.Fatal("processed a partial acknowledgment")
	}
	if err := c.handleVC(join(u32(uint32(len(p))), u32(2), p[7:])); err != nil {
		t.Fatal(err)
	}
	if c.acknowledged != 2 {
		t.Fatal("did not reassemble acknowledgment")
	}
	for _, p := range [][]byte{
		join(u32(1<<21), u32(3), []byte{0}),
		join(u32(1), u32(2), []byte{0}),
		join(u32(1), u32(3), []byte{0, 0}),
		join(u32(1), u32(0x200003), []byte{0}),
		join(u32(2), u32(3), []byte{0x33, 1}),
	} {
		if err := channelTestClient().handleVC(p); err == nil {
			t.Fatalf("accepted invalid channel envelope %x", p)
		}
	}
	if err := c.handleGFX(gfxPDU(0xd, join(u32(0), u32(3), u32(3)))); err == nil {
		t.Fatal("acknowledged an unsent frame")
	}
}

func FuzzChannelMessages(f *testing.F) {
	f.Add([]byte{0})
	f.Add(join(u32(22), u32(3), []byte{0x30, 1}, gfxPDU(0xd, make([]byte, 12))))
	f.Fuzz(func(t *testing.T, p []byte) {
		c := channelTestClient()
		_ = c.handleVC(p)
		_ = c.handleDVC(p)
		_ = c.handleGFX(p)
		_ = c.handleShare(p)
	})
}

func FuzzClientMessages(f *testing.F) {
	f.Add([]byte{0})
	f.Add(testInfo("test", "password"))
	f.Add(join(u16(1), u32(0xa0002), u32(4), u32(0)))
	f.Fuzz(func(t *testing.T, p []byte) {
		_, _, _ = credentials(p)
		_, _, _ = avcCapability(p)
		_, _ = parseConnect(p)
		_, _, _ = parseMCS(p)
		_ = confirmActive(p)
		_, _, _ = readPacket(bytes.NewReader(p))
		_ = avc420SPS(p)
		var i inputState
		_ = i.fast(p)
		_ = i.slow(p)
		_, _, err := takeBER(p, []byte{4})
		if err != nil && !errors.Is(err, errProtocol) && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	})
}

type dvcRecordingConn struct {
	net.Conn
	data bytes.Buffer
}

func (c *dvcRecordingConn) Write(p []byte) (int, error)      { return c.data.Write(p) }
func (c *dvcRecordingConn) SetWriteDeadline(time.Time) error { return nil }

func TestDVCMaximumVersionAdvertisement(t *testing.T) {
	conn := &dvcRecordingConn{}
	c := channelTestClient()
	c.conn = conn
	c.dvcCaps, c.gfxCreated, c.gfxOpen, c.ready = false, false, false, false
	c.data.dvc = 1004
	if err := c.handleShare(shareData(39, make([]byte, 8))); err != nil {
		t.Fatal(err)
	}
	var last []byte
	count := 0
	for conn.data.Len() > 0 {
		p, fast, err := readPacket(&conn.data)
		if err != nil || fast {
			t.Fatalf("invalid server packet: fast=%t error=%v", fast, err)
		}
		last = p
		count++
	}
	if count != 3 || len(last) < 10 || last[3] != 0x68 || be.Uint16(last[6:]) != c.data.dvc {
		t.Fatalf("DVC capabilities not sent after Font Map and pointer: packets=%d final=%x", count, last)
	}
	n, payload, err := takePer(last[9:])
	want := []byte{0x50, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err != nil || n != len(payload) || len(payload) != 8+len(want) ||
		le.Uint32(payload) != uint32(len(want)) || le.Uint32(payload[4:]) != 3 || !bytes.Equal(payload[8:], want) {
		t.Fatalf("invalid DVC v3 capabilities or priority charges: payload=%x error=%v", payload, err)
	}
}
