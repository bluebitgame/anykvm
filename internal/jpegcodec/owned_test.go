package jpegcodec

import (
	"bytes"
	"image/jpeg"
	"testing"
)

func TestOwnedJPEGSurvivesOtherEncodes(t *testing.T) {
	pixels := make([]byte, 65*49*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = 12, 80, 210, 255
	}
	first, err := EncodeBGRAOwned(pixels, 65, 49, 90)
	if err != nil {
		t.Fatal(err)
	}
	held := first.Retain()
	first.Release()
	defer held.Release()
	expected := append([]byte(nil), held.Bytes()...)
	clear(pixels)
	for range 10 {
		other, err := EncodeBGRAOwned(pixels, 65, 49, 60)
		if err != nil {
			t.Fatal(err)
		}
		other.Release()
	}
	if !bytes.Equal(expected, held.Bytes()) {
		t.Fatal("held JPEG storage overwritten")
	}
	img, err := jpeg.Decode(bytes.NewReader(held.Bytes()))
	if err != nil || img.Bounds().Dx() != 65 || img.Bounds().Dy() != 49 {
		t.Fatalf("held JPEG decode: %v", err)
	}
}
