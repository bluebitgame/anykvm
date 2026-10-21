package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"
)

func restartTileFixture(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	count := ((w + 15) / 16) * ((h + 15) / 16)
	coeff := make([]int32, count*384)
	for m := 0; m < count; m++ {
		for b := 0; b < 6; b++ {
			coeff[m*384+b*64] = int32((m*17+b*3)%511 - 255)
			coeff[m*384+b*64+1] = int32(m%7 - 3)
		}
	}
	data, err := encodeQuantized420Restart(coeff, w, h, quality, true)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestartJPEGLosslessCrop(t *testing.T) {
	const w, h = 193, 131
	data := restartTileFixture(t, w, h, 80)
	src, err := ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := src.Crop(image.Rect(0, 0, w, h))
	if err != nil || !bytes.Equal(whole, data) {
		t.Fatal("full crop changed source bytes")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []image.Rectangle{image.Rect(64, 64, 128, 128), image.Rect(192, 128, 193, 131), image.Rect(16, 32, 176, 112)} {
		crop, err := src.Crop(r)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseRestartJPEG(crop)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Width != r.Dx() || parsed.Height != r.Dy() {
			t.Fatal("wrong crop dimensions")
		}
		// Verify payload identity and RST renumbering, not just decoded pixels.
		i := 0
		for y := r.Min.Y / 16; y < (r.Max.Y+15)/16; y++ {
			for x := r.Min.X / 16; x < (r.Max.X+15)/16; x++ {
				if !bytes.Equal(parsed.segments[i], src.segments[y*((w+15)/16)+x]) {
					t.Fatal("MCU entropy was changed")
				}
				i++
			}
		}
		img, err := jpeg.Decode(bytes.NewReader(crop))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < r.Dy(); y++ {
			for x := 0; x < r.Dx(); x++ {
				a, b := img.At(x, y), decoded.At(r.Min.X+x, r.Min.Y+y)
				ar, ag, ab, _ := a.RGBA()
				br, bg, bb, _ := b.RGBA()
				if ar != br || ag != bg || ab != bb {
					t.Fatalf("crop pixels differ at %d,%d", x, y)
				}
			}
		}
	}
	if _, err := src.Crop(image.Rect(1, 0, 64, 64)); err == nil {
		t.Fatal("unaligned crop accepted")
	}
	if _, err := src.Crop(image.Rect(0, 0, 194, 131)); err == nil {
		t.Fatal("out-of-bounds crop accepted")
	}
}

func TestRestartJPEGComparisonAndValidation(t *testing.T) {
	data := restartTileFixture(t, 129, 131, 80)
	old, _ := ParseRestartJPEG(data)
	current, _ := ParseRestartJPEG(bytes.Clone(data))
	if !current.EqualRect(old, image.Rect(64, 64, 128, 128)) {
		t.Fatal("unchanged tile marked dirty")
	}
	// Change only one valid MCU's DC amplitude by reusing another MCU payload.
	current.segments = append([][]byte(nil), current.segments...)
	current.segments[4*9+4] = current.segments[0]
	if current.EqualRect(old, image.Rect(64, 64, 128, 128)) || !current.EqualRect(old, image.Rect(0, 0, 64, 64)) {
		t.Fatal("tile comparison is not spatially isolated")
	}
	quality, _ := ParseRestartJPEG(restartTileFixture(t, 129, 131, 81))
	if current.SameParameters(quality) {
		t.Fatal("quantization change was ignored")
	}
	bad := bytes.Clone(data)
	dri := bytes.Index(bad, []byte{0xff, 0xdd, 0, 4, 0, 1})
	binary.BigEndian.PutUint16(bad[dri+4:], 2)
	if _, err := ParseRestartJPEG(bad); err == nil {
		t.Fatal("non-independent MCU stream accepted")
	}
	for i := 0; i < len(data); i++ {
		if _, err := ParseRestartJPEG(data[:i]); err == nil {
			t.Fatalf("truncated JPEG accepted at %d", i)
		}
	}
	bad = bytes.Clone(data)
	rst := bytes.Index(bad, []byte{0xff, 0xd0})
	bad[rst+1] = 0xd3
	if _, err := ParseRestartJPEG(bad); err == nil {
		t.Fatal("invalid restart numbering accepted")
	}
}

func FuzzParseRestartJPEG(f *testing.F) {
	f.Add([]byte{0xff, 0xd8, 0xff, 0xd9})
	f.Fuzz(func(t *testing.T, data []byte) {
		src, err := ParseRestartJPEG(data)
		if err == nil {
			_, _ = src.Crop(image.Rect(0, 0, src.Width, src.Height))
		}
	})
}
