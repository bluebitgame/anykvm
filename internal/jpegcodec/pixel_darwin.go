//go:build darwin && cgo

package jpegcodec

import "unsafe"

// EncodePixelBuffer synchronously borrows a retained CVPixelBuffer.
// The caller retains ownership of pixel; the returned JPEG owns its storage.
func EncodePixelBuffer(pixel unsafe.Pointer, quality int) ([]byte, error) {
	b, err := EncodePixelBufferOwned(pixel, quality)
	if err != nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}
