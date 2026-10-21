package vnc

import (
	"bytes"
	"math/rand"
	"testing"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
)

func TestJPEGQualityLockOverridesCompressedSource(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		t.Run(encodingName(enc), func(t *testing.T) {
			const w, h = 128, 64
			c, conn := newJPEGWireClient(enc, w, h)
			c.jpegLock = 1
			if got := c.jpegQualityValue(); got != 1 {
				t.Fatalf("client quality overrode server lock: %d", got)
			}
			pixels := make([]byte, w*h*4)
			rand.New(rand.NewSource(42)).Read(pixels)
			source, err := encodeJPEG(pixels, w, h, 95)
			if err != nil {
				t.Fatal(err)
			}
			frame := &frameData{format: capture.FormatJPEG, width: w, height: h, data: source}
			c.sendFrame(frame)
			got := readJPEGWire(t, conn, enc)
			if len(got) != 1 || bytes.Equal(got[0].data, source) || len(got[0].data) >= len(source) {
				t.Fatal("quality lock did not re-encode compressed source at lower quality")
			}
			decoded, _, _, err := jpegcodec.DecodeBGRA(source)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := encodeJPEG(decoded, w, h, 1)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got[0].data, expected) {
				t.Fatal("wire JPEG does not use the server's locked quality")
			}
			if frame.format != capture.FormatJPEG || !bytes.Equal(frame.data, source) {
				t.Fatal("re-encoding modified shared capture input")
			}
			c.sendFrame(frame)
			if conn.wire.Len() != 0 {
				t.Fatal("unchanged compressed input bypassed dirty detection")
			}
		})
	}
}

func TestJPEGQualityLockRequestsPixels(t *testing.T) {
	for _, enc := range []int32{encJPEG, encTight} {
		for _, quality := range []int{0, 1, 19, 95} {
			c, _ := newJPEGWireClient(enc, 64, 64)
			c.jpegLock = quality
			c.server = NewServer(Config{})
			c.refreshPixelNeed()
			if c.pixelNeed != (quality > 0) {
				t.Fatalf("encoding %d quality %d: pixel need=%v", enc, quality, c.pixelNeed)
			}
		}
	}
}
