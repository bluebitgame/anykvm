package rdp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

type bitmapRefreshConn struct {
	displayActivationConn
	wrote chan struct{}
}

func (c bitmapRefreshConn) Write(p []byte) (int, error) {
	select {
	case c.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestBitmapRefreshDoesNotRestartPixelCapture(t *testing.T) {
	for _, codec := range []displayCodec{codecClear, codecPlanar, codecProgressive} {
		t.Run(codec.String(), func(t *testing.T) {
			wrote := make(chan struct{}, 1)
			c := channelTestClient()
			c.conn, c.codec, c.sent = bitmapRefreshConn{wrote: wrote}, codec, 0
			c.frames = make(chan bridge.Frame, 1)
			var restarts atomic.Int32
			c.s.cfg.OnKeyframe = func() { restarts.Add(1) }
			c.reset.Store(true) // Initial client refresh, before its first bitmap.
			c.frames <- bridge.Frame{Format: capture.FormatRaw, Width: 200, Height: 200, Data: bytes.Repeat([]byte{10, 20, 30, 255}, 200*200)}
			finished := make(chan error, 1)
			go func() { finished <- c.bitmapLoop() }()
			select {
			case <-wrote:
			case <-time.After(time.Second):
				close(c.done)
				<-finished
				t.Fatal("first bitmap was not sent")
			}
			close(c.done)
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			if restarts.Load() != 0 {
				t.Fatal("first bitmap refresh reopened capture")
			}
		})
	}
}

func TestBitmapCapabilitySelection(t *testing.T) {
	mobile := [][2]uint32{{0x80004, 2}, {0x80105, 2}, {0xa0002, 0x22}, {0xa0200, 0x22},
		{0xa0301, 0x20}, {0xa0400, 0x22}, {0xb0101, 0x1a2}, {0xb0300, 0x1a2}}
	p := u16(len(mobile))
	for _, v := range mobile {
		p = append(p, join(u32(v[0]), u32(4), u32(v[1]))...)
	}
	for _, preference := range []string{"", "auto", "clearcodec", "planar", "progressive"} {
		version, flags, codec, err := graphicsCapability(p, preference)
		want := codecClear
		if preference == "planar" {
			want = codecPlanar
		}
		if preference == "" || preference == "auto" || preference == "progressive" {
			want = codecProgressive
		}
		if err != nil || version != 0xa0400 || flags != 0x22 || codec != want {
			t.Fatalf("%s: version=%#x flags=%#x codec=%s error=%v", preference, version, flags, codec, err)
		}
	}
	if _, _, _, err := graphicsCapability(p, "h264"); err == nil || errors.Is(err, errProtocol) {
		t.Fatal("strict H.264 policy must reject this valid advertisement")
	}
	if _, _, _, err := graphicsCapability(join(u16(1), u32(0xa0400), u32(4), u32(0x23)), "progressive"); err == nil {
		t.Fatal("THINCLIENT enabled Progressive")
	}
	// A lower non-thin set is eligible even if the highest known set is thin.
	v, f, codec, err := graphicsCapability(join(u16(2), u32(0x80105), u32(4), u32(2), u32(0xa0400), u32(4), u32(0x23)), "progressive")
	if err != nil || v != 0x80105 || f != 2 || codec != codecProgressive {
		t.Fatal("lost non-thin Progressive capability")
	}
	// Prefer an AVC-enabled set over a newer set that disables AVC.
	p = join(u16(2), u32(0x80105), u32(4), u32(0x12), u32(0xa0400), u32(4), u32(0x22))
	version, flags, codec, err := graphicsCapability(p, "auto")
	if err != nil || version != 0x80105 || flags != 0x10 || codec != codecAVC420 {
		t.Fatal("auto policy lost AVC preference")
	}
	for _, version := range []uint32{0x80004, 0x80105} {
		v, flags, codec, err := graphicsCapability(join(u16(1), u32(version), u32(4), u32(3)), "clearcodec")
		if err != nil || v != version || flags != 3 || codec != codecClear {
			t.Fatal("baseline graphics capabilities rejected")
		}
	}
	for _, p := range [][]byte{
		join(u16(1), u32(0xb0300), u32(4), u32(0x1a2)),
		join(u16(1), u32(0xa0400), u32(8), make([]byte, 8)),
		join(u16(1), u32(0xa0100), u32(16), bytes.Repeat([]byte{1}, 16)),
	} {
		if _, _, _, err := graphicsCapability(p, "auto"); err == nil {
			t.Fatal("unsupported or malformed set enabled bitmap output")
		}
	}
}

func TestAutomaticGraphicsCodecPriority(t *testing.T) {
	tests := []struct {
		name    string
		sets    [][2]uint32
		version uint32
		flags   uint32
		codec   displayCodec
	}{
		{"avc_over_progressive", [][2]uint32{{0x80105, 0x12}, {0xa0400, 0x22}}, 0x80105, 0x10, codecAVC420},
		{"progressive_without_avc", [][2]uint32{{0x80105, 2}, {0xa0400, 0x22}}, 0xa0400, 0x22, codecProgressive},
		{"older_progressive_over_newer_thin", [][2]uint32{{0x80105, 2}, {0xa0400, 0x23}}, 0x80105, 2, codecProgressive},
		{"thin_client_planar", [][2]uint32{{0x80004, 3}, {0xa0400, 0x23}}, 0xa0400, 0x23, codecPlanar},
		{"baseline_non_thin_progressive", [][2]uint32{{0x80004, 2}}, 0x80004, 2, codecProgressive},
		{"baseline_thin_planar", [][2]uint32{{0x80004, 3}}, 0x80004, 3, codecPlanar},
		{"unknown_version_ignored", [][2]uint32{{0x80004, 3}, {0xb0300, 0x1a2}}, 0x80004, 3, codecPlanar},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capability ordering must not override the codec priority.
			for _, reverse := range []bool{false, true} {
				p := u16(len(tt.sets))
				for i := range tt.sets {
					index := i
					if reverse {
						index = len(tt.sets) - 1 - i
					}
					set := tt.sets[index]
					p = append(p, join(u32(set[0]), u32(4), u32(set[1]))...)
				}
				for _, preference := range []string{"", "auto"} {
					version, flags, codec, err := graphicsCapability(p, preference)
					if err != nil || version != tt.version || flags != tt.flags || codec != tt.codec {
						t.Fatalf("preference=%q reverse=%t: version=%#x flags=%#x codec=%s error=%v", preference, reverse, version, flags, codec, err)
					}
				}
			}
		})
	}
}

func TestClearCodecRunLengthBoundaries(t *testing.T) {
	for _, n := range []int{1, 254, 255, 65534, 65535, 65536} {
		pixels := bytes.Repeat([]byte{17, 37, 197, 0}, n)
		p := encodeClearCodec(pixels, n, 1, 255)
		want := []byte{17, 37, 197}
		if n < 255 {
			want = append(want, byte(n))
		} else if n < 65535 {
			want = append(want, 255)
			want = append(want, u16(n)...)
		} else {
			want = append(want, 255, 255, 255)
			want = append(want, u32(uint32(n))...)
		}
		if !bytes.Equal(p[:2], []byte{0, 255}) || int(le.Uint32(p[2:])) != len(want) ||
			le.Uint32(p[6:]) != 0 || le.Uint32(p[10:]) != 0 || !bytes.Equal(p[14:], want) {
			t.Fatalf("incorrect ClearCodec run %d: %x", n, p)
		}
	}
	pixels := make([]byte, 20*3*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2] = byte(i), byte(i+1), byte(i+2)
	}
	p := encodeClearCodec(pixels, 20, 3, 0)
	if len(p) != 14+13+20*3*3 || le.Uint32(p[2:]) != 0 || le.Uint32(p[10:]) != 13+20*3*3 ||
		le.Uint16(p[18:]) != 20 || le.Uint16(p[20:]) != 3 || p[26] != 0 {
		t.Fatalf("incorrect raw subcodec rectangle: %x", p[:27])
	}
	for i := 0; i < 60; i++ {
		if !bytes.Equal(p[27+i*3:30+i*3], pixels[i*4:i*4+3]) {
			t.Fatal("raw BGR subcodec changed colors")
		}
	}
}

func TestPlanarNormativeRunSequence(t *testing.T) {
	// MS-RDPEGDI 3.1.9.2.1 example, after the specified signed delta transform.
	rows := [][]byte{{255, 255, 255, 255, 254, 253}, {1, 125, 245, 194, 154, 56}, {1, 103, 139, 163, 120, 175}}
	var got []byte
	for _, row := range rows {
		got = appendPlanarRow(got, row)
	}
	want := []byte{0x13, 0xff, 0x20, 0xfe, 0xfd, 0x60, 0x01, 0x7d, 0xf5, 0xc2, 0x9a, 0x38, 0x60, 0x01, 0x67, 0x8b, 0xa3, 0x78, 0xaf}
	if !bytes.Equal(got, want) {
		t.Fatalf("normative RLE sequence: %x", got)
	}
	p := encodePlanar(bytes.Repeat([]byte{10, 30, 210, 0}, 128*128), 128, 128)
	if p[0] != 0x39 || len(p) >= 128*128 {
		t.Fatal("solid tile was not YCoCg420 RLE compressed")
	}
	p = encodePlanar([]byte{11, 22, 33, 0, 44, 55, 66, 0}, 2, 1)
	if !bytes.Equal(p, []byte{0x29, 22, 55, 245, 0, 0}) {
		t.Fatalf("uncompressed YCoCg420 plane order: %x", p)
	}
}

func TestBitmapDirtyTilesAndExclusiveBounds(t *testing.T) {
	pixels := make([]byte, 129*131*4)
	all := dirtyBitmapRects(pixels, nil, 129, 131)
	if len(all) != 9 || all[8] != (bitmapRect{128, 128, 1, 3}) {
		t.Fatalf("partial edge tiles: %+v", all)
	}
	previous := append([]byte(nil), pixels...)
	if len(dirtyBitmapRects(pixels, previous, 129, 131)) != 0 {
		t.Fatal("unchanged desktop sent updates")
	}
	pixels[(130*129+128)*4] = 37
	changed := dirtyBitmapRects(pixels, previous, 129, 131)
	if len(changed) != 1 || changed[0] != all[8] {
		t.Fatalf("changed edge pixel: %+v", changed)
	}
	tile := bitmapTile(pixels, 129, changed[0])
	if len(tile) != 12 || tile[8] != 37 {
		t.Fatal("edge tile extraction changed row order")
	}
	p := graphicsBitmap(tile, changed[0], codecPlanar)
	if le.Uint32(p[4:]) != uint32(len(p)) || le.Uint16(p[10:]) != 0xa ||
		le.Uint16(p[17:]) != 129 || le.Uint16(p[19:]) != 131 {
		t.Fatal("WIRETOSURFACE_1 codec, length, or exclusive bounds incorrect")
	}
}

func TestProgressiveDirtyTiles(t *testing.T) {
	const w, h = 129, 131
	previous := progressivePixels(w, h, "gradient")
	all := dirtyProgressiveRects(previous, nil, w, h)
	if len(all) != 9 || all[8] != (bitmapRect{128, 128, 1, 3}) {
		t.Fatalf("partial 64px edge tiles: %+v", all)
	}
	if got := dirtyProgressiveRects(previous, previous, w, h); len(got) != 0 {
		t.Fatal("unchanged Progressive picture sent updates")
	}
	for _, point := range [][2]int{{0, 0}, {63, 63}, {64, 63}, {63, 64}, {64, 64}, {127, 127}, {128, 130}} {
		pixels := append([]byte(nil), previous...)
		x, y := point[0], point[1]
		pixels[(y*w+x)*4] ^= 255
		tx, ty := x/64*64, y/64*64
		want := []bitmapRect{{tx, ty, min(64, w-tx), min(64, h-ty)}}
		if got := dirtyProgressiveRects(pixels, previous, w, h); !reflect.DeepEqual(got, want) {
			t.Fatalf("changed pixel %v encoded unchanged neighbors: got %+v want %+v", point, got, want)
		}
	}
	pixels := append([]byte(nil), previous...)
	for _, point := range [][2]int{{1, 1}, {65, 65}, {128, 130}} {
		pixels[(point[1]*w+point[0])*4] ^= 255
	}
	want := []bitmapRect{all[0], all[4], all[8]}
	if got := dirtyProgressiveRects(pixels, previous, w, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("sparse updates: got %+v want %+v", got, want)
	}
	if got := dirtyProgressiveRects(pixels, nil, w, h); !reflect.DeepEqual(got, all) {
		t.Fatal("refresh did not invalidate every tile")
	}
}

func TestBitmapCoalescingDoesNotAddUnchangedBlocks(t *testing.T) {
	all := []bitmapRect{{0, 0, 64, 64}, {64, 0, 64, 64}, {0, 64, 64, 64}, {64, 64, 64, 64}}
	for mask := 0; mask < 16; mask++ {
		var dirty []bitmapRect
		for i, r := range all {
			if mask&(1<<i) != 0 {
				dirty = append(dirty, r)
			}
		}
		merged := coalesceBitmapRects(dirty, 128, 128)
		for i, block := range all {
			covered := 0
			for _, r := range merged {
				if r.x <= block.x && r.y <= block.y && r.x+r.w >= block.x+block.w && r.y+r.h >= block.y+block.h {
					covered++
				}
			}
			want := 0
			if mask&(1<<i) != 0 {
				want = 1
			}
			if covered != want {
				t.Fatalf("mask=%#x block=%d coverage=%d want=%d", mask, i, covered, want)
			}
		}
		if mask == 15 && (len(merged) != 1 || merged[0] != (bitmapRect{0, 0, 128, 128})) {
			t.Fatal("complete dirty group was not coalesced")
		}
	}
	edges := dirtyBitmapRects(make([]byte, 129*131*4), nil, 129, 131)
	merged := coalesceBitmapRects(edges, 129, 131)
	want := []bitmapRect{{0, 0, 128, 128}, {128, 0, 1, 128}, {0, 128, 128, 3}, {128, 128, 1, 3}}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("coalesced edge geometry: %+v", merged)
	}
}

func TestBitmapPixelSourcesAndBounds(t *testing.T) {
	v := videoPipeline{}
	defer v.close()
	for _, size := range [][2]int{{1, 1}, {3, 5}, {0, 4}, {8192, 8192}} {
		w, h := size[0], size[1]
		var pixels []byte
		if w*h < 100 {
			pixels = make([]byte, w*h*4)
		}
		_, _, _, _, err := v.sourcePixels(bridge.Frame{Format: capture.FormatRaw, Data: pixels, Width: w, Height: h})
		if (err != nil) != (w == 0 || w*h > (64<<20)/4) {
			t.Fatalf("source bounds %dx%d: %v", w, h, err)
		}
	}
	for _, format := range []string{"nv12", "yuv420p", "uyvy422", "yuyv422"} {
		w, h := 4, 2
		data := bytes.Repeat([]byte{128}, w*h*3/2)
		if format == "uyvy422" || format == "yuyv422" {
			data = bytes.Repeat([]byte{128}, w*h*2)
		}
		p, err := bitmapBGRA(data, w, h, format)
		if err != nil || len(p) != w*h*4 || !bytes.Equal(p[:4], []byte{130, 130, 130, 255}) {
			t.Fatalf("%s neutral YUV: %x %v", format, p, err)
		}
		if _, err := bitmapBGRA(data[:len(data)-1], w, h, format); err == nil {
			t.Fatal("accepted truncated YUV")
		}
	}
}

func TestRDPCodecConfiguration(t *testing.T) {
	for _, codec := range []string{"", "auto", "h264", "clearcodec", "planar", "progressive", "jpeg"} {
		server, err := NewServer(Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test", Codec: codec,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}})
		if (err != nil) != (codec == "jpeg") {
			t.Fatalf("codec %q: %v", codec, err)
		}
		if codec == "" && server.cfg.Codec != "auto" {
			t.Fatalf("omitted codec normalized to %q", server.cfg.Codec)
		}
	}
}

func TestRDPBitrateConfiguration(t *testing.T) {
	for _, bitrate := range []int{0, 12000, -1} {
		server, err := NewServer(Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test", Bitrate: bitrate,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}})
		if bitrate < 0 {
			if err == nil {
				t.Fatalf("negative bitrate %d accepted", bitrate)
			}
			continue
		}
		if err != nil {
			t.Fatalf("bitrate %d rejected: %v", bitrate, err)
		}
		if server.cfg.Bitrate != bitrate {
			t.Fatalf("bitrate changed: got %d, want %d", server.cfg.Bitrate, bitrate)
		}
	}
}

func TestRDPHEVCConfiguration(t *testing.T) {
	for _, codec := range []string{"auto", "h264", "clearcodec", "planar", "progressive"} {
		t.Run(codec, func(t *testing.T) {
			cfg := Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test", Codec: codec,
				TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}}
			if _, err := NewServer(cfg); err != nil {
				t.Fatalf("default-off HEVC changed existing codec policy: %v", err)
			}
			cfg.EnableHEVC = true
			server, err := NewServer(cfg)
			if server != nil || !errors.Is(err, ErrHEVCUnavailable) {
				t.Fatalf("unimplemented HEVC must fail explicitly before starting: server=%v err=%v", server, err)
			}
		})
	}
}

func TestRDPProgressiveQuantizationConfiguration(t *testing.T) {
	for _, value := range []int{0, 6, 7, 8, 15, -1, 1, 5, 16, 256} {
		server, err := NewServer(Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test",
			Codec: "progressive", ProgressiveQuantization: value,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}})
		if value != 0 && (value < 6 || value > 15) {
			if err == nil {
				t.Fatalf("invalid quantization %d accepted", value)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := value
		if want == 0 {
			want = DefaultProgressiveQuantization
		}
		if server.cfg.ProgressiveQuantization != want {
			t.Fatalf("quantization=%d want %d", server.cfg.ProgressiveQuantization, want)
		}
	}
}
