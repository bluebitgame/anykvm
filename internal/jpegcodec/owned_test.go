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

func BenchmarkJPEGOutputOwnership(b *testing.B) {
	pixels := make([]byte, 1920*1080*4)
	for i := range pixels {
		pixels[i] = byte(i * 7)
	}
	for _, owned := range []bool{false, true} {
		b.Run(map[bool]string{false: "GoBytes", true: "owned"}[owned], func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if owned {
					data, err := EncodeBGRAOwned(pixels, 1920, 1080, 80)
					if err != nil {
						b.Fatal(err)
					}
					data.Release()
				} else {
					if _, err := EncodeBGRA(pixels, 1920, 1080, 80); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
