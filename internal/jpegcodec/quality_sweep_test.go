package jpegcodec

import (
	"bytes"
	"image/jpeg"
	"math/rand"
	"testing"
)

// Exercise quality changes on reused encoders, including returning to a low
// quality after a high one. Decode independently so a matching encoder/decoder
// bug cannot conceal a quality setting that only changes the JPEG header.
func TestEncoderQualitySweep(t *testing.T) {
	const w, h = 320, 192
	pixels := make([]byte, w*h*4)
	rng := rand.New(rand.NewSource(17))
	for i := 0; i < len(pixels); i += 4 {
		v := byte(rng.Intn(256))
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = v, v, v, 255
	}
	for name, encode := range map[string]func([]byte, int, int, int) ([]byte, error){
		"copied": EncodeBGRA,
		"owned": func(p []byte, w, h, q int) ([]byte, error) {
			buffer, err := EncodeBGRAOwned(p, w, h, q)
			if err != nil {
				return nil, err
			}
			defer buffer.Release()
			return bytes.Clone(buffer.Bytes()), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			sizes := make(map[int]int)
			errors := make(map[int]float64)
			for _, q := range []int{95, 1, 19, 50, 75, 100, 1} {
				data, err := encode(pixels, w, h, q)
				if err != nil {
					t.Fatal(err)
				}
				if previous, ok := sizes[q]; ok && previous != len(data) {
					t.Fatalf("quality %d changed size after encoder reuse: %d -> %d", q, previous, len(data))
				}
				img, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				var mse float64
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						r, _, _, _ := img.At(x, y).RGBA()
						d := float64(r>>8) - float64(pixels[(y*w+x)*4])
						mse += d * d
					}
				}
				mse /= w * h
				sizes[q], errors[q] = len(data), mse
				t.Logf("backend=%s quality=%d bytes=%d grayscale MSE=%.2f", Backend(), q, len(data), mse)
			}
			for _, pair := range [][2]int{{1, 19}, {19, 50}, {50, 75}, {75, 95}, {95, 100}} {
				low, high := pair[0], pair[1]
				if sizes[low] >= sizes[high] || errors[low] <= errors[high] {
					t.Fatalf("quality %d -> %d did not increase size and preserve detail: sizes=%v MSE=%v", low, high, sizes, errors)
				}
			}
		})
	}
}
