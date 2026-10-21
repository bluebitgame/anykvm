package vnc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
)

func restartWireFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../jpegcodec/testdata/restart_" + name + ".jpeg")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCompressedGPUJPEGDirtyDelivery(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			c, conn := newJPEGWireClient(enc, 193, 131)
			frame := &frameData{format: capture.FormatJPEG, width: 193, height: 131, data: restartWireFixture(t, "base")}
			c.sendFrame(frame)
			first := readJPEGWire(t, conn, enc)
			if len(first) != 1 || first[0].r != (rectangle{0, 0, 193, 131}) || !bytes.Equal(first[0].data, frame.data) {
				t.Fatal("initial GPU JPEG was transcoded or incorrectly framed")
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 || !c.pendingReq {
				t.Fatal("static GPU JPEG transmitted or pending request lost")
			}
			frame.data = restartWireFixture(t, "changed")
			c.sendFrame(frame)
			got := readJPEGWire(t, conn, enc)
			if len(got) != 1 || got[0].r != (rectangle{64, 64, 64, 64}) {
				t.Fatalf("local GPU change: %+v", got)
			}
			source, err := jpegcodec.ParseRestartJPEG(frame.data)
			if err != nil {
				t.Fatal(err)
			}
			want, err := source.Crop(image.Rect(64, 64, 128, 128))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got[0].data, want) {
				t.Fatal("MCU bytes were re-encoded")
			}
			// Mutating a released/reused capture byte slice must not alter the baseline.
			frame.data[0] = 0
			frame.data = restartWireFixture(t, "changed")
			c.sendFrame(frame)
			if conn.wire.Len() != 0 {
				t.Fatal("baseline aliases capture storage")
			}
			frame.data = restartWireFixture(t, "later")
			c.sendFrame(frame)
			got = readJPEGWire(t, conn, enc)
			if len(got) != 1 || got[0].r != (rectangle{192, 128, 1, 3}) {
				t.Fatalf("edge GPU change: %+v", got)
			}
			c.needFull = true
			c.sendFrame(frame)
			if got := readJPEGWire(t, conn, enc); len(got) != 1 || got[0].r != (rectangle{0, 0, 193, 131}) {
				t.Fatal("full refresh suppressed")
			}
			// A table change changes how every cached MCU is interpreted.
			frame.data = bytes.Clone(frame.data)
			dqt := bytes.Index(frame.data, []byte{0xff, 0xdb})
			frame.data[dqt+5]++
			c.sendFrame(frame)
			if got := readJPEGWire(t, conn, enc); len(got) != 1 || got[0].r != (rectangle{0, 0, 193, 131}) {
				t.Fatal("quantization change did not invalidate the compressed baseline")
			}
			smaller, err := jpegcodec.ParseRestartJPEG(frame.data)
			if err != nil {
				t.Fatal(err)
			}
			frame.data, err = smaller.Crop(image.Rect(0, 0, 128, 128))
			if err != nil {
				t.Fatal(err)
			}
			frame.width, frame.height = 128, 128
			c.sendFrame(frame)
			if got := readJPEGWire(t, conn, enc); len(got) != 1 || got[0].r != (rectangle{0, 0, 128, 128}) {
				t.Fatal("resize did not invalidate the compressed baseline")
			}
		})
	}
}

func TestCompressedJPEGSkippedFrameAndFailedWrite(t *testing.T) {
	c, conn := newJPEGWireClient(encTight, 193, 131)
	frame := &frameData{format: capture.FormatJPEG, width: 193, height: 131, data: restartWireFixture(t, "base")}
	c.sendFrame(frame)
	readJPEGWire(t, conn, encTight)
	// Coalescing can skip the intermediate changed frame. Both changes must
	// still be detected against the viewer's last successfully sent picture.
	frame.data = restartWireFixture(t, "later")
	c.sendFrame(frame)
	got := readJPEGWire(t, conn, encTight)
	if len(got) != 2 || got[0].r != (rectangle{64, 64, 64, 64}) || got[1].r != (rectangle{192, 128, 1, 3}) {
		t.Fatalf("skipped-frame changes lost: %+v", got)
	}
	before := c.jpegPrevious
	frame.data = restartWireFixture(t, "base")
	conn.fail = true
	c.sendFrame(frame)
	if c.jpegPrevious != before || !c.closing {
		t.Fatal("failed write advanced compressed baseline")
	}
}

func TestExternalMJPEGLosslessDirtyDelivery(t *testing.T) {
	const w, h = 129, 131
	pixels := make([]byte, w*h*4)
	data, err := jpegcodec.EncodeBGRA(pixels, w, h, 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := jpegcodec.LosslessJPEGTiles(data, 64); err != nil {
		if os.Getenv("ANYKVM_TEST_TURBO_LOSSLESS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("optional TurboJPEG runtime: %v", err)
	}
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			c, conn := newJPEGWireClient(enc, w, h)
			frame := &frameData{format: capture.FormatJPEG, width: w, height: h, data: data}
			c.sendFrame(frame)
			got := readJPEGWire(t, conn, enc)
			if len(got) != 9 {
				t.Fatalf("initial external JPEG: %d tiles", len(got))
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 {
				t.Fatal("unchanged MJPEG transmitted")
			}
			changed := append([]byte(nil), pixels...)
			for y := 64; y < 128; y++ {
				for x := 64; x < 128; x++ {
					i := (y*w + x) * 4
					changed[i], changed[i+1], changed[i+2] = 32, 96, 192
				}
			}
			frame.data, err = jpegcodec.EncodeBGRA(changed, w, h, 80)
			if err != nil {
				t.Fatal(err)
			}
			c.sendFrame(frame)
			got = readJPEGWire(t, conn, enc)
			if len(got) != 1 || got[0].r != (rectangle{64, 64, 64, 64}) {
				t.Fatalf("external JPEG dirty region: %+v", got)
			}
			original, err := jpeg.Decode(bytes.NewReader(frame.data))
			if err != nil {
				t.Fatal(err)
			}
			var crop image.Image
			if got[0].fill {
				rgb := got[0].rgb
				crop = image.NewUniform(color.RGBA{R: rgb[0], G: rgb[1], B: rgb[2], A: 255})
			} else {
				crop, err = jpeg.Decode(bytes.NewReader(got[0].data))
				if err != nil {
					t.Fatal(err)
				}
			}
			for y := 0; y < 64; y++ {
				for x := 0; x < 64; x++ {
					ar, ag, ab, _ := original.At(x+64, y+64).RGBA()
					br, bg, bb, _ := crop.At(x, y).RGBA()
					if ar != br || ag != bg || ab != bb {
						t.Fatal("MJPEG crop introduced pixel/quantization loss")
					}
				}
			}
		})
	}
}

func TestUnsupportedMJPEGRetainsPassthrough(t *testing.T) {
	// Invalid/unsupported input must never be silently recompressed. Existing
	// passthrough behavior is retained when the optional transform rejects it.
	c, conn := newJPEGWireClient(encJPEG, 2, 2)
	frame := &frameData{format: capture.FormatJPEG, width: 2, height: 2, data: []byte{0xff, 0xd8, 0, 0xff, 0xd9}}
	c.sendFrame(frame)
	wire := conn.wire.Bytes()
	if len(wire) != 16+len(frame.data) || !bytes.Equal(wire[16:], frame.data) {
		t.Fatal("unsupported JPEG changed during passthrough")
	}
}

func TestCompressedGPURegionSnapshots(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			c, conn := newJPEGWireClient(enc, 193, 131)
			snapshot := func(name string) *frameData {
				source, err := jpegcodec.ParseRestartJPEG(restartWireFixture(t, name))
				if err != nil {
					t.Fatal(err)
				}
				return &frameData{jpegRegions: source, format: capture.FormatJPEG, width: 193, height: 131}
			}
			first := snapshot("base")
			// Delivery carries no assembled JPEG bytes or capture buffer.
			retained := first.retain()
			defer retained.release()
			if retained.jpegRegions != first.jpegRegions || len(retained.data) != 0 {
				t.Fatal("lost snapshot ownership")
			}
			c.sendFrame(retained)
			got := readJPEGWire(t, conn, enc)
			if len(got) != 1 || !bytes.Equal(got[0].data, restartWireFixture(t, "base")) {
				t.Fatal("initial snapshot refresh failed")
			}
			c.sendFrame(first)
			if conn.wire.Len() != 0 || !c.pendingReq {
				t.Fatal("static snapshot transmitted or request lost")
			}
			// Skip an intermediate GPU result. The snapshot contains both changes.
			latest := snapshot("later")
			c.sendFrame(latest)
			got = readJPEGWire(t, conn, enc)
			if len(got) != 2 || got[0].r != (rectangle{64, 64, 64, 64}) || got[1].r != (rectangle{192, 128, 1, 3}) {
				t.Fatalf("coalesced changes lost: %+v", got)
			}
			for _, r := range got {
				want, err := latest.jpegRegions.Crop(image.Rect(r.r.x, r.r.y, r.r.x+r.r.w, r.r.y+r.r.h))
				if err != nil || !bytes.Equal(want, r.data) {
					t.Fatal("snapshot MCU payload re-encoded", err)
				}
			}
			c.needFull = true
			c.sendFrame(latest)
			if got := readJPEGWire(t, conn, enc); len(got) != 1 || !bytes.Equal(got[0].data, restartWireFixture(t, "later")) {
				t.Fatal("full snapshot refresh failed")
			}
			fresh, freshConn := newJPEGWireClient(enc, 193, 131)
			fresh.sendFrame(latest)
			if got := readJPEGWire(t, freshConn, enc); len(got) != 1 {
				t.Fatal("new client missing cached tiles")
			}
			before := c.jpegPrevious
			conn.fail = true
			c.sendFrame(first)
			if c.jpegPrevious != before || !c.closing {
				t.Fatal("failed snapshot write advanced baseline")
			}
		})
	}
}
