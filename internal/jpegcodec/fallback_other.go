//go:build !darwin || !cgo

package jpegcodec

import "anykvm/internal/framebuf"

func fallbackBackend() string { return "Go JPEG" }

func encodeFallbackOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	return encodeGoOwned(pixels, width, height, quality)
}

func decodeFallback(data []byte, width, height int) ([]byte, error) {
	return decodeGo(data, width, height)
}
