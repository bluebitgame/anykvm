package rdp

import (
	"testing"

	"anykvm/internal/capture"
)

func dpiConnectPacket(core []byte, extras ...[]byte) []byte {
	channels := join(u32(1), []byte("drdynvc\x00"), u32(0))
	blocks := join(capability(0xc001, core), capability(0xc003, channels), join(extras...))
	gcc := join([]byte("Duca"), perLength(len(blocks)), blocks)
	value := join(berValue([]byte{4}, nil), berValue([]byte{4}, nil), berValue([]byte{1}, nil),
		berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{4}, gcc))
	return join([]byte{2, 0xf0, 0x80}, berValue([]byte{0x7f, 0x65}, value))
}

func TestClientCoreDesktopScale(t *testing.T) {
	for _, tc := range []struct {
		name            string
		length          int
		desktop, device uint32
		want            int
	}{
		{"omitted", 212, 0, 0, 0},
		{"missing device field", 226, 200, 0, 0},
		{"truncated device field", 229, 200, 0, 0},
		{"100 percent", 230, 100, 100, 100},
		{"125 percent", 230, 125, 140, 125},
		{"150 percent", 230, 150, 180, 150},
		{"200 percent", 230, 200, 100, 200},
		{"500 percent", 230, 500, 100, 500},
		{"too small", 230, 99, 100, 0},
		{"too large", 230, 501, 100, 0},
		{"invalid device", 230, 200, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := make([]byte, 230)
			le.PutUint16(core[4:], 1920)
			le.PutUint16(core[6:], 1080)
			le.PutUint16(core[140:], 0x100)
			le.PutUint32(core[222:], tc.desktop)
			le.PutUint32(core[226:], tc.device)
			data, err := parseConnect(dpiConnectPacket(core[:tc.length]))
			if err != nil || data.width != 1920 || data.height != 1080 || data.scalePercent != tc.want {
				t.Fatalf("connection data=%+v error=%v", data, err)
			}
		})
	}
}

func TestSingleMonitorExtendedScale(t *testing.T) {
	core := make([]byte, 230)
	le.PutUint16(core[4:], 1920)
	le.PutUint16(core[6:], 1080)
	le.PutUint16(core[140:], 0x100)
	le.PutUint32(core[222:], 100)
	le.PutUint32(core[226:], 100)
	ex := capability(0xc008, join(u32(0), u32(20), u32(1), u32(0), u32(0), u32(0), u32(150), u32(140)))
	for _, blocks := range [][]byte{
		dpiConnectPacket(core, ex),
		dpiConnectPacket(core[:212], ex),
		// Reversing the GCC block order must not discard monitor DPI.
		dpiConnectPacket(core[:212], ex, capability(0xc001, core)),
	} {
		data, err := parseConnect(blocks)
		if err != nil || data.scalePercent != 150 {
			t.Fatalf("monitor attributes data=%+v error=%v", data, err)
		}
	}
}

func scaledDisplayLayout(w, h, desktop, device int) []byte {
	p := testDisplayLayout(w, h)
	le.PutUint32(p[48:], uint32(desktop))
	le.PutUint32(p[52:], uint32(device))
	return p
}

func TestDisplayControlDesktopScale(t *testing.T) {
	for _, tc := range []struct{ desktop, device, want int }{
		{100, 100, 100}, {125, 140, 125}, {150, 180, 150}, {200, 100, 200}, {500, 100, 500},
		{0, 0, 0}, {99, 100, 0}, {501, 100, 0}, {200, 200, 0},
	} {
		size, err := parseDisplayLayout(scaledDisplayLayout(1920, 1080, tc.desktop, tc.device))
		if err != nil || size.ScalePercent != tc.want {
			t.Fatalf("desktop=%d device=%d: size=%+v error=%v", tc.desktop, tc.device, size, err)
		}
	}
	for _, ready := range []bool{false, true} {
		c := channelTestClient()
		c.ready, c.s.active, c.data.scalePercent = ready, c, 125
		sizes := make(chan capture.DesktopSize, 1)
		c.s.cfg.OnDesktopResize = func(size capture.DesktopSize) error { sizes <- size; return nil }
		if ready {
			startTestDesktopWorker(t, c)
		}
		for _, percent := range []int{200, 0, 150} {
			if err := c.handleDisplayLayout(scaledDisplayLayout(1920, 1080, percent, 100)); err != nil {
				t.Fatal(err)
			}
			want := percent
			if want == 0 {
				want = 200
			}
			if ready {
				if nextTestDesktopSize(t, sizes).ScalePercent != want {
					t.Fatal("DPI-only request or ignored invalid DPI lost")
				}
			} else if c.pendingDesktop.ScalePercent != want {
				t.Fatal("pending DPI request lost")
			}
		}
		if !ready && len(sizes) != 0 {
			t.Fatal("desktop changed before authentication/graphics activation")
		}
		if ready {
			c.s.mu.Lock()
			c.s.active = nil
			c.s.mu.Unlock()
			if err := c.handleDisplayLayout(scaledDisplayLayout(1920, 1080, 100, 100)); err != nil || len(sizes) != 0 {
				t.Fatal("stale client changed DPI")
			}
		}
	}
}
