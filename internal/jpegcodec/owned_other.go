//go:build !cgo || (!darwin && !linux && !windows) || ((linux || windows || anykvm_jpeg_simd) && !amd64 && !386 && !arm64)

package jpegcodec

import "anykvm/internal/framebuf"

func encodeBGRAOwned(p []byte, w, h, q int) (*framebuf.Buffer, error) {
	data, err := encodeBGRA(p, w, h, q)
	if err != nil {
		return nil, err
	}
	return framebuf.New(data, nil), nil
}
