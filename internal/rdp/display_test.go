package rdp

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func testDisplayLayout(w, h int) []byte {
	return join(u32(2), u32(56), u32(40), u32(1), u32(1), u32(0), u32(0), u32(uint32(w)), u32(uint32(h)), u32(0), u32(0), u32(0), u32(100), u32(100))
}

func recordedDVC(t *testing.T, conn *dvcRecordingConn) [][]byte {
	t.Helper()
	var messages [][]byte
	for conn.data.Len() > 0 {
		p, fast, err := readPacket(&conn.data)
		if err != nil || fast || len(p) < 10 {
			t.Fatalf("server packet: %x %v", p, err)
		}
		n, payload, err := takePer(p[9:])
		if err != nil || n != len(payload) || len(payload) < 8 || le.Uint32(payload) != uint32(len(payload)-8) || le.Uint32(payload[4:]) != 3 {
			t.Fatalf("DVC envelope: %x %v", payload, err)
		}
		messages = append(messages, append([]byte(nil), payload[8:]...))
	}
	return messages
}

func TestDisplayControlCreationAndCapabilities(t *testing.T) {
	conn := &dvcRecordingConn{}
	c := channelTestClient()
	c.conn, c.gfxCreated, c.gfxOpen = conn, false, false
	c.s.cfg.OnDesktopResize = func(capture.DesktopSize) error { return nil }
	if err := c.handleDVC([]byte{0x50, 0, 3, 0}); err != nil {
		t.Fatal(err)
	}
	requests := recordedDVC(t, conn)
	if len(requests) != 2 || !bytes.Equal(requests[1], join([]byte{0x10, dispChannel}, []byte("Microsoft::Windows::RDS::DisplayControl\x00"))) {
		t.Fatalf("channel requests: %x", requests)
	}
	if err := c.handleDVC(join([]byte{0x10, dispChannel}, u32(0))); err != nil {
		t.Fatal(err)
	}
	capabilities := recordedDVC(t, conn)
	want := join([]byte{0x30, dispChannel}, u32(5), u32(20), u32(1), u32(8192), u32(8192))
	if len(capabilities) != 1 || !bytes.Equal(capabilities[0], want) {
		t.Fatalf("display capabilities: %x", capabilities)
	}
	if err := c.handleDVC([]byte{0x40, dispChannel}); err != nil || c.dispOpen {
		t.Fatal("display channel close terminated graphics")
	}

	c.dispCreated = true
	if err := c.handleDVC(join([]byte{0x10, dispChannel}, u32(0x80004005))); err != nil || c.dispOpen {
		t.Fatal("optional display channel rejection failed the session")
	}
}

func TestDisplayLayoutBounds(t *testing.T) {
	p := testDisplayLayout(1920, 1080)
	size, err := parseDisplayLayout(p)
	if err != nil || size != (capture.DesktopSize{Width: 1920, Height: 1080, ScalePercent: 100}) {
		t.Fatalf("layout: %+v %v", size, err)
	}
	for n := 0; n < len(p); n++ {
		if _, err := parseDisplayLayout(p[:n]); !errors.Is(err, errProtocol) {
			t.Fatalf("accepted %d-byte layout", n)
		}
	}
	for _, change := range []struct {
		offset int
		value  uint32
	}{
		{0, 5}, {4, 55}, {8, 39}, {12, 2}, {16, 0}, {20, 1}, {24, 1},
		{28, 199}, {28, 1921}, {28, 8194}, {32, 199}, {32, 8193},
	} {
		bad := append([]byte(nil), p...)
		le.PutUint32(bad[change.offset:], change.value)
		if _, err := parseDisplayLayout(bad); !errors.Is(err, errProtocol) {
			t.Fatalf("accepted offset=%d value=%d", change.offset, change.value)
		}
	}
}

func TestDisplayFragmentsInterleavedWithGraphics(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		c := channelTestClient()
		c.dispCreated, c.dispOpen, c.dvcVersion = true, true, 3
		c.s.active = c
		sizes := make(chan capture.DesktopSize, 1)
		c.s.cfg.OnDesktopResize = func(size capture.DesktopSize) error { sizes <- size; return nil }
		startTestDesktopWorker(t, c)
		layout, ack := testDisplayLayout(1280, 720), gfxPDU(0xd, join(u32(0), u32(2), u32(2)))
		first := join([]byte{0x20, dispChannel, byte(len(layout))}, layout[:11])
		last := join([]byte{0x30, dispChannel}, layout[11:])
		if compressed {
			first = join([]byte{0x60, dispChannel, byte(len(layout))}, liteTestRun(layout[:11]))
			last = join([]byte{0x70, dispChannel}, liteTestRun(layout[11:]))
		}
		for _, packet := range [][]byte{first, join([]byte{0x20, gfxChannel, byte(len(ack))}, ack[:8]), last, join([]byte{0x30, gfxChannel}, ack[8:])} {
			if err := c.handleDVC(packet); err != nil {
				t.Fatalf("compressed=%t: %v", compressed, err)
			}
		}
		size := nextTestDesktopSize(t, sizes)
		if size.Width != 1280 || size.Height != 720 || c.acknowledged != 2 || c.dispTotal != 0 || c.dvcTotal != 0 {
			t.Fatal("interleaved channels mixed their fragment state")
		}
		c.s.mu.Lock()
		c.s.active = nil
		c.s.mu.Unlock()
		if err := c.handleDVC(join([]byte{0x30, dispChannel}, layout)); err != nil || len(sizes) != 0 {
			t.Fatal("stale client resized the desktop")
		}
	}
}

type displayActivationConn struct{ discardConn }

func (displayActivationConn) RemoteAddr() net.Addr            { return &net.TCPAddr{} }
func (displayActivationConn) Close() error                    { return nil }
func (displayActivationConn) SetReadDeadline(time.Time) error { return nil }

func startTestDesktopWorker(t *testing.T, c *client) {
	t.Helper()
	c.conn = displayActivationConn{}
	c.desktopWake = make(chan struct{}, 1)
	c.s.wg.Add(1)
	go func() { defer c.s.wg.Done(); c.desktopLoop(capture.DesktopSize{}) }()
	t.Cleanup(func() { c.close(); c.s.wg.Wait() })
}

func nextTestDesktopSize(t *testing.T, sizes <-chan capture.DesktopSize) capture.DesktopSize {
	t.Helper()
	select {
	case size := <-sizes:
		return size
	case <-time.After(time.Second):
		t.Fatal("desktop request not delivered")
		return capture.DesktopSize{}
	}
}

func TestRDPInitialClientGeometryReachesBridge(t *testing.T) {
	for _, pending := range []bool{false, true} {
		c := channelTestClient()
		c.conn = displayActivationConn{}
		c.frames = make(chan bridge.Frame, 2)
		c.data.width, c.data.height, c.data.scalePercent = 1920, 1080, 150
		c.codec, c.ready = codecClear, false
		if pending {
			if err := c.handleDisplayLayout(testDisplayLayout(1280, 720)); err != nil {
				t.Fatal(err)
			}
		}
		sizes := make(chan capture.DesktopSize, 1)
		c.s.cfg.OnSessionStart = func(codec string, desktop capture.DesktopSize) {
			if codec != "clearcodec" {
				t.Errorf("codec=%s", codec)
			}
			sizes <- desktop
		}
		if err := c.activate(); err != nil {
			t.Fatal(err)
		}
		want := capture.DesktopSize{Width: 1920, Height: 1080, ScalePercent: 150}
		if pending {
			want = capture.DesktopSize{Width: 1280, Height: 720, ScalePercent: 100}
		}
		size := nextTestDesktopSize(t, sizes)
		if size != want {
			t.Fatalf("session geometry=%+v want=%+v", size, want)
		}
		c.close()
		c.s.wg.Wait()
	}
}

type startupConn struct {
	displayActivationConn
	closed chan struct{}
	once   sync.Once
}

func (c *startupConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

func TestSlowDesktopStartupDoesNotBlockRDP(t *testing.T) {
	c := channelTestClient()
	conn := &startupConn{closed: make(chan struct{})}
	c.conn, c.frames, c.codec = conn, make(chan bridge.Frame, 2), codecClear
	c.data.width, c.data.height = 1976, 952
	started, release := make(chan struct{}), make(chan struct{})
	disconnected, resized := make(chan struct{}, 1), make(chan struct{}, 1)
	c.s.cfg.OnSessionStart = func(string, capture.DesktopSize) { close(started); <-release }
	c.s.cfg.OnDesktopResize = func(capture.DesktopSize) error { resized <- struct{}{}; return nil }
	c.s.cfg.OnDisconnected = func() { disconnected <- struct{}{} }
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { unblock(); c.close(); c.s.wg.Wait() }()
	activated := make(chan error, 1)
	go func() { activated <- c.activate() }()
	select {
	case err := <-activated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("display switch blocked RDP activation")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("display startup did not begin")
	}
	if err := c.handleDisplayLayout(testDisplayLayout(1600, 900)); err != nil {
		t.Fatal(err)
	}
	if err := c.handleGFX(gfxPDU(0xd, join(u32(0), u32(2), u32(2)))); err != nil || c.acknowledged != 2 {
		t.Fatal("display startup blocked frame acknowledgments")
	}
	closed := make(chan struct{})
	go func() { c.close(); close(closed) }()
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("display startup blocked transport cancellation")
	}
	select {
	case <-disconnected:
		t.Fatal("display cleanup ran before startup finished")
	default:
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("display startup leaked session cleanup")
	}
	c.s.wg.Wait()
	if len(disconnected) != 1 || len(resized) != 0 {
		t.Fatal("closed session applied a queued resize or skipped cleanup")
	}
}

func TestDesktopLayoutsCoalesceDuringStartup(t *testing.T) {
	c := channelTestClient()
	c.conn, c.frames, c.codec = displayActivationConn{}, make(chan bridge.Frame, 2), codecClear
	c.data.width, c.data.height = 1976, 952
	started, release := make(chan struct{}), make(chan struct{})
	sizes := make(chan capture.DesktopSize, 4)
	c.s.cfg.OnSessionStart = func(string, capture.DesktopSize) { close(started); <-release }
	c.s.cfg.OnDesktopResize = func(size capture.DesktopSize) error { sizes <- size; return nil }
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); c.close(); c.s.wg.Wait() }()
	if err := c.activate(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	for _, width := range []int{1280, 1440, 1600} {
		if err := c.handleDisplayLayout(testDisplayLayout(width, 900)); err != nil {
			t.Fatal(err)
		}
	}
	unblock()
	if got := nextTestDesktopSize(t, sizes); got.Width != 1600 || got.Height != 900 {
		t.Fatalf("queued resize=%+v; latest client layout was lost", got)
	}
	c.close()
	c.s.wg.Wait()
	if len(sizes) != 0 {
		t.Fatal("obsolete queued layouts switched the display")
	}
}

func FuzzDisplayLayout(f *testing.F) {
	f.Add(testDisplayLayout(1920, 1080))
	f.Fuzz(func(t *testing.T, p []byte) {
		_, _ = parseDisplayLayout(p)
		c := channelTestClient()
		c.dispCreated, c.dispOpen, c.dvcVersion = true, true, 3
		_ = c.handleDVC(p)
	})
}
