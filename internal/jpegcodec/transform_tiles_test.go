package jpegcodec

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"testing"
)

func TestTurboJPEGLosslessTiles(t *testing.T) {
	const w, h = 129, 131
	data, err := encodeGo(colorFrame(w, h), w, h, 88)
	if err != nil {
		t.Fatal(err)
	}
	tiles, tw, th, err := LosslessJPEGTiles(data, 64)
	if err != nil {
		if os.Getenv("ANYKVM_TEST_TURBO_LOSSLESS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("optional TurboJPEG transform: %v", err)
	}
	if tw != w || th != h || len(tiles) != 9 {
		t.Fatalf("bad grid %dx%d: %d", tw, th, len(tiles))
	}
	original, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for i, tile := range tiles {
		x, y := (i%3)*64, (i/3)*64
		want := image.Rect(x, y, min(w, x+64), min(h, y+64))
		if tile.Rect != want {
			t.Fatalf("tile %d geometry: %v", i, tile.Rect)
		}
		img, err := jpeg.Decode(bytes.NewReader(tile.Data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != want.Dx() || img.Bounds().Dy() != want.Dy() {
			t.Fatal("crop size differs")
		}
		for y := 0; y < want.Dy(); y++ {
			for x := 0; x < want.Dx(); x++ {
				a, b := img.At(x, y), original.At(want.Min.X+x, want.Min.Y+y)
				ar, ag, ab, _ := a.RGBA()
				br, bg, bb, _ := b.RGBA()
				if ar != br || ag != bg || ab != bb {
					t.Fatalf("lossless pixels differ in tile %d", i)
				}
			}
		}
	}
	again, _, _, err := LosslessJPEGTiles(data, 64)
	if err != nil {
		t.Fatal(err)
	}
	for i := range tiles {
		if !bytes.Equal(tiles[i].Data, again[i].Data) {
			t.Fatal("crop output is not deterministic")
		}
	}
}
