package vnc

import "anykvm/internal/jpegcodec"

// encodeJPEG encodes native BGRA pixel data to a JPEG image at the given
// quality (1-100). It is used to honour the client's negotiated JPEG Quality
// Level for the Tight(JPEG) and standalone JPEG encodings.
func encodeJPEG(bgra []byte, width, height, quality int) ([]byte, error) {
	return jpegcodec.EncodeBGRA(bgra, width, height, quality)
}
