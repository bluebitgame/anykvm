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

func TestGraphicsScaledMappingNegotiation(t *testing.T) {
	for _, version := range []uint32{0x80004, 0x80105, 0xa0002, 0xa0100, 0xa0400, 0xa0502, 0xa0600, 0xa0601, 0xa0701} {
		for _, disabled := range []bool{false, true} {
			flags := uint32(0)
			if disabled {
				flags = gfxCapsScaledMapDisable
			}
			data := u32(flags)
			if version == 0xa0100 {
				data = make([]byte, 16)
			}
			for _, codec := range []string{"h264", "progressive", "planar", "clearcodec"} {
				if version == 0x80004 && codec == "h264" {
					continue
				}
				if version == 0x80105 && codec == "h264" {
					data = u32(flags | 0x10)
				}
				payload := join(u16(1), u32(version), u32(uint32(len(data))), data)
				v, f, _, err := graphicsCapability(payload, codec)
				if err != nil {
					t.Fatal(version, codec, err)
				}
				want := version == 0xa0502 || version == 0xa0600 || version == 0xa0601 || version == 0xa0701 && !disabled
				if graphicsSupportsScaledMapping(v, f) != want {
					t.Fatalf("%#x %s flags=%#x: scaled mapping=%v", version, codec, f, !want)
				}
				if version == 0xa0701 && f&gfxCapsScaledMapDisable != flags {
					t.Fatal("confirmation lost disabled mapping flag")
				}
			}
		}
	}
	// Use the selected capability, not an unrelated higher THINCLIENT set.
	payload := join(u16(2), u32(0xa0502), u32(4), u32(2), u32(0xa0701), u32(4), u32(0x81))
	v, f, _, err := graphicsCapability(payload, "progressive")
	if err != nil || v != 0xa0502 || !graphicsSupportsScaledMapping(v, f) {
		t.Fatal("lost eligible scaled Progressive set", err)
	}
}

func TestClientScalingSourceSizeAndCeiling(t *testing.T) {
	c := &client{scaledMapping: true}
	c.setOutputSize(320, 240)
	for _, tc := range []struct{ w, h, maxW, maxH, wantW, wantH int }{
		{1920, 1080, 0, 0, 1920, 1080},
		{1920, 1080, 1280, 1000, 1280, 720},
		{1920, 1080, 0, 540, 960, 540},
	} {
		f := bridge.Frame{Width: tc.w, Height: tc.h, OutputMaxW: tc.maxW, OutputMaxH: tc.maxH}
		bw, bh := c.bitmapOutputSize(f, f.Width, f.Height)
		vw, vh := c.videoOutputSize(f)
		if bw != tc.wantW || bh != tc.wantH || vw != bw || vh != bh {
			t.Fatalf("%+v bitmap=%dx%d video=%dx%d", tc, bw, bh, vw, vh)
		}
	}
}

func TestClientScalingPointerGeometry(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		w, h, ew, eh, ow, oh int
		content, want        image.Rectangle
		codec                displayCodec
	}{
		{"pillarbox", 640, 480, 640, 480, 1920, 1080, image.Rect(0, 0, 640, 480), image.Rect(240, 0, 1680, 1080), codecProgressive},
		{"letterbox", 640, 360, 640, 360, 800, 800, image.Rect(0, 0, 640, 360), image.Rect(0, 175, 800, 625), codecProgressive},
		{"capture letterboxing", 640, 480, 640, 480, 1920, 1080, image.Rect(64, 48, 576, 432), image.Rect(384, 108, 1536, 972), codecProgressive},
		{"odd bitmap", 129, 131, 129, 131, 401, 802, image.Rect(0, 0, 129, 131), image.Rect(0, 197, 401, 604), codecProgressive},
		{"AVC padding", 129, 131, 130, 132, 401, 802, image.Rect(0, 0, 129, 131), image.Rect(0, 197, 394, 604), codecAVC420},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{scaledMapping: true, codec: tc.codec}
			c.graphics = makeGraphicsLayout(tc.ew, tc.eh, tc.ow, tc.oh, true)
			hid := &mouseRecorder{}
			c.input.activate(hid)
			f := bridge.Frame{Width: tc.w, Height: tc.h, SourceWidth: tc.w * 2, SourceHeight: tc.h * 2, ContentRect: tc.content}
			// A newer layout request cannot change coordinates until it is mapped.
			c.setOutputSize(tc.ow+200, tc.oh+200)
			c.frameGeometry(f, tc.ew, tc.eh)
			if c.input.w != tc.ow || c.input.h != tc.oh || c.input.contentRect != tc.want || c.input.sourceW != tc.w*2 {
				t.Fatalf("wrong input geometry: %v", c.input.contentRect)
			}
			for _, point := range []image.Point{tc.want.Min, tc.want.Max.Sub(image.Pt(1, 1)), image.Pt(0, 0), image.Pt(tc.ow-1, tc.oh-1)} {
				if err := c.input.mouse(0x800, uint16(point.X), uint16(point.Y), false); err != nil {
					t.Fatal(err)
				}
			}
			want := [][2]int{{0, 0}, {32767, 32767}, {0, 0}, {32767, 32767}}
			for i, p := range hid.positions {
				if p != want[i] {
					t.Fatalf("mouse %d =%v want=%v", i, p, want[i])
				}
			}
		})
	}
}

func assertScaledFrame(t *testing.T, c *client, conn *surfaceTestConn, w, h, ow, oh int) {
	t.Helper()
	if conn.surfaceW != w || conn.surfaceH != h || conn.outputW != ow || conn.outputH != oh {
		t.Fatalf("surface=%dx%d output=%dx%d, want=%dx%d/%dx%d", conn.surfaceW, conn.surfaceH, conn.outputW, conn.outputH, w, h, ow, oh)
	}
	fit := fittedFrameRect(w, h, ow, oh, false)
	if conn.mapped[1] != fit {
		t.Fatal("wrong source viewport", conn.mapped[1], fit)
	}
	area := fit.Dx() * fit.Dy()
	for id, r := range conn.mapped {
		if id == 1 {
			continue
		}
		if !conn.black[id] || !r.Intersect(fit).Empty() {
			t.Fatal("bar overlaps source or is not black", id, r)
		}
		for other, s := range conn.mapped {
			if other != id && !r.Intersect(s).Empty() {
				t.Fatal("overlapping output mappings", id, other)
			}
		}
		area += r.Dx() * r.Dy()
	}
	if area != ow*oh || len(conn.surfaces) != len(conn.mapped) {
		t.Fatal("unpainted/stale output area", area, ow*oh)
	}
	c.input.mu.Lock()
	iw, ih, content := c.input.w, c.input.h, c.input.contentRect
	c.input.mu.Unlock()
	if iw != ow || ih != oh || content != fit {
		t.Fatal("input differs from mapped frame", iw, ih, content, fit)
	}
}

func TestClientScalingBitmapResizeLifecycle(t *testing.T) {
	for _, codec := range []displayCodec{codecClear, codecPlanar, codecProgressive} {
		t.Run(codec.String(), func(t *testing.T) {
			conn := &surfaceTestConn{frames: make(chan struct{}, 8), allowScaled: true}
			c := channelTestClient()
			c.conn, c.codec, c.sent, c.suspendAck = conn, codec, 0, true
			c.scaledMapping = true
			c.frames = make(chan bridge.Frame, 1)
			c.setOutputSize(800, 600)
			result := make(chan error, 1)
			go func() { result <- c.bitmapLoop() }()
			defer func() {
				c.close()
				if err := <-result; err != nil {
					t.Error(err)
				}
			}()
			push := func(w, h int) {
				t.Helper()
				c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: bytes.Repeat([]byte{10, 20, 30, 255}, w*h)}
				select {
				case <-conn.frames:
				case err := <-result:
					result <- err
					t.Fatal("bitmap failed", err)
				case <-time.After(10 * time.Second):
					t.Fatal("no resized bitmap")
				}
			}
			push(320, 180)
			assertScaledFrame(t, c, conn, 320, 180, 800, 600)
			if conn.resets != 1 {
				t.Fatal("missing initial reset")
			}
			// Changing aspect ratio/size while keeping the canvas does not reset it.
			push(180, 320)
			assertScaledFrame(t, c, conn, 180, 320, 800, 600)
			if conn.resets != 1 {
				t.Fatal("source resize reset the output canvas")
			}
			// Same source pixels, new requested canvas: force full redraw and context.
			c.setOutputSize(602, 801)
			push(180, 320)
			assertScaledFrame(t, c, conn, 180, 320, 602, 801)
			if conn.resets != 2 {
				t.Fatal("layout resize was not reset once")
			}
			// Source exactly fills the new output, deleting all obsolete bar surfaces.
			push(602, 801)
			assertScaledFrame(t, c, conn, 602, 801, 602, 801)
			if conn.resets != 2 || len(conn.surfaces) != 1 {
				t.Fatal("stale bars or redundant RESET_GRAPHICS")
			}
			// Independent source damage keeps the surface and mapping intact.
			creates, maps := conn.creates, conn.scaledMaps
			c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: 602, Height: 801, Data: bytes.Repeat([]byte{11, 20, 30, 255}, 602*801)}
			select {
			case <-conn.frames:
			case <-time.After(10 * time.Second):
				t.Fatal("no changed frame")
			}
			if conn.creates != creates || conn.scaledMaps != maps || conn.resets != 2 {
				t.Fatal("unchanged layout remapped each frame")
			}
		})
	}
}

func TestClientScalingH264PassthroughResize(t *testing.T) {
	conn := &surfaceTestConn{frames: make(chan struct{}, 8), allowScaled: true}
	c := channelTestClient()
	c.conn, c.codec, c.sent, c.suspendAck = conn, codecAVC420, 0, true
	c.scaledMapping = true
	c.frames = make(chan bridge.Frame, 1)
	c.setOutputSize(1920, 1080)
	result := make(chan error, 1)
	go func() { result <- c.videoLoop() }()
	defer func() {
		c.close()
		if err := <-result; err != nil {
			t.Error(err)
		}
	}()
	idr := join([]byte{0, 0, 1, 0x67, 66, 0, 31, 0x80}, []byte{0, 0, 1, 0x68, 0xce}, []byte{0, 0, 1, 0x65, 0x80})
	for _, tc := range []struct{ w, h, ow, oh, resets int }{{1280, 720, 1920, 1080, 1}, {640, 480, 1920, 1080, 1}, {640, 480, 800, 800, 2}, {800, 800, 800, 800, 2}} {
		c.setOutputSize(tc.ow, tc.oh)
		c.frames <- bridge.Frame{Format: capture.FormatH264, Width: tc.w, Height: tc.h, Data: idr, CaptureScalesOutput: true}
		select {
		case <-conn.frames:
		case err := <-result:
			result <- err
			t.Fatal("passthrough attempted conversion", err)
		case <-time.After(5 * time.Second):
			t.Fatal("no native H264 frame")
		}
		assertScaledFrame(t, c, conn, tc.w, tc.h, tc.ow, tc.oh)
		if conn.resets != tc.resets {
			t.Fatal("unexpected output reset", conn.resets, tc)
		}
	}
}

func TestGraphicsLayoutInvalidDimensions(t *testing.T) {
	c := channelTestClient()
	for _, n := range []int{0, -1, 8193} {
		if err := c.ensureGraphicsLayout(makeGraphicsLayout(n, 200, 200, 200, false)); err == nil {
			t.Fatal(fmt.Sprintf("accepted width %d", n))
		}
	}
}

func TestClientScalingNegotiatedFallback(t *testing.T) {
	for _, tc := range []struct {
		name           string
		version, flags uint32
		scaled         bool
	}{
		{"10.5 scaled", 0xa0502, 0x20, true},
		{"10.7 scaled", 0xa0701, 0x20, true},
		{"10.7 disabled", 0xa0701, 0xa0, false},
		{"legacy", 0x80004, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &surfaceTestConn{frames: make(chan struct{}, 8), allowScaled: tc.scaled}
			c := channelTestClient()
			c.conn = conn
			c.ready = false
			c.sent = 0
			c.suspendAck = true
			c.s.cfg.Codec = "progressive"
			c.data.width, c.data.height = 800, 600
			c.frames = make(chan bridge.Frame, 1)
			defer func() { c.close(); c.s.wg.Wait() }()
			if err := c.handleGFX(gfxPDU(0x12, join(u16(1), u32(tc.version), u32(4), u32(tc.flags)))); err != nil {
				t.Fatal(err)
			}
			if c.scaledMapping != tc.scaled {
				t.Fatal("negotiated wrong mapping policy")
			}
			c.s.PushFrame(bridge.Frame{Format: capture.FormatRaw, Width: 640, Height: 360, Data: bytes.Repeat([]byte{10, 20, 30, 255}, 640*360)})
			wait := func() {
				t.Helper()
				select {
				case <-conn.frames:
				case <-c.done:
					t.Fatal("graphics rejected")
				case <-time.After(5 * time.Second):
					t.Fatal("no frame")
				}
			}
			wait()
			if tc.scaled {
				assertScaledFrame(t, c, conn, 640, 360, 800, 600)
			} else if conn.surfaceW != 640 || conn.surfaceH != 360 || conn.outputW != 640 || conn.outputH != 360 {
				t.Fatal("fallback changed source-sized output")
			}
			// A static source is requeued on layout change, without new capture damage.
			c.setOutputSize(200, 200)
			wait()
			if tc.scaled {
				assertScaledFrame(t, c, conn, 640, 360, 200, 200)
			} else {
				if conn.surfaceW != 200 || conn.surfaceH != 200 || conn.outputW != 200 || conn.outputH != 200 || conn.scaledMaps != 0 {
					t.Fatal("legacy server-side reduction was bypassed")
				}
				c.input.mu.Lock()
				content := c.input.contentRect
				c.input.mu.Unlock()
				if content != image.Rect(0, 44, 200, 156) {
					t.Fatal("fallback input does not follow server-side letterboxing", content)
				}
			}
		})
	}
}
