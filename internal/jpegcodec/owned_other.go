//go:build !cgo || (!darwin && !linux && !windows)

package jpegcodec

import "anykvm/internal/framebuf"

func encodeBGRAOwned(p []byte, w, h, q int) (*framebuf.Buffer, error) {
	data, err := encodeBGRA(p, w, h, q)
	if err != nil {
		return nil, err
	}
	return framebuf.New(data, nil), nil
}
