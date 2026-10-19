package videoenc

import "anykvm/internal/framebuf"

// EncodePixels preserves an input's native pixel layout for library encoders.
// Native VideoToolbox also accepts BGRA/YUV and converts in the native API.
func EncodePixels(e Encoder, o Options, data []byte, w, h int, pixfmt string, idr bool) ([]byte, error) {
	if encoder, ok := e.(interface {
		EncodePixels([]byte, int, int, string, bool) ([]byte, error)
	}); ok {
		return encoder.EncodePixels(data, w, h, pixfmt, idr)
	}
	nv12, err := pixelsNV12(data, w, h, pixfmt, o.Width, o.Height)
	if err != nil {
		return nil, err
	}
	return e.Encode(nv12, idr)
}

// EncodePixelsOwned transfers immutable compressed output to its caller.
func EncodePixelsOwned(e Encoder, o Options, data []byte, w, h int, pixfmt string, idr bool) (*framebuf.Buffer, error) {
	if encoder, ok := e.(interface {
		EncodePixelsOwned([]byte, int, int, string, bool) (*framebuf.Buffer, error)
	}); ok {
		return encoder.EncodePixelsOwned(data, w, h, pixfmt, idr)
	}
	bytes, err := EncodePixels(e, o, data, w, h, pixfmt, idr)
	if err != nil || len(bytes) == 0 {
		return nil, err
	}
	return framebuf.New(bytes, nil), nil
}
