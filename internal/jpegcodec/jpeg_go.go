//go:build !cgo || (!darwin && !linux && !windows)

package jpegcodec

func Backend() string { return "Go JPEG" }

func encodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	return encodeGo(pixels, width, height, quality)
}

func decodeBGRA(data []byte, width, height int) ([]byte, error) {
	return decodeGo(data, width, height)
}
