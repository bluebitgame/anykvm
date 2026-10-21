package vnc

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
)

// Reuse the independent GPU fixture's standard tables; substitute exact Annex K
// DC-zero/EOB symbols for gray MCUs. Textured MCUs retain their original bytes.
func tightMixedRestartFixture(t *testing.T) []byte {
	t.Helper()
	source, err := jpegcodec.ParseRestartJPEG(restartWireFixture(t, "base"))
	if err != nil {
		t.Fatal(err)
	}
	tiny, err := source.Crop(image.Rect(0, 0, 16, 16))
	if err != nil {
		t.Fatal(err)
	}
	scan := bytes.Index(tiny, []byte{255, 0xda})
	headerEnd := scan + 2 + int(binary.BigEndian.Uint16(tiny[scan+2:]))
	header := bytes.Clone(tiny[:headerEnd])
	sof := bytes.Index(header, []byte{255, 0xc0})
	binary.BigEndian.PutUint16(header[sof+5:], 64)
	binary.BigEndian.PutUint16(header[sof+7:], 128)
	data := header
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			i := y*8 + x
			if i > 0 {
				data = append(data, 255, byte(0xd0+(i-1)%8))
			}
			if x < 4 {
				// Four Y DC0/EOB pairs (001010), two chroma pairs (0000).
				data = append(data, 0x28, 0xa2, 0x8a, 0x00)
			} else {
				crop, err := source.Crop(image.Rect(x*16, y*16, (x+1)*16, (y+1)*16))
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, crop[headerEnd:len(crop)-2]...)
			}
		}
	}
	return append(data, 255, 0xd9)
}

func TestTightCompressedMixedFillAndJPEG(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		data := tightMixedRestartFixture(t)
		source, err := jpegcodec.ParseRestartJPEG(data)
		if err != nil {
			t.Fatal(err)
		}
		c, conn := newJPEGWireClient(encTight, 128, 64)
		frame := &frameData{format: capture.FormatJPEG, width: 128, height: 64, data: data}
		if snapshot {
			frame.jpegRegions, frame.data = source, nil // No full packed JPEG available.
		}
		c.sendFrame(frame)
		got := readJPEGWire(t, conn, encTight)
		if len(got) != 2 || got[0].r != (rectangle{0, 0, 64, 64}) || !got[0].fill || got[0].rgb != [3]byte{128, 128, 128} ||
			got[1].r != (rectangle{64, 0, 64, 64}) || got[1].fill {
			t.Fatalf("compressed mixed update snapshot=%v: %+v", snapshot, got)
		}
		want, err := source.Crop(image.Rect(64, 0, 128, 64))
		if err != nil || !bytes.Equal(want, got[1].data) {
			t.Fatal("textured JPEG was re-encoded")
		}
		if c.prevFrame != nil || c.jpegPrevious == nil {
			t.Fatal("compressed Fill created a pixel baseline or lost compressed state")
		}
		c.sendFrame(frame)
		if conn.wire.Len() != 0 || !c.pendingReq {
			t.Fatal("unchanged compressed Fill/JPEG picture was resent")
		}
		c.needFull = true
		c.sendFrame(frame)
		if got := readJPEGWire(t, conn, encTight); len(got) != 2 || !got[0].fill {
			t.Fatal("compressed full refresh lost Fill")
		}
		// Failed writes must not replace the viewer's committed baseline.
		previous := c.jpegPrevious
		c.needFull, conn.fail = true, true
		c.sendFrame(frame)
		if c.jpegPrevious != previous || !c.closing {
			t.Fatal("failed compressed Fill update advanced the baseline")
		}
	}
}

func TestTightExternalCompressedFill(t *testing.T) {
	// Auto quality retains external MJPEG quantization. Fill must match the
	// JPEG's reconstructed color, even if the encoder changed the source RGB.
	const w, h = 65, 67
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Pix = bytes.Repeat([]byte{179, 97, 43, 255}, w*h)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 25}); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	wantPixels, _, _, err := jpegcodec.DecodeBGRA(data)
	if err != nil {
		t.Fatal(err)
	}
	c, conn := newJPEGWireClient(encTight, w, h)
	frame := &frameData{format: capture.FormatJPEG, width: w, height: h, data: data}
	c.sendFrame(frame)
	got := readJPEGWire(t, conn, encTight)
	area := 0
	for _, r := range got {
		if !r.fill || r.rgb != [3]byte{wantPixels[2], wantPixels[1], wantPixels[0]} {
			t.Fatalf("external JPEG color not preserved by Fill: %+v want BGR=%v", r, wantPixels[:3])
		}
		area += r.r.w * r.r.h
	}
	if area != w*h {
		t.Fatal("incomplete external MJPEG Fill coverage")
	}
	c.sendFrame(frame)
	if conn.wire.Len() != 0 {
		t.Fatal("static external MJPEG Fill was resent")
	}
}
