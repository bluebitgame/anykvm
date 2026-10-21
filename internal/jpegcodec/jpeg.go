// Package jpegcodec converts tightly packed BGRA frames to and from JPEG.
// anyjpeg uses AVX2/SSE2 on x86 and NEON on ARM64. macOS ARM64 defaults to
// ImageIO. Go JPEG handles unsupported CPUs, non-CGO builds and unusual scans.
// Windows/Linux also provide compute JPEG encoding and baseline JPEG decoding
// to BT.709 NV12.
package jpegcodec

import (
	"anykvm/internal/framebuf"
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
)

// EncodeBGRA ignores the unused alpha byte, as capture/VNC frames are opaque.
// The caller retains the input; the returned JPEG owns its storage.
func EncodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	if width < 1 || height < 1 || width > 65535 || height > 65535 || int64(width)*int64(height)*4 != int64(len(pixels)) {
		return nil, fmt.Errorf("JPEG: invalid BGRA frame %dx%d (%d bytes)", width, height, len(pixels))
	}
	return encodeBGRA(pixels, width, height, min(100, max(1, quality)))
}

// DecodeBGRA returns opaque BGRA pixels in top-to-bottom row order.
func DecodeBGRA(data []byte) (pixels []byte, width, height int, err error) {
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	// Bound allocation for compressed inputs received from capture/network peers.
	if config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 {
		return nil, 0, 0, fmt.Errorf("JPEG: invalid decoded dimensions %dx%d", config.Width, config.Height)
	}
	pixels, err = decodeBGRA(data, config.Width, config.Height)
	if err != nil {
		return nil, 0, 0, err
	}
	return pixels, config.Width, config.Height, nil
}

func encodeGo(pixels []byte, width, height, quality int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < len(pixels); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pixels[i+2], pixels[i+1], pixels[i], 255
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodeGo(data []byte, width, height int) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	rgba := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	for i := 0; i < len(rgba.Pix); i += 4 {
		rgba.Pix[i], rgba.Pix[i+2] = rgba.Pix[i+2], rgba.Pix[i]
	}
	return rgba.Pix, nil
}

// EncodeBGRAOwned returns immutable JPEG storage with an explicit lifetime.
func EncodeBGRAOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	if width < 1 || height < 1 || width > 65535 || height > 65535 || int64(width)*int64(height)*4 != int64(len(pixels)) {
		return nil, fmt.Errorf("JPEG: invalid BGRA frame %dx%d (%d bytes)", width, height, len(pixels))
	}
	return encodeBGRAOwned(pixels, width, height, min(100, max(1, quality)))
}
