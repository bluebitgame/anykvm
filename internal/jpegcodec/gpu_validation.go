package jpegcodec

import (
	"bytes"
	"fmt"
	"image/jpeg"
)

// Validate the first standalone GPU frame, including entropy, before trusting
// the encoder. A failure lets capture disable it and retry through CPU JPEG.
func validateGPUJPEG(data []byte, width, height int) error {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("GPU JPEG validation failed (%d bytes): %w", len(data), err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		return fmt.Errorf("GPU JPEG validation: got %dx%d, expected %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), width, height)
	}
	return nil
}
