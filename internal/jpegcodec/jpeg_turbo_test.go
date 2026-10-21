//go:build cgo && !ffmpeg && (linux || windows || darwin)

package jpegcodec

import (
	"bytes"
	"os"
	"testing"
)

func TestTurboJPEGCMYKFallback(t *testing.T) {
	if !loadTurbo() {
		t.Skip("TurboJPEG runtime unavailable")
	}
	// A 3x2 CMYK fixture generated from solid color samples. TurboJPEG's BGRA
	// output does not support this colorspace; use the platform fallback.
	data, err := os.ReadFile("testdata/cmyk.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeFallback(data, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, width, height, err := DecodeBGRA(data)
	if err != nil {
		t.Fatal(err)
	}
	if width != 3 || height != 2 || !bytes.Equal(got, want) {
		t.Fatalf("CMYK fallback returned unexpected pixels or dimensions: %dx%d", width, height)
	}
}
